package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ytResolve runs yt-dlp for a single search hit and returns its video id + title.
// yt-dlp stays a subprocess on purpose: writing a YouTube extractor is a bad trade.
// The timeout matters: an unresponsive network must not wedge the daemon forever.
func ytResolve(query string) (id, title string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "yt-dlp", "--no-warnings", "--default-search", "none",
		"--get-id", "--get-title", "ytsearch1:"+query)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", "", errors.New("yt-dlp: timed out")
		}
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	// yt-dlp prints title first, then id
	if len(lines) < 2 {
		return "", "", errors.New("yt-dlp: no result")
	}
	title, id = lines[0], lines[1]
	if !validVideoID(id) {
		return "", "", errors.New("yt-dlp: invalid video id")
	}
	return id, title, nil
}

// ytSearch returns up to limit hits as "<n>\t<id>\t<title>" lines, one per line.
// Lines whose id is not a valid video id (channels, playlists) are dropped, so
// an empty result really means "nothing playable".
func ytSearch(query string, limit int) (string, error) {
	if limit <= 0 {
		limit = 5
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "yt-dlp", "--no-warnings", "--default-search", "none",
		"--flat-playlist", "--print", "%(id)s\t%(title)s",
		fmt.Sprintf("ytsearch%d:%s", limit, query))
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", errors.New("search timed out")
		}
		return "", fmt.Errorf("search failed: %v", err)
	}
	var b strings.Builder
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, title, ok := strings.Cut(line, "\t")
		if !ok || !validVideoID(id) {
			continue
		}
		n++
		fmt.Fprintf(&b, "%d\t%s\t%s\n", n, id, strings.TrimSpace(title))
		if n >= limit {
			break
		}
	}
	if n == 0 {
		return "", fmt.Errorf("no results for: %s", query)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// validVideoID enforces YouTube's 11-char id alphabet, which is what stops a
// garbage search hit (e.g. a channel name) from being queued as a URL.
func validVideoID(id string) bool {
	if len(id) != 11 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
