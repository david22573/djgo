package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type playlistEntry struct {
	Filename string `json:"filename"`
	Title    string `json:"title"`
}

type daemon struct {
	sock   string
	ctl    string
	mu     sync.Mutex // one command at a time => single writer to mpv
	ipc    *ipcClient
	titles *titleCache
}

func (d *daemon) connect() error {
	if d.ipc != nil {
		select {
		case <-d.ipc.closed:
			d.ipc = nil
		default:
			return nil
		}
	}
	if !socketAlive(d.sock) {
		d.ipc = nil
		return fmt.Errorf("mpv not running (no socket %s) — dj start [playlist.m3u]", d.sock)
	}
	c, err := dialIPC(d.sock)
	if err != nil {
		d.ipc = nil
		return fmt.Errorf("mpv not running (no socket %s) — dj start [playlist.m3u]", d.sock)
	}
	d.ipc = c
	// keep the queue model fresh; ids are fixed by design (no generic framework).
	// One deadline for all four: a stuck mpv must not pin connect() forever.
	octx, ocancel := ctxTimeout(2 * time.Second)
	for i, name := range []string{"media-title", "playlist-pos", "pause", "playlist-count"} {
		_ = c.Observe(octx, i+1, name)
	}
	ocancel()
	// first connection of this daemon: fill in titles mpv never resolved
	go d.prefetchTitles(c)
	return nil
}

func (d *daemon) client() (*ipcClient, error) {
	if err := d.connect(); err != nil {
		return nil, err
	}
	return d.ipc, nil
}

func ctxTimeout(dur time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dur)
}

func (d *daemon) playlist(c *ipcClient) ([]playlistEntry, error) {
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	raw, err := c.Prop(ctx, "playlist")
	if err != nil {
		return nil, err
	}
	var pl []playlistEntry
	err = json.Unmarshal(raw, &pl)
	return pl, err
}

func (d *daemon) pos(c *ipcClient) (int, error) {
	ctx, cancel := ctxTimeout(3 * time.Second)
	defer cancel()
	return c.PropInt(ctx, "playlist-pos")
}

func (d *daemon) paused(c *ipcClient) (bool, error) {
	ctx, cancel := ctxTimeout(3 * time.Second)
	defer cancel()
	b, err := c.PropBool(ctx, "pause")
	if err != nil {
		return false, err
	}
	return b, nil
}

// titleOf resolves the best display title for an entry: mpv's own (when it
// loaded the track), then the yt-dlp cache, then nothing.
func (d *daemon) titleOf(e playlistEntry) string {
	if e.Title != "" && e.Title != "unknown" && !strings.HasPrefix(e.Title, "watch?v=") {
		return e.Title
	}
	if d.titles != nil {
		if t := d.titles.get(e.Filename); t != "" {
			return t
		}
	}
	return ""
}

// isYouTubeURL reports whether an entry is something yt-dlp can name.
func isYouTubeURL(name string) bool {
	return strings.Contains(name, "youtube.com/") || strings.Contains(name, "youtu.be/")
}

// prefetchTitles fills the cache for entries mpv has not resolved yet.
// Bounded: one burst of at most prefetchBurst goroutines per call, and only
// YouTube entries (local files would just burn a yt-dlp process each).
func (d *daemon) prefetchTitles(c *ipcClient) {
	if d.titles == nil {
		return
	}
	pl, err := d.playlist(c)
	if err != nil {
		return
	}
	const prefetchBurst = 20
	started := 0
	for _, e := range pl {
		if started >= prefetchBurst {
			return
		}
		if !isYouTubeURL(e.Filename) || d.titleOf(e) != "" {
			continue
		}
		started++
		go d.titles.resolve(e.Filename)
	}
}

func mmss(secs float64) string {
	if secs < 0 {
		secs = 0
	}
	s := int(secs)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---------- verbs ----------

// resolveOutsideMu does a verb's subprocess work (yt-dlp) before the daemon
// mutex is taken, so a slow lookup never blocks `dj now`. It returns the
// possibly rewritten request plus a finished reply when the verb was answered
// here. Everything else falls through to dispatch under d.mu.
func (d *daemon) resolveOutsideMu(req ctlRequest) (ctlRequest, *ctlReply) {
	switch req.Cmd {
	case "add":
		if len(req.Args) == 0 || strings.HasPrefix(req.Args[0], "http") {
			return req, nil // raw urls are appended as-is, resolved in the background
		}
		url, err := d.prepareAdd(strings.Join(req.Args, " "))
		if err != nil {
			return req, &ctlReply{ID: req.ID, Err: err.Error()}
		}
		req.Args = []string{url}
		return req, nil
	case "search":
		if len(req.Args) == 0 {
			return req, &ctlReply{ID: req.ID, Err: "usage: dj search <words...>"}
		}
		out, err := ytSearch(strings.Join(req.Args, " "), 5)
		if err != nil {
			return req, &ctlReply{ID: req.ID, Err: err.Error()}
		}
		return req, &ctlReply{ID: req.ID, Out: out}
	}
	return req, nil
}

// prepareAdd turns a search query into a canonical watch URL and seeds the
// title cache with the name the search already paid for.
func (d *daemon) prepareAdd(query string) (string, error) {
	id, title, err := ytResolve(query)
	if err != nil {
		return "", fmt.Errorf("no match for: %s", query)
	}
	url := "https://www.youtube.com/watch?v=" + id
	if d.titles != nil && title != "" {
		d.titles.set(url, title)
	}
	return url, nil
}

func (d *daemon) handle(req ctlRequest) ctlReply {
	out, err := d.dispatch(req.Cmd, req.Args)
	rep := ctlReply{ID: req.ID, Out: out}
	if err != nil {
		rep.Err = err.Error()
	}
	return rep
}

func (d *daemon) dispatch(cmd string, args []string) (string, error) {
	switch cmd {
	case "ping":
		return "pong", nil
	case "status":
		return d.status()
	case "now":
		return d.now()
	case "list":
		return d.list()
	case "add":
		if len(args) == 0 {
			return "", errors.New("usage: dj add <url|search words...>")
		}
		return d.add(strings.Join(args, " "))
	case "search":
		// Run() serves search outside d.mu; this keeps the verb contract in one place.
		return d.search(args)
	case "vol":
		return d.volume(args)
	case "rm":
		if len(args) == 0 {
			return "", errors.New("usage: dj rm <n>   (n = number from dj list)")
		}
		return d.remove(args[0])
	case "pos":
		if len(args) == 0 {
			return "", errors.New("usage: dj pos <n>")
		}
		return d.jump(args[0])
	case "skip":
		return d.skip()
	case "pause":
		return d.setPause(true)
	case "play":
		return d.setPause(false)
	case "save":
		path := ""
		if len(args) > 0 {
			path = args[0]
		}
		return d.save(path)
	case "start":
		pl := ""
		if len(args) > 0 {
			pl = args[0]
		}
		return d.start(pl)
	case "stop":
		return d.stop()
	case "restore":
		return d.restore()
	default:
		return "", fmt.Errorf("unknown command %q", cmd)
	}
}

func (d *daemon) status() (string, error) {
	c, err := d.client()
	if err != nil {
		return "mpv not running", nil
	}
	ctx, cancel := ctxTimeout(3 * time.Second)
	defer cancel()
	pid, err := c.PropInt(ctx, "pid")
	if err != nil {
		return "mpv not running", nil
	}
	return fmt.Sprintf("mpv running (pid %d), socket ok", pid), nil
}

func (d *daemon) now() (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	// fetch pos once and reuse it on the fallback path: an extra round trip
	// here is directly visible in `dj now` latency.
	pos, posErr := c.PropInt(ctx, "playlist-pos")
	title, err := c.PropString(ctx, "media-title")
	if err != nil || title == "" || title == "unknown" || strings.HasPrefix(title, "watch?v=") {
		pl, _ := d.playlist(c)
		if posErr == nil && pos >= 0 && pos < len(pl) {
			if t := d.titleOf(pl[pos]); t != "" {
				title = t
			} else {
				title = "(unknown) " + pl[pos].Filename
			}
		}
	}
	if title == "" {
		title = "(unknown)"
	}
	count, _ := c.PropInt(ctx, "playlist-count")
	cur, err := c.PropFloat(ctx, "time-pos")
	if err != nil {
		cur = 0
	}
	dur, _ := c.PropFloat(ctx, "duration")
	paused, _ := c.PropBool(ctx, "pause")
	state := "PLAYING"
	if paused {
		state = "PAUSED"
	}
	return fmt.Sprintf("%s  [%d/%d]  %s/%s  %s", state, pos+1, count, mmss(cur), mmss(dur), title), nil
}

func (d *daemon) list() (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	pl, err := d.playlist(c)
	if err != nil {
		return "", err
	}
	pos, _ := d.pos(c)
	var b strings.Builder
	for i, e := range pl {
		prefix := " "
		if i == pos {
			prefix = "▶"
		}
		title := d.titleOf(e)
		if title == "" {
			title = "(loading) " + e.Filename
		}
		b.WriteString(truncRunes(prefix+" "+fmt.Sprintf("%d. ", i+1)+title, 90))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// search is the in-mutex fallback for the "search" verb; Run() serves it
// outside the lock via resolveOutsideMu.
func (d *daemon) search(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("usage: dj search <words...>")
	}
	return ytSearch(strings.Join(args, " "), 5)
}

// volume reports or sets mpv's volume. Args accept an absolute 0-100 or a
// signed delta (+10 / -10); the result is always clamped into 0-100.
func (d *daemon) volume(args []string) (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	cur, err := c.PropFloat(ctx, "volume")
	if err != nil {
		return "", fmt.Errorf("mpv volume unavailable: %v", err)
	}
	spec := ""
	if len(args) > 0 {
		spec = strings.TrimSpace(args[0])
	}
	if spec == "" {
		return fmt.Sprintf("volume: %d", int(math.Round(cur))), nil
	}
	next, perr := strconv.ParseFloat(spec, 64)
	if perr != nil {
		return "", errors.New("usage: dj vol [0-100|+n|-n]")
	}
	if spec[0] == '+' || spec[0] == '-' {
		next += cur
	}
	if next < 0 {
		next = 0
	}
	if next > 100 {
		next = 100
	}
	if _, errstr, err := c.Call(ctx, "set_property", "volume", next); err != nil {
		return "", err
	} else if errstr != "" && errstr != "success" {
		return "", errors.New(errstr)
	}
	return fmt.Sprintf("volume: %d", int(math.Round(next))), nil
}

func (d *daemon) add(query string) (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	url := query
	if !strings.HasPrefix(query, "http") {
		url, err = d.prepareAdd(query)
		if err != nil {
			return "", err
		}
	} else if d.titles != nil && d.titles.get(url) == "" {
		go d.titles.resolve(url) // raw url: resolve in the background
	}
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	_, errstr, err := c.Call(ctx, "loadfile", url, "append")
	if err != nil {
		return "", err
	}
	if errstr != "" && errstr != "success" {
		return "", fmt.Errorf("append failed: %s", errstr)
	}
	d.snapshot()
	// report the real title when the cache knows it (a search add always does);
	// a bare url that was never resolved falls back to the url itself.
	if d.titles != nil {
		if t := d.titles.get(url); t != "" {
			return "queued: " + t, nil
		}
	}
	return "queued: " + url, nil
}

func (d *daemon) remove(ns string) (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(ns), "%d", &n); err != nil || n < 1 {
		return "", errors.New("usage: dj rm <n>   (n = number from dj list)")
	}
	pl, err := d.playlist(c)
	if err != nil {
		return "", err
	}
	if n > len(pl) {
		return "", fmt.Errorf("no track #%d", n)
	}
	title := pl[n-1].Title
	if title == "" {
		title = pl[n-1].Filename
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if _, errstr, err := c.Call(ctx, "playlist-remove", n-1); err != nil {
		return "", err
	} else if errstr != "" && errstr != "success" {
		return "", errors.New(errstr)
	}
	d.snapshot()
	return fmt.Sprintf("removed #%d: %s", n, title), nil
}

func (d *daemon) jump(ns string) (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(ns), "%d", &n); err != nil || n < 1 {
		return "", errors.New("usage: dj pos <n>")
	}
	pl, err := d.playlist(c)
	if err != nil {
		return "", err
	}
	if n > len(pl) {
		return "", fmt.Errorf("no track #%d", n)
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if _, errstr, err := c.Call(ctx, "set_property", "playlist-pos", n-1); err != nil {
		return "", err
	} else if errstr != "" && errstr != "success" {
		return "", errors.New(errstr)
	}
	time.Sleep(120 * time.Millisecond)
	d.snapshot()
	return fmt.Sprintf("jumped to #%d", n), nil
}

func (d *daemon) skip() (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	bctx, bcancel := ctxTimeout(5 * time.Second)
	before, berr := c.PropInt(bctx, "playlist-pos")
	count, cerr := c.PropInt(bctx, "playlist-count")
	befTitle, _ := c.PropString(bctx, "media-title")
	bcancel()
	atEnd := berr == nil && cerr == nil && before >= 0 && before == count-1

	ctx, cancel := ctxTimeout(10 * time.Second)
	_, errstr, err := c.Call(ctx, "playlist-next", "weak")
	cancel()
	if err != nil || (errstr != "" && errstr != "success") {
		if atEnd { // mpv has nowhere to go: that is the end of the queue, not a failure
			go d.snapshotWith(c)
			return "skipped → (end of queue)", nil
		}
		if err != nil {
			return "", err
		}
		return "", errors.New(errstr)
	}

	// poll instead of a blind sleep: mpv switches entries in tens of ms, so
	// this returns as soon as the position moves and never waits longer than
	// ~400ms when it does not.
	moved := false
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		pctx, pcancel := ctxTimeout(time.Second)
		p, perr := c.PropInt(pctx, "playlist-pos")
		pcancel()
		if perr == nil && p >= 0 && p != before {
			moved = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !moved && atEnd {
		go d.snapshotWith(c)
		return "skipped → (end of queue)", nil
	}

	// the new entry needs a moment to demux before it has a title; poll until
	// media-title CHANGES from the one we saw before the skip — a stale title
	// is worse than a slow reply.
	title := ""
	deadline = time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		tctx, tcancel := ctxTimeout(time.Second)
		got, _ := c.PropString(tctx, "media-title")
		tcancel()
		if got != "" && got != "unknown" && !strings.HasPrefix(got, "watch?v=") && got != befTitle {
			title = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if title == "" {
		if pl, perr := d.playlist(c); perr == nil {
			if p, perr := d.pos(c); perr == nil && p >= 0 && p < len(pl) {
				title = d.titleOf(pl[p])
				if title == "" {
					title = pl[p].Filename
				}
			}
		}
	}
	// persistence is not worth making the reply wait
	go d.snapshotWith(c)
	if title == "" {
		title = "(unknown)"
	}
	return "skipped → " + title, nil
}

func (d *daemon) setPause(p bool) (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if _, errstr, err := c.Call(ctx, "set_property", "pause", p); err != nil {
		return "", err
	} else if errstr != "" && errstr != "success" {
		return "", errors.New(errstr)
	}
	d.snapshot()
	if p {
		return "paused", nil
	}
	return "playing", nil
}

func (d *daemon) save(path string) (string, error) {
	c, err := d.client()
	if err != nil {
		return "", err
	}
	pl, err := d.playlist(c)
	if err != nil {
		return "", err
	}
	if path == "" {
		path = filepath.Join(playlistDir(), "dj-"+time.Now().Format("20060102-1504")+".m3u")
	}
	tracks := make([]Track, 0, len(pl))
	for _, e := range pl {
		tracks = append(tracks, Track{URL: e.Filename, Title: d.titleOf(e)})
	}
	if err := writeM3U(path, tracks); err != nil {
		return "", err
	}
	return fmt.Sprintf("saved: %s (%d tracks)", path, len(tracks)), nil
}

func (d *daemon) start(playlist string) (string, error) {
	pid, err := startMPV(playlist)
	if err != nil {
		if errors.Is(err, errAlreadyRunning) {
			return "", errors.New("mpv already running")
		}
		return "", fmt.Errorf("mpv failed to start: %v (see %s)", err, mpvLogPath())
	}
	d.ipc = nil
	if err := d.connect(); err != nil {
		return "", err
	}
	d.snapshot()
	if d.ipc != nil {
		go d.prefetchTitles(d.ipc)
	}
	return fmt.Sprintf("mpv up (pid %d)", pid), nil
}

func (d *daemon) stop() (string, error) {
	if d.ipc != nil {
		select {
		case <-d.ipc.closed:
			d.ipc = nil
		default:
		}
	}
	if d.ipc == nil {
		// the control daemon can outlive an mpv restart: reconnect before
		// deciding there is nothing to quit.
		if c, err := d.client(); err == nil {
			d.ipc = c
		}
	}
	quit := false
	if d.ipc != nil {
		// autosave before the queue evaporates
		autosave := filepath.Join(playlistDir(), ".autosave.m3u")
		_ = d.saveQuiet(autosave)
		ctx, cancel := ctxTimeout(3 * time.Second)
		_, _, _ = d.ipc.Call(ctx, "quit")
		cancel()
		d.ipc = nil
		quit = true
	}
	if quit {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && socketAlive(d.sock) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if socketAlive(d.sock) {
		return "", errors.New("mpv still running after quit")
	}
	_ = os.Remove(d.sock) // nothing holds it, so this is a stale file
	return "mpv stopped", nil
}

func (d *daemon) saveQuiet(path string) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	pl, err := d.playlist(c)
	if err != nil {
		return err
	}
	tracks := make([]Track, 0, len(pl))
	for _, e := range pl {
		tracks = append(tracks, Track{URL: e.Filename, Title: d.titleOf(e)})
	}
	return writeM3U(path, tracks)
}

func (d *daemon) restore() (string, error) {
	snap, err := loadSnapshot(stateFile())
	fallback := ""
	if err != nil || len(snap.Tracks) == 0 {
		// stop() writes .autosave.m3u before quitting; when the json snapshot
		// is gone or empty that file is still a usable queue.
		auto := filepath.Join(playlistDir(), ".autosave.m3u")
		tracks, aerr := loadM3U(auto)
		if aerr != nil || len(tracks) == 0 {
			if err != nil {
				return "", fmt.Errorf("no snapshot at %s", stateFile())
			}
			return "", errors.New("snapshot is empty")
		}
		snap = Snapshot{Tracks: tracks}
		fallback = " (from .autosave.m3u)"
	}
	tmp := filepath.Join(playlistDir(), ".restore.m3u")
	if err := writeM3U(tmp, snap.Tracks); err != nil {
		return "", err
	}
	if !socketAlive(d.sock) {
		if _, err := d.start(tmp); err != nil {
			return "", err
		}
	} else {
		c, err := d.client()
		if err != nil {
			return "", err
		}
		ctx, cancel := ctxTimeout(20 * time.Second)
		defer cancel()
		if _, errstr, err := c.Call(ctx, "loadlist", tmp, "replace"); err != nil {
			return "", err
		} else if errstr != "" && errstr != "success" {
			return "", errors.New(errstr)
		}
	}
	c, err := d.client()
	if err != nil {
		return "", err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	_, _, _ = c.Call(ctx, "set_property", "playlist-pos", snap.Pos)
	cancel()

	// resume mid-track: wait for the entry to demux, then seek to the saved offset
	seeked := 0.0
	if snap.TimePos > 2 {
		if waitLoaded(c, 20*time.Second) {
			sctx, scancel := ctxTimeout(5 * time.Second)
			_, errstr, err := c.Call(sctx, "set_property", "time-pos", snap.TimePos)
			scancel()
			if err == nil && (errstr == "" || errstr == "success") {
				seeked = snap.TimePos
			}
		}
	}
	if snap.Paused {
		pctx, pcancel := ctxTimeout(5 * time.Second)
		_, _, _ = c.Call(pctx, "set_property", "pause", true)
		pcancel()
	}
	go d.prefetchTitles(c)
	if seeked > 0 {
		return fmt.Sprintf("restored %d tracks at #%d (%s in)%s", len(snap.Tracks), snap.Pos+1, mmss(seeked), fallback), nil
	}
	return fmt.Sprintf("restored %d tracks at #%d%s", len(snap.Tracks), snap.Pos+1, fallback), nil
}

// snapshot persists the live mpv queue + position so a kill never loses it.
// Callers hold d.mu, so d.ipc cannot change under it.
func (d *daemon) snapshot() {
	d.snapshotWith(d.ipc)
}

// snapshotWith persists the queue for a known client. Safe to run off the
// reply path: it only touches the ipcClient (correlated request ids) and the
// title cache (own mutex). A slightly stale snapshot is fine; racing the
// daemon mutex is not.
func (d *daemon) snapshotWith(c *ipcClient) {
	if c == nil {
		return
	}
	select {
	case <-c.closed:
		return
	default:
	}
	pl, err := d.playlist(c)
	if err != nil {
		return
	}
	pos, _ := d.pos(c)
	paused, _ := d.paused(c)
	ctx, cancel := ctxTimeout(2 * time.Second)
	timePos, _ := c.PropFloat(ctx, "time-pos") // unavailable while a track loads
	cancel()
	tracks := make([]Track, 0, len(pl))
	for _, e := range pl {
		tracks = append(tracks, Track{URL: e.Filename, Title: d.titleOf(e)})
	}
	_ = saveSnapshot(stateFile(), Snapshot{Tracks: tracks, Pos: pos, TimePos: timePos, Paused: paused})
}

// waitLoaded blocks until mpv has a duration for the current entry, i.e. the
// file is demuxed far enough that seeking is meaningful.
func waitLoaded(c *ipcClient, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := ctxTimeout(1500 * time.Millisecond)
		dur, err := c.PropFloat(ctx, "duration")
		cancel()
		if err == nil && dur > 0 {
			return true
		}
		time.Sleep(120 * time.Millisecond)
	}
	return false
}

// ---------- server ----------

func (d *daemon) Run() error {
	if err := os.MkdirAll(filepath.Dir(d.ctl), 0o755); err != nil {
		return err
	}
	// A live daemon answers ping; a dead one leaves a stale socket file behind.
	if out, err := controlCall(d.ctl, "ping", nil, 1500*time.Millisecond); err == nil && out == "pong" {
		return errDaemonRunning
	}
	os.Remove(d.ctl)
	ln, err := net.Listen("unix", d.ctl)
	if err != nil {
		return err
	}
	defer ln.Close()

	go d.eventLoop()
	go d.autosaveLoop()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func(c net.Conn) {
			defer c.Close()
			dec := json.NewDecoder(c)
			enc := json.NewEncoder(c)
			for {
				var req ctlRequest
				if err := dec.Decode(&req); err != nil {
					return
				}
				// subprocess-backed verbs resolve here, outside the mutex
				req, early := d.resolveOutsideMu(req)
				var rep ctlReply
				if early != nil {
					rep = *early
				} else {
					d.mu.Lock()
					rep = d.handle(req)
					d.mu.Unlock()
				}
				if err := enc.Encode(rep); err != nil {
					return
				}
			}
		}(conn)
	}
}

// eventLoop drains mpv property events (keeps the lossy channel empty; the
// autosave loop is what actually persists state).
func (d *daemon) eventLoop() {
	for {
		d.mu.Lock()
		c := d.ipc
		d.mu.Unlock()
		if c == nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		select {
		case <-c.Events:
		case <-c.closed:
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// autosaveLoop snapshots every 10s while mpv is alive — crash-safe, no cron.
func (d *daemon) autosaveLoop() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		d.mu.Lock()
		if d.ipc != nil {
			select {
			case <-d.ipc.closed:
			default:
				d.snapshot()
			}
		}
		d.mu.Unlock()
	}
}

func runDaemon() error {
	d := &daemon{sock: mpvSock(), ctl: ctlSock(), titles: newTitleCache(filepath.Join(homeDir(), ".dj", "titles.json"))}
	if err := d.Run(); err != nil {
		return err
	}
	return nil
}
