package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type tgClient struct {
	token string
	base  string
	http  *http.Client
}

type tgUpdate struct {
	UpdateID      int              `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

type tgMessage struct {
	MessageID int `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Text string `json:"text"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	Data    string     `json:"data"`
	Message *tgMessage `json:"message"`
}

func newTG(token string) *tgClient {
	return &tgClient{
		token: token,
		base:  "https://api.telegram.org/bot" + token + "/",
		http:  &http.Client{Timeout: 70 * time.Second},
	}
}

func (t *tgClient) call(method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", t.base+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if !env.OK {
		// keep the HTTP status in the text: it is what tells a 409 Conflict
		// (another getUpdates poller owns the token) apart from a transient blip
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s (http %d)", env.Description, resp.StatusCode)
		}
		return nil, errors.New(env.Description)
	}
	return env.Result, nil
}

// ---- env / allowlist ----

func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

func allowedChats() map[string]bool {
	allow := map[string]bool{}
	dir := filepath.Join(homeDir(), ".hermes", "channel_directory.json")
	if buf, err := os.ReadFile(dir); err == nil {
		var d struct {
			Platforms struct {
				Telegram []struct {
					ID string `json:"id"`
				} `json:"telegram"`
			} `json:"platforms"`
		}
		if json.Unmarshal(buf, &d) == nil {
			for _, c := range d.Platforms.Telegram {
				if c.ID != "" {
					allow[c.ID] = true
				}
			}
		}
	}
	if v := os.Getenv("DJ_TG_CHAT"); v != "" {
		allow[v] = true
	}
	return allow
}

// ---- menu / text ----

func kbMenu() map[string]any {
	btn := func(text, data string) map[string]any {
		return map[string]any{"text": text, "callback_data": data}
	}
	return map[string]any{"inline_keyboard": [][]map[string]any{
		{btn("🔄 Now", "now"), btn("📜 Queue", "list")},
		{btn("⏭ Skip", "skip"), btn("⏸ Pause", "pause"), btn("▶️ Play", "play")},
		{btn("💾 Save", "save"), btn("🛑 Stop mpv", "stop")},
		{btn("↩️ Restore", "restore")},
		{btn("🔉 Vol -10", "vol:-10"), btn("🔊 Vol +10", "vol:+10")},
	}}
}

const botHelp = `🎧 DJ bot
/now — what's playing
/list — queue
/add <song or url> — queue it
/find <words> — search, then tap a hit to queue
/rm <n> — drop track n
/pos <n> — jump to n
/skip /pause /play
/vol — show volume, /vol 40 | /vol +10 | /vol -10
/save — dump queue to m3u
/restore — bring back the saved queue
/startmpv [m3u] — launch mpv
/stop — kill mpv
/status — mpv alive?`

var botCommands = [][2]string{
	{"now", "what's playing"}, {"list", "show the queue"}, {"add", "queue a song or url"},
	{"find", "search youtube, tap to queue"},
	{"rm", "remove track by number"}, {"pos", "jump to a track"}, {"skip", "skip current"},
	{"pause", "pause"}, {"play", "resume"}, {"vol", "volume 0-100 or +10 / -10"},
	{"save", "save queue to m3u"}, {"restore", "restore the saved queue"},
	{"startmpv", "launch mpv"}, {"stop", "stop mpv"}, {"status", "is mpv alive"},
	{"help", "commands"},
}

// callCtl is the bot's only way to act: it is just another control client.
func callCtl(cmd string, args []string, timeout time.Duration) string {
	sock, err := ensureDaemon()
	if err != nil {
		return "⚠️ " + err.Error()
	}
	out, err := controlCall(sock, cmd, args, timeout)
	if err != nil {
		if out != "" {
			return out + "\n⚠️ " + err.Error()
		}
		return "⚠️ " + err.Error()
	}
	return out
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// validVideoID gates youtube ids that arrive from callback_data before they are
// pasted into a URL: 11 chars, base64url alphabet only.
func botVideoID(s string) bool {
	if len(s) != 11 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

type findHit struct {
	id    string
	title string
}

// parseFindHits reads the search verb's "<n>\t<11-char-id>\t<title>" lines.
func parseFindHits(s string) []findHit {
	var hits []findHit
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		id := strings.TrimSpace(f[1])
		if !botVideoID(id) {
			continue
		}
		title := strings.TrimSpace(strings.Join(f[2:], " "))
		if title == "" {
			title = id
		}
		hits = append(hits, findHit{id: id, title: title})
	}
	return hits
}

// findReply turns a search reply into a numbered list plus one tap target per
// hit. Nothing is queued here — the tap does that.
func findReply(out string) (string, any) {
	hits := parseFindHits(out)
	if len(hits) == 0 {
		return out, nil
	}
	var b strings.Builder
	b.WriteString("🔎 results — tap one to queue:\n")
	row := make([]map[string]any, 0, len(hits))
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. %s\n", i+1, clipRunes(h.title, 70))
		row = append(row, map[string]any{
			"text":          strconv.Itoa(i + 1),
			"callback_data": "q:" + h.id, // 13 bytes, well under Telegram's 64
		})
	}
	return b.String(), map[string]any{"inline_keyboard": [][]map[string]any{row}}
}

// handleBotCommand returns the reply text and an optional keyboard; a nil
// keyboard means "use the main menu".
func handleBotCommand(text string) (string, any) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return botHelp, nil
	}
	cmd := parts[0]
	if i := strings.Index(cmd, "@"); i > 0 {
		cmd = cmd[:i]
	}
	cmd = strings.ToLower(cmd)
	args := parts[1:]
	switch cmd {
	case "/start", "/help", "/menu":
		return botHelp, nil
	case "/now":
		return callCtl("now", nil, 10*time.Second), nil
	case "/list", "/ls":
		return callCtl("list", nil, 15*time.Second), nil
	case "/add":
		if len(args) == 0 {
			return "usage: /add <song name or youtube url>", nil
		}
		// the daemon answers "queued: <title>" — pass it through verbatim
		return callCtl("add", args, 180*time.Second), nil
	case "/find", "/search":
		if len(args) == 0 {
			return "usage: /find <words>", nil
		}
		return findReply(callCtl("search", args, 55*time.Second)) // > daemon's 45s ytSearch ctx
	case "/rm":
		if len(args) == 0 {
			return "usage: /rm <n>", nil
		}
		return callCtl("rm", args[:1], 15*time.Second), nil
	case "/pos":
		if len(args) == 0 {
			return "usage: /pos <n>", nil
		}
		return callCtl("pos", args[:1], 15*time.Second), nil
	case "/skip":
		// the daemon's skip reply already re-reads media-title: asking for
		// "now" here raced the freshly loading track
		return callCtl("skip", nil, 15*time.Second), nil
	case "/pause":
		return callCtl("pause", nil, 10*time.Second), nil
	case "/play":
		return callCtl("play", nil, 10*time.Second), nil
	case "/vol", "/volume":
		if len(args) == 0 {
			return callCtl("vol", nil, 10*time.Second), nil
		}
		return callCtl("vol", args[:1], 10*time.Second), nil
	case "/save":
		return callCtl("save", nil, 30*time.Second), nil
	case "/restore":
		return callCtl("restore", nil, 90*time.Second), nil
	case "/startmpv":
		return callCtl("start", args, 60*time.Second), nil
	case "/stop", "/kill":
		return callCtl("stop", nil, 30*time.Second), nil
	case "/status":
		return callCtl("status", nil, 10*time.Second), nil
	}
	// bare chat text must never hit yt-dlp: a stray sentence once queued a song
	return "⚠️ not a command — nothing queued.\nUse /add <song or url> to queue music, /help for the list.", nil
}

// ---- sending ----

func chunkText(s string, max int) []string {
	if max <= 0 {
		return []string{s}
	}
	var parts []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, cur.String())
			cur.Reset()
		}
	}
	for _, line := range strings.SplitAfter(s, "\n") {
		if len(line) > max {
			// a single oversized line cannot be split on line boundaries
			flush()
			parts = append(parts, splitByRunes(line, max)...)
			continue
		}
		if cur.Len()+len(line) > max {
			flush()
		}
		cur.WriteString(line)
	}
	flush()
	if len(parts) == 0 {
		parts = []string{s}
	}
	return parts
}

// splitByRunes cuts on rune boundaries so multi-byte titles do not produce
// invalid UTF-8, and never exceeds max bytes either.
func splitByRunes(s string, max int) []string {
	var out []string
	var b strings.Builder
	for _, r := range s {
		if b.Len()+utf8.RuneLen(r) > max {
			out = append(out, b.String())
			b.Reset()
		}
		b.WriteRune(r)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func (t *tgClient) sendLong(chatID int64, text string, markup any) {
	parts := chunkText(text, 3800)
	for i, p := range parts {
		params := map[string]any{"chat_id": chatID, "text": p}
		if markup != nil && i == len(parts)-1 {
			params["reply_markup"] = markup
		}
		if _, err := t.call("sendMessage", params); err != nil {
			fmt.Fprintf(os.Stderr, "djbot: sendMessage: %v (retrying once)\n", err)
			time.Sleep(2 * time.Second)
			if _, err2 := t.call("sendMessage", params); err2 != nil {
				fmt.Fprintf(os.Stderr, "djbot: sendMessage retry failed: %v\n", err2)
			}
		}
	}
}

// ---- polling ----

func offsetPath() string { return filepath.Join(homeDir(), ".dj", "bot.offset") }

func readOffset() int {
	buf, err := os.ReadFile(offsetPath())
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(string(buf)), "%d", &n)
	return n
}

func writeOffset(n int) {
	_ = os.MkdirAll(filepath.Dir(offsetPath()), 0o755)
	_ = os.WriteFile(offsetPath(), []byte(fmt.Sprintf("%d", n)), 0o644)
}

func botStartedPath() string { return filepath.Join(homeDir(), ".dj", "bot.started") }

func readStarted() (time.Time, bool) {
	buf, err := os.ReadFile(botStartedPath())
	if err != nil {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(buf)), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

func writeStarted(at time.Time) {
	_ = os.MkdirAll(filepath.Dir(botStartedPath()), 0o755)
	_ = os.WriteFile(botStartedPath(), []byte(strconv.FormatInt(at.Unix(), 10)), 0o644)
}

func runBot() error {
	env := readEnvFile(filepath.Join(homeDir(), ".hermes", ".env"))
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		token = env["TELEGRAM_BOT_TOKEN"]
	}
	if token == "" {
		return errors.New("no TELEGRAM_BOT_TOKEN in ~/.hermes/.env")
	}
	t := newTG(token)

	meRaw, err := t.call("getMe", nil)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	_ = json.Unmarshal(meRaw, &me)

	cmds := make([]map[string]any, 0, len(botCommands))
	for _, c := range botCommands {
		cmds = append(cmds, map[string]any{"command": c[0], "description": c[1]})
	}
	if _, err := t.call("setMyCommands", map[string]any{"commands": cmds}); err != nil {
		fmt.Fprintf(os.Stderr, "djbot: setMyCommands: %v\n", err)
	}

	allow := allowedChats()
	delete(allow, fmt.Sprintf("%d", me.ID)) // a bot cannot message itself
	// fail closed: an empty allowlist would hand mpv to every stranger who
	// finds the bot, so refuse to poll at all
	if len(allow) == 0 {
		return errors.New("refusing to start: telegram allowlist is empty (no telegram ids in ~/.hermes/channel_directory.json and DJ_TG_CHAT unset)")
	}
	fmt.Fprintf(os.Stderr, "djbot: poll as @%s | allowed chats: %v\n", me.Username, keysOf(allow))

	now := time.Now()
	if last, ok := readStarted(); !ok || now.Sub(last) > 30*time.Minute {
		for id := range allow {
			var cid int64
			fmt.Sscanf(id, "%d", &cid)
			t.sendLong(cid, "🎧 DJ bot online — buttons below, /help for commands", kbMenu())
		}
	}
	writeStarted(now)

	// one update at a time froze every later tap behind a long /add, so each
	// update runs in its own goroutine, bounded to keep the box sane
	sem := make(chan struct{}, 4)
	dispatch := func(u tgUpdate) {
		if u.CallbackQuery != nil {
			t.handleCallback(u.CallbackQuery, allow)
			return
		}
		m := u.Message
		if m == nil {
			return
		}
		if !allow[fmt.Sprintf("%d", m.Chat.ID)] {
			// stay silent: replying to strangers turns the bot into a spam
			// oracle and burns Telegram flood budget
			fmt.Fprintf(os.Stderr, "djbot: ignored message from unlisted chat %d\n", m.Chat.ID)
			return
		}
		if strings.TrimSpace(m.Text) == "" {
			return
		}
		msg, markup := handleBotCommand(m.Text)
		if markup == nil {
			markup = kbMenu()
		}
		t.sendLong(m.Chat.ID, msg, markup)
	}

	const baseBackoff = 2 * time.Second
	const maxBackoff = 30 * time.Second
	backoff := baseBackoff
	off := readOffset()
	for {
		params := map[string]any{"offset": off, "timeout": 30}
		res, err := t.call("getUpdates", params)
		if err != nil {
			msg := err.Error()
			if strings.Contains(msg, "Conflict") || strings.Contains(msg, "409") {
				// another getUpdates poller owns this token (e.g. a Hermes
				// gateway): two pollers just steal each other's updates
				fmt.Fprintf(os.Stderr, "djbot: another bot process is polling this token (409 Conflict) — exiting instead of fighting: %v\n", err)
				return fmt.Errorf("getUpdates conflict: %w", err)
			}
			fmt.Fprintf(os.Stderr, "djbot: poll error (retry in %s): %v\n", backoff, err)
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = baseBackoff
		var ups []tgUpdate
		if err := json.Unmarshal(res, &ups); err != nil {
			continue
		}
		for _, u := range ups {
			// the offset is advanced here, in the loop, before dispatch: it must
			// never depend on how long a handler takes
			off = u.UpdateID + 1
			writeOffset(off)
			// take the slot HERE, not inside the goroutine: a burst of updates
			// behind a slow /add would otherwise park an unbounded number of
			// goroutines. Pausing the poll loop is safe — the offset is already
			// persisted, so nothing is lost, just fetched later.
			sem <- struct{}{}
			go func(u tgUpdate) {
				defer func() { <-sem }()
				defer func() {
					// a handler panic must not take the whole bot down
					if r := recover(); r != nil {
						fmt.Fprintf(os.Stderr, "djbot: handler panic: %v\n", r)
					}
				}()
				dispatch(u)
			}(u)
		}
	}
}

func (t *tgClient) handleCallback(cq *tgCallbackQuery, allow map[string]bool) {
	if cq.Message == nil {
		// answer anyway: an untouched callback query leaves the tap spinner up
		_, _ = t.call("answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": "expired"})
		return
	}
	chatID := cq.Message.Chat.ID
	if !allow[fmt.Sprintf("%d", chatID)] {
		_, _ = t.call("answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": "not allowed"})
		return
	}

	// a /find hit: q:<11-char video id>
	if id, ok := strings.CutPrefix(cq.Data, "q:"); ok {
		if !botVideoID(id) {
			_, _ = t.call("answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": "unknown"})
			return
		}
		_, _ = t.call("answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": "queueing…"})
		out := callCtl("add", []string{"https://www.youtube.com/watch?v=" + id}, 180*time.Second)
		if err := t.editText(chatID, cq.Message.MessageID, out); err != nil {
			t.sendLong(chatID, out, kbMenu())
		}
		return
	}

	// a volume button: vol:-10 / vol:+10
	if arg, ok := strings.CutPrefix(cq.Data, "vol:"); ok {
		_, _ = t.call("answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": "volume"})
		var vargs []string
		if arg != "" {
			vargs = []string{arg}
		}
		out := callCtl("vol", vargs, 10*time.Second)
		if err := t.editText(chatID, cq.Message.MessageID, out); err != nil {
			t.sendLong(chatID, out, kbMenu())
		}
		return
	}

	verb, ok := map[string]string{
		"now": "now", "list": "list", "skip": "skip", "pause": "pause",
		"play": "play", "save": "save", "stop": "stop", "restore": "restore",
	}[cq.Data]
	if !ok {
		_, _ = t.call("answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": "unknown"})
		return
	}
	// ack the tap first: the toast lands instantly while the work runs
	toast := map[string]string{"skip": "skipping", "pause": "pausing", "play": "playing",
		"save": "saving", "stop": "stopping mpv", "list": "loading queue…", "now": "…",
		"restore": "restoring…"}[cq.Data]
	_, _ = t.call("answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": toast})

	timeout := 30 * time.Second
	if verb == "restore" {
		timeout = 90 * time.Second
	}
	out := callCtl(verb, nil, timeout)
	if err := t.editText(chatID, cq.Message.MessageID, out); err != nil {
		t.sendLong(chatID, out, kbMenu())
	}
}

func (t *tgClient) editText(chatID int64, msgID int, text string) error {
	if len(text) > 3900 {
		return errors.New("too long to edit")
	}
	_, err := t.call("editMessageText", map[string]any{
		"chat_id": chatID, "message_id": msgID, "text": text, "reply_markup": kbMenu(),
	})
	if err != nil && strings.Contains(err.Error(), "not modified") {
		return nil // Telegram 400s when the text is identical; harmless
	}
	return err
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
