package portkit

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ListeningSockets returns the TCP sockets in LISTEN state owned by process
// group pgid: the socket inodes under /proc/<pid>/fd of every process in the
// group, matched against /proc/net/tcp and /proc/net/tcp6.
func ListeningSockets(pgid int) ([]Socket, error) {
	inodes := map[string]bool{}
	pids, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	for _, e := range pids {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if g, err := procPgid(pid); err != nil || g != pgid {
			continue
		}
		fds, _ := os.ReadDir(filepath.Join("/proc", e.Name(), "fd"))
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join("/proc", e.Name(), "fd", fd.Name()))
			if err == nil && strings.HasPrefix(link, "socket:[") {
				inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
			}
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	var socks []Socket
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, s := range parseProcNetTCP(data) {
			if inodes[s.inode] {
				socks = append(socks, s.Socket)
			}
		}
	}
	return socks, nil
}

type procSocket struct {
	Socket
	inode string
}

// parseProcNetTCP returns the LISTEN (state 0A) sockets in /proc/net/tcp{,6}.
func parseProcNetTCP(data []byte) []procSocket {
	var out []procSocket
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[3] != "0A" {
			continue
		}
		hexIP, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil {
			continue
		}
		ip, err := parseProcIP(hexIP)
		if err != nil {
			continue
		}
		out = append(out, procSocket{Socket: Socket{IP: ip, Port: int(port)}, inode: f[9]})
	}
	return out
}

// parseProcIP decodes an address from /proc/net/tcp, stored as 32-bit words
// in host (little-endian) byte order.
func parseProcIP(s string) (string, error) {
	b, err := hex.DecodeString(s)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return "", fmt.Errorf("bad address %q", s)
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	ip := net.IP(b)
	if v4 := ip.To4(); v4 != nil && len(b) == 16 && !ip.Equal(net.IPv6unspecified) && !ip.Equal(net.IPv6loopback) {
		return v4.String(), nil // ::ffff:127.0.0.1
	}
	return ip.String(), nil
}

// procStat returns the fields of /proc/<pid>/stat after the command name.
func procStat(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return nil, errors.New("bad stat")
	}
	return strings.Fields(string(data[i+1:])), nil
}

func procPgid(pid int) (int, error) {
	f, err := procStat(pid)
	if err != nil || len(f) < 3 {
		return 0, err
	}
	return strconv.Atoi(f[2]) // field 5: pgrp
}

// ProcessStartTime identifies a process across PID reuse: field 22 of
// /proc/<pid>/stat, in clock ticks since boot.
func ProcessStartTime(pid int) (string, error) {
	f, err := procStat(pid)
	if err != nil {
		return "", err
	}
	if len(f) < 20 {
		return "", errors.New("bad stat")
	}
	if f[0] == "Z" {
		return "", syscall.ESRCH // a zombie has exited
	}
	return f[19], nil
}

// ProcessUID returns the user ID a process runs as.
func ProcessUID(pid int) (int, error) {
	fi, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return 0, err
	}
	return int(fi.Sys().(*syscall.Stat_t).Uid), nil
}

// waitExit blocks until pid exits, using a pidfd, or polling on kernels
// without pidfd_open.
func waitExit(pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		for syscall.Kill(pid, 0) == nil {
			time.Sleep(2 * time.Second)
		}
		return nil
	}
	defer unix.Close(fd)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(fds, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}
