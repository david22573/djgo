package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

var (
	errAlreadyRunning = errors.New("mpv already running")
	errTimeout        = errors.New("timed out waiting for mpv socket")
	errDaemonRunning  = errors.New("daemon already running")
)

type ctlRequest struct {
	ID   int      `json:"id"`
	Cmd  string   `json:"cmd"`
	Args []string `json:"args,omitempty"`
}

type ctlReply struct {
	ID  int    `json:"id"`
	Err string `json:"err,omitempty"`
	Out string `json:"out"`
}

// controlCall sends one verb to the resident daemon and reads its answer.
func controlCall(sock, cmd string, args []string, timeout time.Duration) (string, error) {
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	conn.SetDeadline(time.Now().Add(timeout))
	enc := json.NewEncoder(conn)
	if err := enc.Encode(ctlRequest{ID: 1, Cmd: cmd, Args: args}); err != nil {
		return "", err
	}
	dec := json.NewDecoder(conn)
	var rep ctlReply
	if err := dec.Decode(&rep); err != nil {
		return "", err
	}
	if rep.Err != "" {
		return rep.Out, errors.New(rep.Err)
	}
	return rep.Out, nil
}

// spawnDaemon starts `dj daemon` detached and waits until its control socket answers.
func spawnDaemon() error {
	if err := os.MkdirAll(filepath.Dir(ctlSock()), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(daemonLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "daemon")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	go func() { _ = cmd.Wait() }()
	logf.Close()

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := controlCall(ctlSock(), "ping", nil, 2*time.Second); err == nil && out == "pong" {
			return nil
		}
		time.Sleep(60 * time.Millisecond)
	}
	// Another CLI may have raced us into existence; a live daemon answers even if we lost.
	if out, err := controlCall(ctlSock(), "ping", nil, 2*time.Second); err == nil && out == "pong" {
		return nil
	}
	return fmt.Errorf("daemon did not come up (see %s)", daemonLogPath())
}

// ensureDaemon returns a working control path, spawning the daemon if needed.
func ensureDaemon() (string, error) {
	sock := ctlSock()
	if out, err := controlCall(sock, "ping", nil, 2*time.Second); err == nil && out == "pong" {
		return sock, nil
	}
	if err := spawnDaemon(); err != nil {
		return sock, err
	}
	return sock, nil
}
