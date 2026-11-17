package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// titleCache maps YouTube URL -> human title. mpv only resolves titles when it
// actually loads an entry, so queued-but-never-played tracks (and anything
// restored from a playlist file) otherwise show "(loading) url" or "unknown".
type titleCache struct {
	mu   sync.Mutex
	m    map[string]string
	path string
	sem  chan struct{} // bounds concurrent yt-dlp calls
}

func newTitleCache(path string) *titleCache {
	t := &titleCache{m: map[string]string{}, path: path, sem: make(chan struct{}, 3)}
	if buf, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(buf, &t.m)
	}
	if t.m == nil {
		t.m = map[string]string{}
	}
	return t
}

func (t *titleCache) get(url string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[url]
}

// set writes the file while still holding the mutex: two concurrent setters
// would otherwise race, and the older snapshot could win the rename.
func (t *titleCache) set(url, title string) {
	if url == "" || title == "" || title == "unknown" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m[url] == title {
		return
	}
	t.m[url] = title
	buf, err := json.MarshalIndent(t.m, "", " ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if os.WriteFile(tmp, buf, 0o644) == nil {
		_ = os.Rename(tmp, t.path)
	}
}

// resolve runs yt-dlp for one URL (no search) and caches the title. The ctx
// bounds the subprocess so a hung yt-dlp cannot hold a semaphore slot forever.
func (t *titleCache) resolve(url string) string {
	if got := t.get(url); got != "" {
		return got
	}
	t.sem <- struct{}{}
	defer func() { <-t.sem }()
	if got := t.get(url); got != "" { // another goroutine won the race
		return got
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "yt-dlp", "--no-warnings", "--get-title", "--no-playlist", url).Output()
	if err != nil {
		return ""
	}
	title := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if title == "" || title == "unknown" {
		return ""
	}
	t.set(url, title)
	return title
}
