package portkit

import (
	"regexp"
	"strings"
	"testing"
)

// Startup output captured from real dev servers, ANSI codes included.
const (
	viteOutput = "\n  \x1b[32m\x1b[1mVITE\x1b[22m v5.4.2\x1b[39m  \x1b[2mready in \x1b[0m\x1b[1m312\x1b[22m\x1b[2m\x1b[0m ms\x1b[22m\n\n" +
		"  \x1b[32m➜\x1b[39m  \x1b[1mLocal\x1b[22m:   \x1b[36mhttp://localhost:\x1b[1m5174\x1b[22m/\x1b[39m\n" +
		"  \x1b[32m➜\x1b[39m  \x1b[1mNetwork\x1b[22m\x1b[2m: use \x1b[22m\x1b[1m--host\x1b[22m\x1b[2m to expose\x1b[22m\n" +
		"  \x1b[32m➜\x1b[39m\x1b[2m  press \x1b[22m\x1b[1mh + enter\x1b[22m\x1b[2m to show help\x1b[22m\n"
	viteInUse    = "Port 5173 is in use, trying another one...\n"
	nextOutput   = "  \x1b[35m\x1b[1m▲ Next.js 14.2.3\x1b[22m\x1b[39m\n  - Local:        http://localhost:3000\n\n \x1b[32m\x1b[1m✓\x1b[22m\x1b[39m Starting...\n \x1b[32m\x1b[1m✓\x1b[22m\x1b[39m Ready in 1514ms\n"
	djangoOutput = "Watching for file changes with StatReloader\nPerforming system checks...\n\nSystem check identified no issues (0 silenced).\n" +
		"October 26, 2026 - 14:02:11\nDjango version 5.0.6, using settings 'mysite.settings'\nStarting development server at http://127.0.0.1:8000/\nQuit the server with CONTROL-C.\n"
	railsOutput = "=> Booting Puma\n=> Rails 7.1.3 application starting in development \n=> Run `bin/rails server --help` for more startup options\n" +
		"Puma starting in single mode...\n* Puma version: 6.4.2 (ruby 3.3.0-p0) (\"The Eagle of Durango\")\n*  Min threads: 5\n*  Max threads: 5\n" +
		"*  Environment: development\n*          PID: 41234\n* Listening on http://127.0.0.1:3000\n* Listening on http://[::1]:3000\nUse Ctrl-C to stop\n"
	inspectOutput = "Debugger listening on ws://127.0.0.1:9229/4b1c3a5e-8f2d-4c9a-b7e1-2d3f4a5b6c7d\nFor help, see: https://nodejs.org/en/docs/inspector\nServer listening on port 4000\n"
)

func TestPortFromLine(t *testing.T) {
	tests := []struct {
		line string
		port int
	}{
		{"  ➜  Local:   http://localhost:5174/", 5174},
		{"Starting development server at http://127.0.0.1:8000/", 8000},
		{"* Listening on http://[::1]:3000", 3000},
		{"Server listening on port 4000", 4000},
		{"listening on :8080", 8080},
		{"App running, port=3001", 3001},
		{"Debugger listening on ws://127.0.0.1:9229/abc", 9229},
		{"Port 5173 is in use, trying another one...", 0},
		{"Error: listen EADDRINUSE: address already in use :::3000", 0},
		{"Ready in 1514ms", 0},
		{"*          PID: 41234", 0},
	}
	for _, tt := range tests {
		got, _ := PortFromLine(StripANSI(tt.line))
		if got != tt.port {
			t.Errorf("PortFromLine(%q) = %d, want %d", tt.line, got, tt.port)
		}
	}
}

func TestStripANSI(t *testing.T) {
	in := "\x1b[36mhttp://localhost:\x1b[1m5174\x1b[22m/\x1b[39m \x1b]8;;http://x\x07link\x1b]8;;\x07"
	if got, want := StripANSI(in), "http://localhost:5174/ link"; got != want {
		t.Errorf("StripANSI = %q, want %q", got, want)
	}
}

// detect feeds output to a detector and steps it with fixed sockets until the
// main port resolves, as RunDetection does every 250 ms.
func detect(t *testing.T, targets []Target, output string, socks []Socket, accepts func(int) (string, bool)) map[string]Result {
	t.Helper()
	if accepts == nil {
		accepts = func(int) (string, bool) { return "", false }
	}
	d := NewDetector(targets, accepts)
	for line := range strings.SplitSeq(output, "\n") {
		d.AddLine(line)
	}
	for range 10 {
		d.Step(socks)
	}
	out := map[string]Result{}
	for _, tg := range targets {
		if r, ok := d.Result(tg.Name); ok {
			out[tg.Name] = r
		}
	}
	return out
}

func TestDetectServers(t *testing.T) {
	tests := []struct {
		name     string
		assigned int
		output   string
		socks    []Socket
		addr     string
		reason   string
	}{
		{"next uses PORT", 3000, nextOutput, []Socket{{"::", 3000}}, "[::1]:3000", "assigned port"},
		{"vite ignores PORT", 41000, viteInUse + viteOutput, []Socket{{"::1", 5174}, {"127.0.0.1", 24678}}, "[::1]:5174", "from log"},
		{"django", 41000, djangoOutput, []Socket{{"127.0.0.1", 8000}}, "127.0.0.1:8000", "from log"},
		{"rails on both families", 41000, railsOutput, []Socket{{"::1", 3000}, {"127.0.0.1", 3000}}, "127.0.0.1:3000", "from log"},
		{"node --inspect", 41000, inspectOutput, []Socket{{"127.0.0.1", 9229}, {"0.0.0.0", 4000}}, "127.0.0.1:4000", "from log"},
		{"single socket, quiet server", 41000, "ready\n", []Socket{{"0.0.0.0", 7000}}, "127.0.0.1:7000", "only listening port"},
		{"guess drops the debugger port", 41000, "", []Socket{{"127.0.0.1", 9229}, {"127.0.0.1", 8100}, {"127.0.0.1", 8200}}, "127.0.0.1:8100", "guessed lowest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := detect(t, []Target{{Name: "", Assigned: tt.assigned}}, tt.output, tt.socks, nil)
			r, ok := res[""]
			if !ok {
				t.Fatal("main port not resolved")
			}
			if r.Addr != tt.addr || !strings.HasPrefix(r.Reason, tt.reason) {
				t.Errorf("got %s (%s), want %s (%s…)", r.Addr, r.Reason, tt.addr, tt.reason)
			}
		})
	}
}

func TestDetectWaitsToSettle(t *testing.T) {
	d := NewDetector([]Target{{Name: "", Assigned: 41000}}, func(int) (string, bool) { return "", false })
	// A debugger port opens first; the guess must hold for settleSteps rounds.
	socks := []Socket{{"127.0.0.1", 9229}}
	for range settleSteps - 1 {
		d.Step(socks)
	}
	socks = append(socks, Socket{"127.0.0.1", 4000})
	d.AddLine("Server listening on port 4000")
	d.Step(socks)
	r, ok := d.Result("")
	if !ok || r.Port != 4000 {
		t.Fatalf("got %+v, want port 4000", r)
	}
}

func TestDetectOutsideProcessGroup(t *testing.T) {
	// docker compose logs a port that belongs to another process.
	accepts := func(p int) (string, bool) { return "127.0.0.1:8080", p == 8080 }
	res := detect(t, []Target{{Name: "", Assigned: 41000}}, "web-1  | Listening at http://0.0.0.0:8080\n", nil, accepts)
	if r := res[""]; r.Port != 8080 || !strings.Contains(r.Reason, "outside the process group") {
		t.Errorf("got %+v", r)
	}
}

func TestDetectListenPort(t *testing.T) {
	res := detect(t, []Target{{Name: "", Assigned: 41000, Fixed: 5000}}, "Server listening on port 6000\n", []Socket{{"127.0.0.1", 5000}, {"127.0.0.1", 6000}}, nil)
	if r := res[""]; r.Port != 5000 || r.Reason != "--listen-port" {
		t.Errorf("got %+v", r)
	}
}

func TestDetectSubdomains(t *testing.T) {
	targets := []Target{
		{Name: "", Assigned: 41000},
		{Name: "api", Assigned: 41001},
		{Name: "grpc", Match: regexp.MustCompile(`GraphQL server ready`)},
	}
	output := "GraphQL server ready at :4001\nListening on http://localhost:41000\n"
	socks := []Socket{{"127.0.0.1", 41000}, {"127.0.0.1", 41001}, {"127.0.0.1", 4001}}
	res := detect(t, targets, output, socks, nil)
	want := map[string]string{"": "127.0.0.1:41000", "api": "127.0.0.1:41001", "grpc": "127.0.0.1:4001"}
	for name, addr := range want {
		if res[name].Addr != addr {
			t.Errorf("%q: got %+v, want %s", name, res[name], addr)
		}
	}
	if !strings.Contains(res["grpc"].Reason, "GraphQL server ready at :4001") {
		t.Errorf("grpc reason = %q", res["grpc"].Reason)
	}
}

func TestDetectSubdomainPortsAreSkippedForMain(t *testing.T) {
	// The main port isn't assigned and the only other socket is the api port.
	targets := []Target{{Name: ""}, {Name: "api", Assigned: 41001}}
	socks := []Socket{{"127.0.0.1", 41001}, {"127.0.0.1", 7000}}
	res := detect(t, targets, "", socks, nil)
	if res[""].Port != 7000 {
		t.Errorf("main = %+v, want 7000", res[""])
	}
}

func TestDetectReset(t *testing.T) {
	d := NewDetector([]Target{{Name: "", Assigned: 41000}}, nil)
	d.Step([]Socket{{"127.0.0.1", 41000}})
	ready := d.Ready("")
	select {
	case <-ready:
	default:
		t.Fatal("not ready")
	}
	d.Reset("")
	if d.Done() {
		t.Fatal("still done after Reset")
	}
	d.Step([]Socket{{"::1", 41000}})
	if r, _ := d.Result(""); r.Addr != "[::1]:41000" {
		t.Errorf("after reset: %+v", r)
	}
}

func TestDialAddr(t *testing.T) {
	tests := []struct {
		socks []Socket
		want  string
	}{
		{[]Socket{{"::", 80}}, "[::1]:80"},
		{[]Socket{{"0.0.0.0", 80}}, "127.0.0.1:80"},
		{[]Socket{{"::1", 80}, {"127.0.0.1", 80}}, "127.0.0.1:80"},
		{[]Socket{{"::", 80}, {"::1", 80}}, "[::1]:80"},
		{[]Socket{{"192.168.1.5", 80}}, "192.168.1.5:80"},
	}
	for _, tt := range tests {
		if got, _ := dialAddr(tt.socks, 80); got != tt.want {
			t.Errorf("dialAddr(%v) = %s, want %s", tt.socks, got, tt.want)
		}
	}
	if _, ok := dialAddr([]Socket{{"127.0.0.1", 81}}, 80); ok {
		t.Error("matched the wrong port")
	}
}
