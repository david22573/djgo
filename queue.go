package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Track is one queue entry as persisted in the snapshot.
type Track struct {
	URL   string  `json:"url"`
	Title string  `json:"title"`
	Secs  float64 `json:"secs,omitempty"`
}

// Snapshot is the whole player state the daemon can restore after a kill.
type Snapshot struct {
	Version int       `json:"version"`
	Tracks  []Track   `json:"tracks"`
	Pos     int       `json:"pos"`
	TimePos float64   `json:"time_pos,omitempty"` // seconds into the current track
	Paused  bool      `json:"paused"`
	SavedAt time.Time `json:"saved_at"`
}

const snapshotVersion = 1

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mpvSock() string { return envOr("DJ_MPV_SOCK", "/data/data/com.termux/files/usr/tmp/mpv.sock") }
func ctlSock() string {
	return envOr("DJ_CTRL_SOCK", filepath.Join(homeDir(), ".dj", "djd.sock"))
}
func stateFile() string { return envOr("DJ_STATE", filepath.Join(homeDir(), ".dj", "queue.json")) }
func playlistDir() string {
	return envOr("DJ_PLAYLIST_DIR", filepath.Join(homeDir(), "playlists"))
}
func mpvLogPath() string { return envOr("DJ_MPV_LOG", "/data/data/com.termux/files/usr/tmp/mpv.log") }
func daemonLogPath() string {
	return envOr("DJ_DAEMON_LOG", filepath.Join(homeDir(), ".dj", "daemon.log"))
}

// saveSnapshot writes atomically: temp file in the same dir, then rename.
func saveSnapshot(path string, s Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	s.Version = snapshotVersion
	s.SavedAt = time.Now()
	buf, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadSnapshot(path string) (Snapshot, error) {
	var s Snapshot
	buf, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(buf, &s)
	return s, err
}

// writeM3U renders tracks as a playlist mpv can load with `loadlist`.
func writeM3U(path string, tracks []Track) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("#EXTM3U\n"); err != nil {
		return err
	}
	for _, t := range tracks {
		title := t.Title
		// never bake a placeholder into the playlist: mpv inherits #EXTINF as the
		// entry title, which is what made restored queues show "unknown" forever.
		if title != "" && title != "unknown" {
			if _, err := f.WriteString("#EXTINF:-1," + title + "\n"); err != nil {
				return err
			}
		}
		if _, err := f.WriteString(t.URL + "\n"); err != nil {
			return err
		}
	}
	return f.Sync()
}

// loadM3U reads back an #EXTINF playlist (written by writeM3U or by mpv's own
// autosave). Used as the restore() fallback when the json snapshot is gone.
func loadM3U(path string) ([]Track, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tracks := make([]Track, 0, 64)
	pending := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXTINF:"):
			if _, after, ok := strings.Cut(line, ","); ok {
				pending = strings.TrimSpace(after)
			}
		case strings.HasPrefix(line, "#"):
			// other m3u directives (#EXTM3U, #PLAYLIST:) carry no entry data
		default:
			tracks = append(tracks, Track{URL: line, Title: pending})
			pending = ""
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return tracks, nil
}
