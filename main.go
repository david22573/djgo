package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const helpText = `usage: dj <cmd>
  now | list | add <url|words> | rm <n> | pos <n>
  skip | pause | play | vol [0-100|+10|-10] | search <words>
  save [file.m3u] | start [m3u] | stop | status
  restore | bot | daemon`

// verb aliases: CLI name -> daemon verb
var aliases = map[string]string{
	"now": "now", "list": "list", "ls": "list",
	"add": "add", "rm": "rm", "del": "rm",
	"pos": "pos", "go": "pos",
	"skip": "skip", "next": "skip",
	"pause": "pause", "play": "play", "resume": "play",
	"save": "save", "start": "start", "stop": "stop", "kill": "stop",
	"status": "status", "restore": "restore",
	"vol": "vol", "volume": "vol", "search": "search", "find": "search",
}

// per-verb deadlines: search resolution and mpv launch need more than a query
var slowVerbs = map[string]time.Duration{
	"add": 180 * time.Second, "start": 60 * time.Second, "restore": 90 * time.Second,
	"stop": 30 * time.Second, "save": 30 * time.Second, "search": 60 * time.Second,
}

func die(msg string) {
	fmt.Fprintf(os.Stderr, "dj: %s\n", msg)
	os.Exit(1)
}

func main() {
	args := os.Args[1:]
	cmdName := "now"
	if len(args) > 0 {
		cmdName = args[0]
	}
	rest := args[1:]

	switch cmdName {
	case "daemon":
		if err := runDaemon(); err != nil {
			if errors.Is(err, errDaemonRunning) {
				return // another daemon owns the control socket
			}
			die(err.Error())
		}
		return
	case "bot":
		if err := runBot(); err != nil {
			die(err.Error())
		}
		return
	case "help", "-h", "--help":
		fmt.Println(helpText)
		return
	}

	verb, ok := aliases[cmdName]
	if !ok {
		die("unknown command " + strconvQuote(cmdName) + "\n" + helpText)
	}

	sock, err := ensureDaemon()
	if err != nil {
		die(err.Error())
	}
	timeout := 30 * time.Second
	if d, ok := slowVerbs[verb]; ok {
		timeout = d
	}
	out, err := controlCall(sock, verb, rest, timeout)
	if err != nil {
		if out != "" {
			fmt.Println(out)
		}
		die(err.Error())
	}
	if out != "" {
		fmt.Println(out)
	}
}

func strconvQuote(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
}
