package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// B5: chunkText must never emit a part over the cap, even for one huge line.
func TestChunkTextOversizedSingleLine(t *testing.T) {
	huge := strings.Repeat("x", 10000)
	parts := chunkText(huge, 3800)
	if len(parts) < 3 {
		t.Fatalf("expected >=3 parts, got %d", len(parts))
	}
	for i, p := range parts {
		if len(p) > 3800 {
			t.Fatalf("part %d is %d bytes, over cap", i, len(p))
		}
	}
	if got := strings.Join(parts, ""); got != huge {
		t.Fatalf("byte total not preserved: %d vs %d", len(got), len(huge))
	}
}

// B5: splitting must land on rune boundaries (multibyte UTF-8 stays valid).
func TestChunkTextUTF8Safety(t *testing.T) {
	huge := strings.Repeat("\u00e9", 5000) // 2 bytes each = 10000 bytes
	for _, p := range chunkText(huge, 3800) {
		if len(p) > 3800 {
			t.Fatalf("part over cap: %d", len(p))
		}
		if !utf8.ValidString(p) {
			t.Fatalf("cut mid-rune")
		}
	}
}

// Multi-line text keeps its line structure.
func TestChunkTextNormal(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("1. some track title here\n")
	}
	parts := chunkText(b.String(), 3800)
	for i, p := range parts {
		if len(p) > 3800 {
			t.Fatalf("part %d over cap: %d", i, len(p))
		}
	}
	if !strings.HasPrefix(parts[0], "1. some track") {
		t.Fatalf("first line lost")
	}
}

// B7: find hits parse into id+title with a valid id.
func TestParseFindHits(t *testing.T) {
	in := "1\tTCd6PfxOy0Y\tDaft Punk - Veridis Quo\n2\tHhZaHf8RP6g\tVeridis Quo (Video)\n"
	hits := parseFindHits(in)
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].id != "TCd6PfxOy0Y" {
		t.Fatalf("bad id %q", hits[0].id)
	}
	if !strings.Contains(hits[0].title, "Veridis") {
		t.Fatalf("bad title %q", hits[0].title)
	}
	if hits[1].id != "HhZaHf8RP6g" {
		t.Fatalf("second id wrong: %q", hits[1].id)
	}
}

// B7: garbage lines are dropped, not turned into buttons.
func TestParseFindHitsGarbage(t *testing.T) {
	hits := parseFindHits("not a hit\n\n1\tshort\tbad id\n")
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits, got %d: %+v", len(hits), hits)
	}
}

func TestBotVideoID(t *testing.T) {
	if !botVideoID("UE_-obgiWm0") {
		t.Fatal("valid id rejected")
	}
	if botVideoID("UE_-obgiWm") {
		t.Fatal("short id accepted")
	}
	if botVideoID("UE_-obgiWm!") {
		t.Fatal("illegal char accepted")
	}
}

// Live integration: /find must reach the real daemon and parse real hits.
// Opt-in only (DJGO_LIVE=1) so a normal `go test` never touches a live daemon.
func TestHandleBotCommandFindLive(t *testing.T) {
	if os.Getenv("DJGO_LIVE") != "1" {
		t.Skip("set DJGO_LIVE=1 to run against the live daemon")
	}
	raw := callCtl("search", []string{"daft punk veridis quo"}, 60*time.Second)
	hits := parseFindHits(raw)
	if len(hits) == 0 {
		t.Fatalf("daemon search returned no parseable hits: %q", raw)
	}

	txt, markup := handleBotCommand("/find daft punk veridis quo")
	if !strings.Contains(txt, "1.") {
		t.Fatalf("reply not numbered: %q", txt)
	}
	if markup == nil {
		t.Fatal("no keyboard produced")
	}
	kb, ok := markup.(map[string]any)
	if !ok {
		t.Fatalf("markup is %T not map", markup)
	}
	rows, ok := kb["inline_keyboard"].([][]map[string]any)
	if !ok || len(rows) == 0 || len(rows[0]) != len(hits) {
		t.Fatalf("expected %d buttons, got %#v", len(hits), kb["inline_keyboard"])
	}
	if got := rows[0][0]["callback_data"]; got != "q:"+hits[0].id {
		t.Fatalf("first button data %v, want q:%s", got, hits[0].id)
	}
	t.Logf("verified %d hits, first: %s / %s", len(hits), hits[0].id, hits[0].title)
}

// The single hardest requirement: bare chat text must NEVER reach yt-dlp or the
// daemon. If this regresses, a stray sentence becomes a queued song.
func TestBareTextNeverResolvesToSong(t *testing.T) {
	for _, in := range []string{
		"play something by kodak black",
		"kodak black no flockin",
		"https://www.youtube.com/watch?v=UE_-obgiWm0",
		"skip it please",
		"",
		"   ",
	} {
		out, markup := handleBotCommand(in)
		if !strings.Contains(out, "not a command") && !strings.Contains(out, "DJ bot") {
			t.Fatalf("input %q was not refused/hinted, got %q", in, out)
		}
		if markup != nil {
			// the menu keyboard is fine, but no result payload may be produced
			if kb, ok := markup.(map[string]any); ok {
				if _, has := kb["inline_keyboard"]; !has {
					t.Fatalf("input %q returned unexpected markup %#v", in, markup)
				}
			}
		}
	}
}

// Every slash command must route to a daemon verb without resolving bare text.
func TestSlashCommandsAreDistinct(t *testing.T) {
	for _, in := range []string{"/help", "/now", "/list", "/skip", "/pause", "/play", "/status", "/vol", "/restore"} {
		out, _ := handleBotCommand(in)
		if strings.Contains(out, "not a command") {
			t.Fatalf("%q was treated as bare text: %q", in, out)
		}
	}
}

// loadM3U is the restore fallback; a title with a comma must survive intact.
func TestLoadM3URoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.m3u")
	tracks := []Track{
		{URL: "https://a/1", Title: "Artist - Song, With A Comma"},
		{URL: "https://a/2#frag", Title: "Title #1"},
		{URL: "https://a/3", Title: ""},
	}
	if err := writeM3U(path, tracks); err != nil {
		t.Fatalf("writeM3U: %v", err)
	}
	got, err := loadM3U(path)
	if err != nil {
		t.Fatalf("loadM3U: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 tracks, got %d: %+v", len(got), got)
	}
	if got[0].Title != tracks[0].Title {
		t.Fatalf("comma title mangled: %q", got[0].Title)
	}
	if got[1].URL != tracks[1].URL {
		t.Fatalf("url with # mangled: %q", got[1].URL)
	}
	if got[2].Title != "" {
		t.Fatalf("placeholder title invented: %q", got[2].Title)
	}
}
