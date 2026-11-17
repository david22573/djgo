package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// socketAlive dials the mpv IPC socket: a connect succeeds only if mpv holds it.
func socketAlive(sock string) bool {
	conn, err := net.DialTimeout("unix", sock, 400*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// startMPV launches mpv detached (own session), logging to the mpv log file,
// and waits for its IPC socket to appear.
func startMPV(playlist string) (int, error) {
	sock := mpvSock()
	if socketAlive(sock) {
		return 0, errAlreadyRunning
	}
	if _, err := os.Stat(sock); err == nil {
		os.Remove(sock) // stale socket from a dead mpv
	}
	if err := os.MkdirAll(filepath.Dir(mpvLogPath()), 0o755); err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(mpvLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer logf.Close()

	args := []string{
		"--no-video", "--idle=yes", "--keep-open=yes",
		"--ytdl-format=bestaudio",
		"--input-ipc-server=" + sock,
	}
	if playlist != "" {
		args = append(args, "--playlist="+playlist)
	}
	cmd := exec.Command("mpv", args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap so the daemon doesn't grow zombies

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if socketAlive(sock) {
			return pid, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return pid, errTimeout
}
