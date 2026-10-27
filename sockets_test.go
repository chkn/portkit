package portkit

import (
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestListeningSockets starts a process in its own group that listens on a
// port, and finds that port through the OS.
func TestListeningSockets(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f, err := ln.(*net.TCPListener).File()
	ln.Close()
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// The child inherits the listening socket as fd 3 and sleeps.
	cmd := exec.Command("sleep", "10")
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer cmd.Process.Kill()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		socks, err := ListeningSockets(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(socks, Socket{IP: "127.0.0.1", Port: port}) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("port %d not found for process group %d", port, cmd.Process.Pid)
}

func TestOwner(t *testing.T) {
	o, err := OwnerOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !o.alive() {
		t.Error("this process isn't alive")
	}
	if uid, err := ProcessUID(os.Getpid()); err != nil || uid != os.Getuid() {
		t.Errorf("ProcessUID = %d, %v", uid, err)
	}
	reused := *o
	reused.Started = "a different start"
	if reused.alive() {
		t.Error("a reused PID counted as the same process")
	}

	cmd := exec.Command("sleep", "0.2")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child, err := OwnerOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- waitExit(cmd.Process.Pid) }()
	go cmd.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitExit didn't return")
	}
	time.Sleep(50 * time.Millisecond)
	if child.alive() {
		t.Error("exited child still alive " + strconv.Itoa(child.PID))
	}
}
