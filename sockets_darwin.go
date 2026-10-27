package portkit

import (
	"bufio"
	"bytes"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ListeningSockets returns the TCP sockets in LISTEN state owned by process
// group pgid, using lsof.
func ListeningSockets(pgid int) ([]Socket, error) {
	cmd := exec.Command("/usr/sbin/lsof", "-a", "-iTCP", "-sTCP:LISTEN", "-g", strconv.Itoa(pgid), "-P", "-n", "-F", "tn")
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
		return nil, err // exit status 1 just means nothing matched
	}
	return parseLsof(out), nil
}

// parseLsof reads `lsof -F tn` output: "t" lines give IPv4 or IPv6 and "n"
// lines the address, such as "127.0.0.1:3000", "[::1]:5173" or "*:8000".
func parseLsof(out []byte) []Socket {
	var socks []Socket
	family := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			family = ""
		case 't':
			family = line[1:]
		case 'n':
			name := line[1:]
			i := strings.LastIndexByte(name, ':')
			if i < 0 {
				continue
			}
			port, err := strconv.Atoi(name[i+1:])
			if err != nil {
				continue
			}
			ip := strings.Trim(name[:i], "[]")
			if ip == "*" {
				ip = "0.0.0.0"
				if family == "IPv6" {
					ip = "::"
				}
			}
			socks = append(socks, Socket{IP: ip, Port: port})
		}
	}
	return socks
}

// ProcessStartTime identifies a process across PID reuse.
func ProcessStartTime(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	const zombie = 5 // SZOMB: exited, not yet reaped
	if kp.Proc.P_pid != int32(pid) || kp.Proc.P_stat == zombie {
		return "", unix.ESRCH
	}
	tv := kp.Proc.P_starttime
	return time.Unix(tv.Sec, int64(tv.Usec)*1000).UTC().Format(time.RFC3339Nano), nil
}

// ProcessUID returns the user ID a process runs as.
func ProcessUID(pid int) (int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if kp.Proc.P_pid != int32(pid) {
		return 0, unix.ESRCH
	}
	return int(kp.Eproc.Ucred.Uid), nil
}

// waitExit blocks until pid exits, using kqueue.
func waitExit(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	var ev unix.Kevent_t
	unix.SetKevent(&ev, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev.Fflags = unix.NOTE_EXIT
	events := make([]unix.Kevent_t, 1)
	for {
		_, err := unix.Kevent(kq, []unix.Kevent_t{ev}, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ESRCH) {
			return nil // already gone
		}
		return err
	}
}
