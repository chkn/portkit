package portkit

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// runForeground runs the command in a pseudo-terminal, registers a
// "foreground" record once its port is detected, and removes the record
// when the command exits. It returns the command's exit status.
func runForeground(name string, s *Service, replace bool) (int, error) {
	// Check the command and subdomains as for an on-demand service; the
	// port isn't known yet.
	check := *s
	check.Mode = ModeOnDemand
	if err := check.provision(name); err != nil {
		return 0, err
	}
	prev, err := getService(name)
	if err != nil {
		return 0, err
	}
	var restore *Service // the record to put back on exit
	if prev != nil {
		switch {
		case prev.Owner != nil && !prev.Owner.alive():
			restore = prev.Replaces // a stale record from a run that was killed
		case replace:
			restore = prev
			if prev.Owner != nil {
				restore = prev.Replaces
			}
		case prev.Mode == ModeForeground:
			return 0, fmt.Errorf("%s is already running with `portkit run` (process %d)", name, prev.Owner.PID)
		default:
			return 0, fmt.Errorf("%s is already registered as a %s service; pass --replace to take it over until this command exits", name, modeLabel(prev.Mode))
		}
	}

	ports, err := AssignPorts(s)
	if err != nil {
		return 0, err
	}
	argv := ExpandCommand(s.Command, ports)
	env := ChildEnv(os.Environ(), name, s, ports)
	path, err := lookPath(argv[0], env, s.Cwd)
	if err != nil {
		return 0, err
	}
	owner, err := OwnerOf(os.Getpid())
	if err != nil {
		return 0, err
	}

	cmd := &exec.Cmd{Path: path, Args: argv, Dir: s.Cwd, Env: env}
	ptmx, err := pty.Start(cmd) // the child leads its own session and process group
	if err != nil {
		return 0, fmt.Errorf("starting %s: %w", argv[0], err)
	}
	defer ptmx.Close()
	pgid := cmd.Process.Pid

	// Pass the terminal through: raw input, window size and signals.
	stdinFd := int(os.Stdin.Fd())
	raw := term.IsTerminal(stdinFd)
	if raw {
		if old, err := term.MakeRaw(stdinFd); err == nil {
			defer term.Restore(stdinFd, old)
		}
		_ = pty.InheritSize(os.Stdin, ptmx)
	}
	say := func(line string) {
		if raw {
			line = strings.ReplaceAll(line, "\n", "\r\n")
		}
		fmt.Fprint(os.Stderr, line)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	go func() {
		forwarded := 0
		for sig := range sigs {
			if sig == syscall.SIGWINCH {
				if raw {
					_ = pty.InheritSize(os.Stdin, ptmx)
				}
				continue
			}
			forwarded++
			if forwarded > 2 {
				sig = syscall.SIGKILL // a third signal means stop now
			}
			_ = syscall.Kill(-pgid, sig.(syscall.Signal))
		}
	}()
	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()

	det := NewDetector(Targets(s, ports), nil)
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		copyLines(ptmx, os.Stdout, det.AddLine)
	}()

	exited := make(chan struct{})
	var mu sync.Mutex
	registered := false
	rec := &Service{
		Mode: ModeForeground, Cwd: s.Cwd, Labels: s.Labels, HostHeader: s.HostHeader,
		Owner: owner, Replaces: restore,
	}
	go RunDetection(det, pgid, exited, func(target string, res Result) {
		mu.Lock()
		defer mu.Unlock()
		host := name
		if target == "" {
			rec.Port = res.Port
		} else {
			host = target + "." + name
			if rec.Subdomains == nil {
				rec.Subdomains = map[string]*Subdomain{}
			}
			rec.Subdomains[target] = &Subdomain{Port: res.Port}
		}
		say(DescribeResult(host, res) + "\n")
		if rec.Port == 0 {
			return // subdomains wait for the main port
		}
		if err := call(http.MethodPost, servicePath(name), rec, nil); err != nil {
			say("portkit: registering " + name + ": " + err.Error() + "\n")
			return
		}
		registered = true
	})

	werr := cmd.Wait()
	close(exited)
	select {
	case <-outputDone:
	case <-time.After(time.Second): // a grandchild may still hold the terminal
	}

	mu.Lock()
	defer mu.Unlock()
	if registered {
		if err := removeOwned(admin(), name, owner); err != nil {
			say("portkit: removing " + name + ": " + err.Error() + "\n")
		}
	}
	return exitStatus(werr), nil
}

// copyLines copies r to w and calls line for each complete line.
func copyLines(r io.Reader, w io.Writer, line func(string)) {
	br := bufio.NewReaderSize(r, 32<<10)
	var partial strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = w.Write(chunk)
			for len(chunk) > 0 {
				i := strings.IndexByte(string(chunk), '\n')
				if i < 0 {
					partial.Write(chunk)
					break
				}
				partial.Write(chunk[:i])
				line(partial.String())
				partial.Reset()
				chunk = chunk[i+1:]
			}
		}
		if err != nil {
			// A pty returns EIO once the child side closes.
			if partial.Len() > 0 {
				line(partial.String())
			}
			return
		}
	}
}

// exitStatus maps a Wait error to a shell-style exit status.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}
