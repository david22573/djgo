# djgo

A music-player daemon for Termux/Android that drives [mpv](https://mpv.io) over its
JSON IPC socket, with a small CLI and a Telegram bot as control clients.

Zero dependencies — Go standard library only.

## Design

Two sockets, one writer:

```
  dj (CLI) ─┐
            ├─► ~/.dj/djd.sock ─► djgo daemon ─► /usr/tmp/mpv.sock ─► mpv
  djgo bot ─┘   (JSON lines)                        (mpv JSON IPC)
```

Nothing but the daemon talks to mpv. The CLI and the bot are just control clients
that send `{id,cmd,args}` over a Unix socket and read back `{id,err,out}`. The
daemon serialises mutations under a single mutex, so there is exactly one writer
to mpv and the queue can never be corrupted by concurrent clients.

Subprocess-backed verbs (`add <words>`, `search`) resolve yt-dlp **outside** that
mutex, so a slow or hung network lookup cannot stall `now`, `skip` or `ping`.

State survives a kill: the queue is snapshotted to `~/.dj/queue.json` after every
mutation and on a 10s ticker, written atomically (temp file + rename).
`djgo restore` reloads it, including the position within the current track, and
falls back to `~/.dj/../playlists/.autosave.m3u` if the snapshot is gone.

## Install

Requires Go, `mpv`, and `yt-dlp`:

```sh
pkg install mpv yt-dlp golang
git clone git@github.com:david22573/djgo.git
cd djgo
go build -o djgo .
cp djgo "$PREFIX/bin/djgo.new" && mv -f "$PREFIX/bin/djgo.new" "$PREFIX/bin/djgo"
```

The `.new` + `mv` dance is required: `cp`ing over a running binary fails with
`Text file busy`, and the rename swaps the inode while the old process keeps
running the old code.

## Usage

```
djgo now | list | add <url|words> | rm <n> | pos <n>
djgo skip | pause | play | vol [0-100|+10|-10] | search <words>
djgo save [file.m3u] | start [m3u] | stop | status
djgo restore | bot | daemon
```

The CLI auto-spawns the daemon on first use. `djgo search` prints up to five
candidates as `N<TAB><video-id><TAB><title>`; `djgo add` accepts either search
words or a URL.

## Telegram bot

```sh
djbot start     # launcher for: djgo bot
```

`/now /list /add /rm /pos /skip /pause /play /vol /find /save /startmpv /stop
/status /restore`, plus inline buttons. `/find` returns numbered hits with one
tap target each — nothing is queued until a button is pressed.

Hard rules the bot enforces:

- **Bare chat text is never resolved into a song.** Only slash commands act.
- No `parse_mode` is ever sent — track titles contain `_ . ( ) [ ]` which
  Telegram's Markdown parser rejects with a 400.
- The chat allowlist fails **closed**: with no resolvable chat ids the bot
  refuses to start rather than accepting commands from strangers.
- Exactly one process may call `getUpdates` per token; a `409 Conflict` makes the
  process exit instead of fighting the other poller.

## Notes

- mpv must run with `--idle=yes --keep-open=yes`, otherwise it exits at the end
  of the playlist and the in-memory queue is lost.
- Detect mpv with `pgrep -x mpv`, never `pgrep -f` — the command-line match also
  matches shell wrappers.
- `/tmp` is not writable on Android; test paths belong under `$HOME`.
- `djgo save` writes `#EXTINF` lines only when a real title exists. A placeholder
  title is inherited by mpv forever, which is how restored queues used to show
  "unknown" permanently.

`test/parity.sh` diffs the older shell `dj` wrapper against `djgo` for
`now`/`status`/`list` with timings normalised.
