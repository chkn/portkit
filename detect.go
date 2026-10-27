package portkit

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Port detection trusts the listening sockets the OS reports for a process
// group, and uses log lines to choose between candidates.

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*(\x07|\x1b\\)`)
	// In priority order; the last group is the port.
	portPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bhttps?://(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1?\]|[a-z0-9.-]+):(\d{2,5})\b`),
		regexp.MustCompile(`(?i)\blisten(ing)?\b.*?(port\s*|:)(\d{2,5})\b`),
		regexp.MustCompile(`(?i)\bport\s*[:=]?\s*(\d{2,5})\b`),
	}
	inUseRe = regexp.MustCompile(`(?i)in use|EADDRINUSE`)
	// For a line a subdomain's regex picked out: the last ":4001" or bare number.
	anyPortRe = regexp.MustCompile(`(?:^|[\s:])(\d{2,5})\b`)
)

// knownSidePorts are dropped when guessing between several sockets.
var knownSidePorts = []int{9229} // Node debugger

// StripANSI removes terminal escape sequences.
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// PortFromLine returns the port a log line names, if any. The line should
// already have ANSI escapes stripped.
func PortFromLine(line string) (int, bool) {
	if inUseRe.MatchString(line) {
		return 0, false
	}
	for _, re := range portPatterns {
		if m := re.FindStringSubmatch(line); m != nil {
			if p, err := strconv.Atoi(m[len(m)-1]); err == nil && p > 0 && p <= 65535 {
				return p, true
			}
		}
	}
	return 0, false
}

// Socket is one TCP socket in LISTEN state.
type Socket struct {
	IP   string
	Port int
}

// dialAddr picks the address to dial for port among the listening sockets:
// a loopback address when there is one, so a Node server bound only to ::1
// is dialed on ::1.
func dialAddr(socks []Socket, port int) (string, bool) {
	best, rank := "", 99
	for _, s := range socks {
		if s.Port != port {
			continue
		}
		var host string
		var r int
		switch s.IP {
		case "127.0.0.1":
			host, r = "127.0.0.1", 0
		case "::1":
			host, r = "::1", 1
		case "0.0.0.0", "":
			host, r = "127.0.0.1", 2
		case "::":
			host, r = "::1", 3
		default:
			host, r = s.IP, 4
		}
		if r < rank {
			best, rank = host, r
		}
	}
	if rank == 99 {
		return "", false
	}
	return net.JoinHostPort(best, strconv.Itoa(port)), true
}

// probePort returns the loopback address on which port accepts connections.
func probePort(port int) (string, bool) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return addr, true
		}
	}
	return "", false
}

// Target is one port to find: the main port ("") or a subdomain's.
type Target struct {
	Name     string
	Assigned int            // assigned through PORT, {port} or {<sub>}
	Fixed    int            // known in advance: --listen-port or --sub NAME=PORT
	Match    *regexp.Regexp // --sub NAME=/regex/
}

// Result is a detected port and why it was chosen.
type Result struct {
	Addr   string // host:port to dial
	Port   int
	Reason string
}

type logLine struct {
	text string
	port int // 0 if the line names no port
}

// Detector resolves targets from snapshots of listening sockets and the
// lines a process prints. Step is called every 250 ms until every target is
// resolved; it doesn't touch the OS itself, so it can be tested.
type Detector struct {
	targets []Target
	accepts func(port int) (string, bool)

	mu      sync.Mutex
	lines   []logLine // recent lines that name a port
	matched map[string]logLine
	results map[string]Result
	pending map[string]Result // candidate seen on the previous step
	seen    map[string]int    // consecutive steps it was seen
	ready   map[string]chan struct{}
}

// settleSteps is how many consecutive steps a guess must hold before it is
// used, so a debugger port that opens first doesn't win the race.
const settleSteps = 3

const maxLogLines = 200

// NewDetector makes a detector. accepts reports whether a port outside the
// process group takes connections; nil uses probePort.
func NewDetector(targets []Target, accepts func(int) (string, bool)) *Detector {
	if accepts == nil {
		accepts = probePort
	}
	d := &Detector{
		targets: targets,
		accepts: accepts,
		matched: map[string]logLine{},
		results: map[string]Result{},
		pending: map[string]Result{},
		seen:    map[string]int{},
		ready:   map[string]chan struct{}{},
	}
	for _, t := range targets {
		d.ready[t.Name] = make(chan struct{})
	}
	return d
}

// AddLine records an output line.
func (d *Detector) AddLine(raw string) {
	text := strings.TrimRight(StripANSI(raw), "\r\n")
	port, _ := PortFromLine(text)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.targets {
		if t.Match == nil {
			continue
		}
		if _, done := d.matched[t.Name]; done {
			continue
		}
		if m := t.Match.FindStringSubmatch(text); m != nil {
			p := port
			if p == 0 {
				if all := anyPortRe.FindAllStringSubmatch(text, -1); len(all) > 0 {
					p, _ = strconv.Atoi(all[len(all)-1][1])
				}
			}
			if len(m) > 1 {
				if n, err := strconv.Atoi(m[1]); err == nil {
					p = n
				}
			}
			d.matched[t.Name] = logLine{text: text, port: p}
		}
	}
	if port == 0 {
		return
	}
	d.lines = append(d.lines, logLine{text: text, port: port})
	if len(d.lines) > maxLogLines {
		d.lines = d.lines[len(d.lines)-maxLogLines:]
	}
}

// Ready is closed once the target's port is known.
func (d *Detector) Ready(target string) <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ready[target]
}

// Result returns the detected port for a target.
func (d *Detector) Result(target string) (Result, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.results[target]
	return r, ok
}

// Done reports whether every target is resolved.
func (d *Detector) Done() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.results) == len(d.targets)
}

// Reset forgets a target's result so detection runs again, for example
// after dials to it start failing.
func (d *Detector) Reset(target string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.results[target]; ok {
		delete(d.results, target)
		d.ready[target] = make(chan struct{})
	}
}

// Resolved is a target resolved by Step.
type Resolved struct {
	Target string
	Result
}

// Step runs one round of detection against the current sockets and returns
// the targets newly resolved in this round.
func (d *Detector) Step(socks []Socket) []Resolved {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Resolved
	resolve := func(name string, r Result) {
		d.results[name] = r
		close(d.ready[name])
		delete(d.pending, name)
		out = append(out, Resolved{Target: name, Result: r})
	}

	// Subdomain ports are resolved first, and the main-port steps skip them.
	claimed := map[int]bool{}
	for _, t := range d.targets {
		if t.Name == "" {
			continue
		}
		if r, ok := d.results[t.Name]; ok {
			claimed[r.Port] = true
			continue
		}
		if t.Assigned != 0 {
			claimed[t.Assigned] = true
		}
		if t.Fixed != 0 {
			claimed[t.Fixed] = true
		}
		if r, ok := d.resolveSub(t, socks); ok {
			claimed[r.Port] = true
			resolve(t.Name, r)
		}
	}

	for _, t := range d.targets {
		if t.Name != "" {
			continue
		}
		if _, ok := d.results[""]; ok {
			break
		}
		r, sure, ok := d.resolveMain(t, socks, claimed)
		if !ok {
			delete(d.pending, "")
			d.seen[""] = 0
			break
		}
		if sure {
			resolve("", r)
			break
		}
		if prev, had := d.pending[""]; had && prev.Addr == r.Addr {
			d.seen[""]++
		} else {
			d.pending[""] = r
			d.seen[""] = 1
		}
		if d.seen[""] >= settleSteps {
			resolve("", r)
		}
	}
	return out
}

func (d *Detector) resolveSub(t Target, socks []Socket) (Result, bool) {
	switch {
	case t.Fixed != 0:
		if addr, ok := dialAddr(socks, t.Fixed); ok {
			return Result{Addr: addr, Port: t.Fixed, Reason: "fixed port"}, true
		}
		if addr, ok := d.accepts(t.Fixed); ok {
			return Result{Addr: addr, Port: t.Fixed, Reason: "fixed port"}, true
		}
	case t.Assigned != 0:
		if addr, ok := dialAddr(socks, t.Assigned); ok {
			return Result{Addr: addr, Port: t.Assigned, Reason: "assigned port"}, true
		}
	case t.Match != nil:
		m, ok := d.matched[t.Name]
		if !ok || m.port == 0 {
			return Result{}, false
		}
		reason := fmt.Sprintf("from log: %q", m.text)
		if addr, ok := dialAddr(socks, m.port); ok {
			return Result{Addr: addr, Port: m.port, Reason: reason}, true
		}
		if addr, ok := d.accepts(m.port); ok {
			return Result{Addr: addr, Port: m.port, Reason: reason}, true
		}
	}
	return Result{}, false
}

// resolveMain applies the main-port steps in order. sure is false for
// guesses that must hold for settleSteps rounds.
func (d *Detector) resolveMain(t Target, socks []Socket, claimed map[int]bool) (r Result, sure, ok bool) {
	// --listen-port: the port is known.
	if t.Fixed != 0 {
		if addr, ok := dialAddr(socks, t.Fixed); ok {
			return Result{Addr: addr, Port: t.Fixed, Reason: "--listen-port"}, true, true
		}
		if addr, ok := d.accepts(t.Fixed); ok {
			return Result{Addr: addr, Port: t.Fixed, Reason: "--listen-port"}, true, true
		}
		return Result{}, false, false
	}

	// 1. The assigned port.
	if t.Assigned != 0 {
		if addr, ok := dialAddr(socks, t.Assigned); ok {
			return Result{Addr: addr, Port: t.Assigned, Reason: "assigned port"}, true, true
		}
	}

	// 2. Listening sockets owned by the process group.
	var ports []int
	for _, s := range socks {
		if !claimed[s.Port] && !slices.Contains(ports, s.Port) {
			ports = append(ports, s.Port)
		}
	}
	slices.Sort(ports)

	// 3. The most recent log line naming one of those ports.
	for i := len(d.lines) - 1; i >= 0; i-- {
		l := d.lines[i]
		if slices.Contains(ports, l.port) {
			addr, _ := dialAddr(socks, l.port)
			return Result{Addr: addr, Port: l.port, Reason: fmt.Sprintf("from log: %q", l.text)}, true, true
		}
	}

	// 4. Exactly one listening port.
	if len(ports) == 1 {
		addr, _ := dialAddr(socks, ports[0])
		return Result{Addr: addr, Port: ports[0], Reason: "only listening port"}, false, true
	}

	// 5. A logged port that another process owns, e.g. docker compose.
	for i := len(d.lines) - 1; i >= 0; i-- {
		l := d.lines[i]
		if claimed[l.port] {
			continue
		}
		if addr, ok := d.accepts(l.port); ok {
			return Result{Addr: addr, Port: l.port, Reason: fmt.Sprintf("from log, outside the process group: %q", l.text)}, false, true
		}
		break
	}

	// 6. Guess: drop known side ports and use the lowest remaining one.
	if len(ports) > 1 {
		rest := slices.DeleteFunc(slices.Clone(ports), func(p int) bool { return slices.Contains(knownSidePorts, p) })
		if len(rest) == 0 {
			rest = ports
		}
		addr, _ := dialAddr(socks, rest[0])
		var all []string
		for _, p := range ports {
			all = append(all, strconv.Itoa(p))
		}
		reason := fmt.Sprintf("guessed lowest of %s; use {port} or --listen-port to fix", strings.Join(all, ", "))
		if len(rest) == 1 {
			reason = fmt.Sprintf("guessed %d from %s; use {port} or --listen-port to fix", rest[0], strings.Join(all, ", "))
		}
		return Result{Addr: addr, Port: rest[0], Reason: reason}, false, true
	}
	return Result{}, false, false
}

// DescribeResult formats the line portkit writes once a port is chosen, e.g.
// "portkit: foo.localhost → 127.0.0.1:3000 (assigned port)".
func DescribeResult(host string, r Result) string {
	return fmt.Sprintf("portkit: %s.localhost → %s (%s)", host, r.Addr, r.Reason)
}

// RunDetection steps d every 250 ms with the sockets of process group pgid
// until every target resolves or stop closes. report is called for each
// result as it resolves.
func RunDetection(d *Detector, pgid int, stop <-chan struct{}, report func(target string, r Result)) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if !d.Done() {
			socks, _ := ListeningSockets(pgid)
			for _, r := range d.Step(socks) {
				report(r.Target, r.Result)
			}
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}
