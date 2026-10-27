package portkit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

const (
	startTimeout = 60 * time.Second // how long a request waits for a port
	fastExit     = 5 * time.Second  // an exit sooner than this counts as a crash
	maxFastExits = 3
	stopGrace    = 10 * time.Second // SIGTERM, then SIGKILL
)

// sup starts, watches and stops on-demand processes. It is package-level
// and keyed by service name, so it outlives the app instance Caddy rebuilds
// on every config reload.
var sup = &supervisor{procs: map[string]*proc{}}

type supervisor struct {
	mu    sync.Mutex
	procs map[string]*proc
}

// proc is the supervisor's state for one on-demand service.
type proc struct {
	name string

	mu        sync.Mutex
	svc       *Service
	fp        string // what a restart depends on
	rec       string // the whole record
	cur       *run
	stale     bool // config changed: restart on the next request
	fastExits int
	crashed   bool
	lastErr   string
	inflight  int
	idle      *time.Timer

	tail *lineRing
	log  *logFile
}

// run is one execution of a service's command.
type run struct {
	cmd      *exec.Cmd
	pgid     int
	started  time.Time
	ports    map[string]int
	det      *Detector
	exited   chan struct{}
	stopping atomic.Bool
}

// CrashError is returned once a service exited quickly too many times.
type CrashError struct {
	Name    string
	Message string
	Tail    string
}

func (e *CrashError) Error() string { return e.Message }

func (s *supervisor) get(name string, svc *Service) *proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.procs[name]
	if p == nil {
		p = &proc{name: name, tail: newLineRing(tailLines)}
		p.update(svc)
		if lf, err := openLog(filepath.Join(LogDir(), name+".log")); err == nil {
			p.log = lf
		}
		s.procs[name] = p
	}
	return p
}

func (s *supervisor) lookup(name string) *proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[name]
}

// reconcile applies a new config: removed services are stopped, changed
// ones restart on their next request, and the rest keep running.
func (s *supervisor) reconcile(services map[string]*Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, p := range s.procs {
		svc := services[name]
		if svc == nil || svc.Mode != ModeOnDemand {
			delete(s.procs, name)
			go p.shutdown("unregistered")
			continue
		}
		p.mu.Lock()
		p.update(svc)
		p.mu.Unlock()
	}
}

// stopAll stops every process; Caddy is exiting.
func (s *supervisor) stopAll() {
	s.mu.Lock()
	procs := make([]*proc, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Go(func() { p.stop("Caddy is exiting") })
	}
	wg.Wait()
}

// update records a new config for the service. p.mu must be held, or p
// not yet shared.
func (p *proc) update(svc *Service) {
	fp, _ := json.Marshal([]any{svc.Command, svc.Cwd, svc.Env, svc.Subdomains, svc.ListenPort})
	rec, _ := json.Marshal(svc)
	if p.cur != nil && p.fp != "" && string(fp) != p.fp {
		p.stale = true
	}
	if string(rec) != p.rec {
		// A changed record, such as a new `portkit register`, gets a fresh start.
		p.crashed, p.fastExits, p.lastErr = false, 0, ""
	}
	p.svc, p.fp, p.rec = svc, string(fp), string(rec)
}

// Ensure starts the service if needed and waits until the port for sub
// ("" for the main port) is known.
func (s *supervisor) Ensure(ctx context.Context, name string, svc *Service, sub string) (Result, error) {
	p := s.get(name, svc)
	deadline := time.NewTimer(startTimeout)
	defer deadline.Stop()
	for {
		r, err := p.running()
		if err != nil {
			return Result{}, err
		}
		ready := r.det.Ready(sub)
		if ready == nil {
			return Result{}, fmt.Errorf("%s has no subdomain %q", name, sub)
		}
		select {
		case <-ready:
			if res, ok := r.det.Result(sub); ok {
				return res, nil
			}
		case <-r.exited:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-deadline.C:
			return Result{}, &CrashError{Name: name, Message: fmt.Sprintf("%s started but no port was detected within %s", name, startTimeout), Tail: p.tail.String()}
		}
	}
}

// running returns the current run, starting one if needed.
func (p *proc) running() (*run, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.crashed {
		return nil, &CrashError{Name: p.name, Message: p.lastErr, Tail: p.tail.String()}
	}
	if p.cur != nil && p.stale {
		old := p.cur
		p.cur = nil
		p.mu.Unlock()
		old.stop()
		p.mu.Lock()
	}
	p.stale = false
	if p.cur != nil {
		return p.cur, nil
	}
	r, err := p.start()
	if err != nil {
		p.lastErr = err.Error()
		p.logLine("portkit: " + err.Error())
		p.fastExits++
		if p.fastExits >= maxFastExits {
			p.crashed = true
		}
		return nil, &CrashError{Name: p.name, Message: err.Error(), Tail: p.tail.String()}
	}
	p.cur = r
	return r, nil
}

// start launches the command in its own process group. p.mu must be held.
func (p *proc) start() (*run, error) {
	svc := p.svc
	ports, err := AssignPorts(svc)
	if err != nil {
		return nil, err
	}
	argv := ExpandCommand(svc.Command, ports)
	env := ChildEnv(childBaseEnv(), p.name, svc, ports)
	dir := svc.Cwd
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	path, err := lookPath(argv[0], env, dir)
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{
		Path:        path,
		Args:        argv,
		Dir:         dir,
		Env:         env,
		Stdout:      pw,
		Stderr:      pw,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	p.logLine(fmt.Sprintf("portkit: starting %s on port %d: %s", p.name, ports[""], strings.Join(argv, " ")))
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("starting %s: %w", argv[0], err)
	}
	pw.Close()

	r := &run{
		cmd:     cmd,
		pgid:    cmd.Process.Pid,
		started: time.Now(),
		ports:   ports,
		det:     NewDetector(Targets(svc, ports), nil),
		exited:  make(chan struct{}),
	}
	activity.childStarted()
	children.add(r.pgid)
	caddy.Log().Named("portkit").Info("started",
		zap.String("service", p.name), zap.Int("pid", r.pgid), zap.Strings("command", argv))

	go p.readOutput(r, pr)
	go RunDetection(r.det, r.pgid, r.exited, func(target string, res Result) {
		host := p.name
		if target != "" {
			host = target + "." + p.name
		}
		p.logLine(DescribeResult(host, res))
		caddy.Log().Named("portkit").Info("port detected",
			zap.String("host", host+".localhost"), zap.String("addr", res.Addr), zap.String("reason", res.Reason))
	})
	go p.wait(r)
	return r, nil
}

func (p *proc) readOutput(r *run, pr *os.File) {
	defer pr.Close()
	br := bufio.NewReaderSize(pr, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if p.log != nil {
				_, _ = io.WriteString(p.log, line)
			}
			text := strings.TrimRight(line, "\r\n")
			p.tail.add(StripANSI(text))
			r.det.AddLine(text)
		}
		if err != nil {
			return
		}
	}
}

func (p *proc) wait(r *run) {
	err := r.cmd.Wait()
	// Clean up anything the leader left behind in its group.
	_ = syscall.Kill(-r.pgid, syscall.SIGTERM)
	ran := time.Since(r.started)
	msg := fmt.Sprintf("%s exited after %s", p.name, ran.Round(time.Millisecond))
	if err != nil {
		msg += ": " + err.Error()
	}

	p.mu.Lock()
	if p.cur == r {
		p.cur = nil
	}
	if !r.stopping.Load() {
		if ran < fastExit {
			p.fastExits++
			if p.fastExits >= maxFastExits {
				p.crashed = true
				msg += fmt.Sprintf("; it exited within %s %d times in a row, so portkit stopped restarting it", fastExit, maxFastExits)
			}
		} else {
			p.fastExits = 0
		}
		p.lastErr = msg
	}
	p.mu.Unlock()

	p.logLine("portkit: " + msg)
	caddy.Log().Named("portkit").Info("exited", zap.String("service", p.name), zap.Int("pid", r.pgid), zap.NamedError("status", err))
	children.remove(r.pgid)
	activity.childExited()
	close(r.exited) // last: stopAll returns, and Caddy may exit, once this closes
}

// stop sends SIGTERM to the process group, then SIGKILL after stopGrace.
func (r *run) stop() {
	r.stopping.Store(true)
	_ = syscall.Kill(-r.pgid, syscall.SIGTERM)
	select {
	case <-r.exited:
	case <-time.After(stopGrace):
		_ = syscall.Kill(-r.pgid, syscall.SIGKILL)
		<-r.exited
	}
}

// stop stops the current run, if any, and clears the crash state. It
// reports whether a process was running and whether a crash was cleared.
func (p *proc) stop(why string) (stopped, reset bool) {
	p.mu.Lock()
	r := p.cur
	p.cur = nil
	reset = p.crashed
	p.crashed, p.fastExits, p.lastErr = false, 0, ""
	if p.idle != nil {
		p.idle.Stop()
	}
	p.mu.Unlock()
	if r == nil {
		return false, reset
	}
	p.logLine("portkit: stopping " + p.name + " (" + why + ")")
	r.stop()
	return true, reset
}

// shutdown stops the process and closes the log; the service is gone.
func (p *proc) shutdown(why string) {
	p.stop(why)
	if p.log != nil {
		p.log.Close()
	}
}

// Begin marks a request to the service as in flight; the returned func ends
// it. When nothing is in flight for idle_timeout, the process is stopped.
func (s *supervisor) Begin(name string, svc *Service) func() {
	p := s.get(name, svc)
	p.mu.Lock()
	p.inflight++
	if p.idle != nil {
		p.idle.Stop()
		p.idle = nil
	}
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.inflight--
		if p.inflight > 0 || p.svc.idleTimeout <= 0 {
			return
		}
		var t *time.Timer
		t = time.AfterFunc(p.svc.idleTimeout, func() {
			p.mu.Lock()
			idle := p.idle == t && p.inflight == 0 && p.cur != nil
			p.mu.Unlock()
			if idle {
				p.stop(fmt.Sprintf("idle for %s", p.svc.idleTimeout))
			}
		})
		p.idle = t
	}
}

// Redetect forgets the detected port for sub, after dials to it failed.
func (s *supervisor) Redetect(name, sub string) {
	p := s.lookup(name)
	if p == nil {
		return
	}
	p.mu.Lock()
	r := p.cur
	p.mu.Unlock()
	if r == nil {
		return
	}
	if res, ok := r.det.Result(sub); ok {
		if _, alive := probePort(res.Port); !alive {
			r.det.Reset(sub)
		}
	}
}

// Stop stops a running service and clears a crash; the next request
// starts it again.
func (s *supervisor) Stop(name string) (stopped, reset bool) {
	p := s.lookup(name)
	if p == nil {
		return false, false
	}
	return p.stop("portkit stop")
}

func (p *proc) logLine(line string) {
	p.tail.add(line)
	if p.log != nil {
		_, _ = io.WriteString(p.log, line+"\n")
	}
}

// ProcState is the runtime state of an on-demand service.
type ProcState struct {
	State      string            `json:"state"` // "running", "starting", "stopped" or "crashed"
	PID        int               `json:"pid,omitempty"`
	Addr       string            `json:"addr,omitempty"`
	Subdomains map[string]string `json:"subdomains,omitempty"`
	Started    *time.Time        `json:"started,omitempty"`
	Error      string            `json:"error,omitempty"`
}

func (s *supervisor) State(name string) ProcState {
	p := s.lookup(name)
	if p == nil {
		return ProcState{State: "stopped"}
	}
	p.mu.Lock()
	r, crashed, lastErr := p.cur, p.crashed, p.lastErr
	p.mu.Unlock()
	if crashed {
		return ProcState{State: "crashed", Error: lastErr}
	}
	if r == nil {
		return ProcState{State: "stopped", Error: lastErr}
	}
	st := ProcState{State: "starting", PID: r.pgid, Started: &r.started}
	if res, ok := r.det.Result(""); ok {
		st.State, st.Addr = "running", res.Addr
	}
	for sub := range p.svc.Subdomains {
		if res, ok := r.det.Result(sub); ok {
			if st.Subdomains == nil {
				st.Subdomains = map[string]string{}
			}
			st.Subdomains[sub] = res.Addr
		}
	}
	return st
}

// childBaseEnv is Caddy's environment minus what belongs to portkit itself.
func childBaseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "XDG_DATA_HOME", "XDG_CONFIG_HOME", "LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES", "NOTIFY_SOCKET", "PORTKIT_SOCKETS":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// children records running process groups in a file, so a Caddy that was
// killed without cleaning up can stop the orphans on its next start.
var children = &childFile{}

type childFile struct {
	mu    sync.Mutex
	once  sync.Once
	pgids map[int]string // pgid -> start time
}

func (c *childFile) path() string { return filepath.Join(Home(), "children.json") }

// reapOrphans stops process groups left by a previous Caddy.
func (c *childFile) reapOrphans() {
	c.once.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.pgids = map[int]string{}
		data, err := os.ReadFile(c.path())
		if err != nil {
			return
		}
		var old map[int]string
		if json.Unmarshal(data, &old) != nil {
			return
		}
		for pgid, started := range old {
			if s, err := ProcessStartTime(pgid); err == nil && s == started {
				caddy.Log().Named("portkit").Info("stopping orphaned process group", zap.Int("pgid", pgid))
				_ = syscall.Kill(-pgid, syscall.SIGTERM)
			}
		}
		c.save()
	})
}

func (c *childFile) add(pgid int) {
	started, err := ProcessStartTime(pgid)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pgids == nil {
		c.pgids = map[int]string{}
	}
	c.pgids[pgid] = started
	c.save()
}

func (c *childFile) remove(pgid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pgids, pgid)
	c.save()
}

// save writes the file. c.mu must be held.
func (c *childFile) save() {
	data, _ := json.Marshal(c.pgids)
	tmp := c.path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, c.path())
	}
}

var errNoService = errors.New("no such service")
