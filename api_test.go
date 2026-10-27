package portkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func liveOwner(t *testing.T) *Owner {
	t.Helper()
	o, err := OwnerOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestDiscover(t *testing.T) {
	services := map[string]*Service{
		"repo.evalution": {Mode: ModeOnDemand, Command: []string{"x"}, Cwd: "/src/repo", Labels: map[string]string{"kind": "evalution"}},
		"app.evalution":  {Mode: ModeOnDemand, Command: []string{"x"}, Cwd: "/src/repo/app", Labels: map[string]string{"kind": "evalution"}},
		"other":          {Mode: ModeOnDemand, Command: []string{"x"}, Cwd: "/src/repo/app", Labels: map[string]string{"kind": "web"}},
		"elsewhere":      {Mode: ModeOnDemand, Command: []string{"x"}, Cwd: "/src/repository", Labels: map[string]string{"kind": "evalution"}},
		"gone":           {Mode: ModeAdopted, Port: 1, Cwd: "/src/repo", Labels: map[string]string{"kind": "evalution"}, Owner: &Owner{PID: 999999, Started: "x"}},
	}
	list, err := discover(services, []string{"kind=evalution"}, "/src/repo/app/pkg")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range list {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "app.evalution,repo.evalution" {
		t.Errorf("discover = %s; want deepest first, without other labels, siblings or dead owners", got)
	}
	data, _ := json.Marshal(list[0])
	if strings.Contains(string(data), "command") {
		t.Errorf("command leaked: %s", data)
	}
	if _, err := discover(services, []string{"kind"}, ""); err == nil {
		t.Error("bad label filter accepted")
	}
}

func TestNameLimiter(t *testing.T) {
	l := &nameLimiter{allowed: map[string]bool{}}
	now := time.Now()
	for i := range newNamesPerMinute {
		if err := l.allow(string(rune('a'+i)), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.allow("z", now); err == nil {
		t.Error("11th new name in a minute allowed")
	}
	if err := l.allow("a", now); err != nil {
		t.Error("a name seen before was refused")
	}
	if err := l.allow("z", now.Add(61*time.Second)); err != nil {
		t.Errorf("refused after a minute: %v", err)
	}
}

func TestPlanAdopt(t *testing.T) {
	owner := liveOwner(t)
	stopped := func(string, *Service) ServiceState { return ServiceState{ProcState: ProcState{State: "stopped"}} }
	runningOn := func(addr string) func(string, *Service) ServiceState {
		return func(string, *Service) ServiceState {
			return ServiceState{ProcState: ProcState{State: "running", Addr: addr}}
		}
	}
	ondemand := &Service{Mode: ModeOnDemand, Command: []string{"x"}, Labels: map[string]string{"a": "1"}}

	// A service already routes to the port: merge into it.
	services := map[string]*Service{"web": {Mode: ModePort, Port: 4100, Labels: map[string]string{"a": "1"}}}
	name, rec, action, err := planAdopt(services, AdoptRequest{Name: "x", Port: 4100, Labels: map[string]string{"kind": "e"}, Owner: *owner}, stopped)
	if err != nil || name != "web" || action != "merged" || rec.Labels["a"] != "1" || rec.Labels["kind"] != "e" || rec.Owner != nil {
		t.Errorf("merge: %s %s %+v %v", name, action, rec, err)
	}
	if services["web"].Labels["kind"] != "" {
		t.Error("merge modified the existing record in place")
	}

	// A running on-demand service on that port also counts.
	services = map[string]*Service{"app": ondemand}
	if name, _, action, _ := planAdopt(services, AdoptRequest{Name: "x", Port: 4100, Owner: *owner}, runningOn("[::1]:4100")); name != "app" || action != "merged" {
		t.Errorf("merge into running on-demand: %s %s", name, action)
	}

	// A free name.
	name, rec, action, err = planAdopt(services, AdoptRequest{Name: "new", Port: 4100, Subdomains: map[string]int{"api": 4101}, Owner: *owner}, stopped)
	if err != nil || name != "new" || action != "created" || rec.Mode != ModeAdopted || rec.Subdomains["api"].Port != 4101 {
		t.Errorf("create: %s %s %+v %v", name, action, rec, err)
	}

	// An on-demand service that isn't running: take it over.
	name, rec, action, err = planAdopt(services, AdoptRequest{Name: "app", Port: 4200, Owner: *owner}, stopped)
	if err != nil || action != "took over" || rec.Replaces != ondemand {
		t.Errorf("take over: %s %s %+v %v", name, action, rec, err)
	}

	// A running service on another port: conflict.
	if _, _, _, err := planAdopt(services, AdoptRequest{Name: "app", Port: 4200, Owner: *owner}, runningOn("127.0.0.1:3000")); err == nil {
		t.Error("conflict not reported")
	}

	// A record whose owner exited is replaced, keeping what it replaced.
	services = map[string]*Service{"app": {Mode: ModeAdopted, Port: 1, Owner: &Owner{PID: 999999, Started: "x"}, Replaces: ondemand}}
	if _, rec, action, _ := planAdopt(services, AdoptRequest{Name: "app", Port: 4200, Owner: *owner}, stopped); action != "created" || rec.Replaces != ondemand {
		t.Errorf("stale record: %s %+v", action, rec)
	}
}

func TestHelpPage(t *testing.T) {
	services := map[string]*Service{
		"fooo":            {Mode: ModePort, Port: 1},
		"api":             {Mode: ModePort, Port: 2, Subdomains: map[string]*Subdomain{"v2": {Port: 3}}},
		"myapp.evalution": {Mode: ModePort, Port: 4},
		"web":             {Mode: ModePort, Port: 5},
	}
	want := `foo.localhost isn't registered with portkit.
Run one of these from your project's directory:

Proxy to a server already running on a fixed port:
  portkit register foo 3000

Start a command whenever foo.localhost is visited:
  portkit register foo -- npm run dev -- --port {port}

Run a command now, in your terminal, until it exits:
  portkit run foo -- npm run dev

Did you mean fooo.localhost?
Registered: api.localhost, fooo.localhost, myapp.evalution.localhost, web.localhost
`
	if got := helpPage("foo", services).text(); got != want {
		t.Errorf("help page:\n%s\nwant:\n%s", got, want)
	}
	if got := helpPage("evalution", services).text(); !strings.Contains(got, "Services under evalution.localhost: myapp.evalution.localhost") {
		t.Errorf("index of instances missing:\n%s", got)
	}
	if got := helpPage("v1.api", services).text(); !strings.Contains(got, "api.localhost has these subdomains: v2.api.localhost") {
		t.Errorf("subdomains of parent missing:\n%s", got)
	}
}

func TestUpgradeConfig(t *testing.T) {
	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "bootstrap.json")
	autosave := filepath.Join(dir, "autosave.json")
	os.WriteFile(bootstrap, []byte(`{"apps":{"portkit":{"idle_exit":"2h","services":{}},"http":{"new":true}}}`), 0o600)
	if n, err := upgradeConfig(bootstrap, autosave); n != -1 || err != nil {
		t.Errorf("no autosave: %d %v", n, err)
	}
	os.WriteFile(autosave, []byte(`{"apps":{"portkit":{"services":{"web":{"mode":"port","port":3000}}},"http":{"old":true}}}`), 0o600)
	if n, err := upgradeConfig(bootstrap, autosave); n != 1 || err != nil {
		t.Fatalf("upgrade: %d %v", n, err)
	}
	data, _ := os.ReadFile(autosave)
	s := string(data)
	if !strings.Contains(s, `"new":true`) || strings.Contains(s, `"old"`) || !strings.Contains(s, `"web":{"mode":"port","port":3000}`) {
		t.Errorf("upgraded config: %s", s)
	}
}

// The install script writes bootstrap.json from a heredoc, so a reader sees
// it before running anything. It must match the file in the repository.
func TestInstallScriptBootstrap(t *testing.T) {
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("bootstrap.json")
	if err != nil {
		t.Fatal(err)
	}
	const start = "write_file \"$PORTKIT_HOME/bootstrap.json\" <<'JSON'\n"
	_, rest, ok := strings.Cut(string(script), start)
	if !ok {
		t.Fatal("install.sh doesn't write bootstrap.json with a heredoc")
	}
	got, _, ok := strings.Cut(rest, "\nJSON\n")
	if !ok {
		t.Fatal("unterminated heredoc")
	}
	if got+"\n" != string(want) {
		t.Error("the bootstrap.json in install.sh differs from bootstrap.json")
	}
}
