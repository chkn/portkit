package portkit

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"foo", "my-app", "myapp.evalution", "a.b.c.d", "x1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "portkit", "Foo", "foo_bar", "-foo", "foo-", "a..b", "a.b.c.d.e", ".foo"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) accepted", bad)
		}
	}
}

func TestPlaceholders(t *testing.T) {
	argv := []string{"app", "--port", "{port}", "--api={api}", "--json", "{{\"a\":1}}", "{Not}", "{grpc}{api}"}
	if got, want := Placeholders(argv), []string{"port", "api", "grpc"}; !slices.Equal(got, want) {
		t.Errorf("Placeholders = %v, want %v", got, want)
	}
	got := ExpandCommand(argv, map[string]int{"": 3000, "api": 3001, "grpc": 3002})
	want := []string{"app", "--port", "3000", "--api=3001", "--json", "{\"a\":1}", "{Not}", "30023001"}
	if !slices.Equal(got, want) {
		t.Errorf("ExpandCommand = %q, want %q", got, want)
	}
}

func TestLookup(t *testing.T) {
	services := map[string]*Service{
		"foo":             {Mode: ModePort, Port: 1, Subdomains: map[string]*Subdomain{"api": {Port: 2}}},
		"myapp.evalution": {Mode: ModePort, Port: 3},
	}
	tests := []struct{ host, name, sub string }{
		{"foo.localhost", "foo", ""},
		{"FOO.localhost:443", "foo", ""},
		{"api.foo.localhost", "foo", "api"},
		{"myapp.evalution.localhost", "myapp.evalution", ""},
		{"web.foo.localhost", "", ""},
		{"evalution.localhost", "", ""},
		{"foo.example.com", "", ""},
		{"localhost", "", ""},
	}
	for _, tt := range tests {
		name, sub, _ := lookup(services, tt.host)
		if name != tt.name || sub != tt.sub {
			t.Errorf("lookup(%q) = %q, %q; want %q, %q", tt.host, name, sub, tt.name, tt.sub)
		}
	}
}

func TestProvision(t *testing.T) {
	bad := map[string]*Service{
		"no port":          {Mode: ModePort},
		"no command":       {Mode: ModeOnDemand},
		"bad mode":         {Mode: "nope", Port: 1},
		"no owner":         {Mode: ModeAdopted, Port: 1},
		"undeclared {api}": {Mode: ModeOnDemand, Command: []string{"x", "{api}"}},
		"bad idle":         {Mode: ModeOnDemand, Command: []string{"x"}, IdleTimeout: "soon"},
		"bad regex":        {Mode: ModeOnDemand, Command: []string{"x"}, Subdomains: map[string]*Subdomain{"a": {Match: "("}}},
		"two sources":      {Mode: ModeOnDemand, Command: []string{"x"}, Subdomains: map[string]*Subdomain{"a": {Port: 1, Assign: true}}},
		"regex for port":   {Mode: ModePort, Port: 1, Subdomains: map[string]*Subdomain{"a": {Match: "x"}}},
	}
	for what, s := range bad {
		if err := s.provision("foo"); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	deep := &Service{Mode: ModePort, Port: 1, Subdomains: map[string]*Subdomain{"a": {Port: 2}}}
	if err := deep.provision("a.b.c.d"); err == nil {
		t.Error("subdomain of a four-label name accepted")
	}
	good := &Service{Mode: ModeOnDemand, Command: []string{"x", "{port}", "{api}"}, IdleTimeout: "never",
		Subdomains: map[string]*Subdomain{"api": {Assign: true}, "grpc": {Match: `ready at :(\d+)`}}}
	if err := good.provision("foo"); err != nil {
		t.Fatal(err)
	}
	if good.idleTimeout != 0 || good.Subdomains["grpc"].re == nil {
		t.Error("provision didn't parse idle_timeout or compile the regex")
	}
}

func TestCheckConflicts(t *testing.T) {
	services := map[string]*Service{
		"evalution":       {Mode: ModePort, Port: 1, Subdomains: map[string]*Subdomain{"myapp": {Port: 2}}},
		"myapp.evalution": {Mode: ModePort, Port: 3},
	}
	if err := checkConflicts(services); err == nil || !strings.Contains(err.Error(), "myapp.evalution.localhost") {
		t.Errorf("checkConflicts = %v", err)
	}
}

func TestChildEnv(t *testing.T) {
	t.Setenv("PORTKIT_HOME", "/pk")
	svc := &Service{Env: map[string]string{"PATH": "/custom/bin", "FOO": "bar"},
		Subdomains: map[string]*Subdomain{"api-v2": {Assign: true}, "db": {Port: 5432}}}
	env := ChildEnv([]string{"PATH=/usr/bin", "HOME=/home/x"}, "foo", svc, map[string]int{"": 3000, "api-v2": 3001})
	want := []string{"PATH=/custom/bin", "HOME=/home/x", "FOO=bar", "PORT=3000", "PORTKIT_SERVICE=foo",
		"PORTKIT_URL=https://foo.localhost", "PORTKIT_HOME=/pk", "PORTKIT_PORT_API_V2=3001", "PORTKIT_PORT_DB=5432"}
	for _, kv := range want {
		if !slices.Contains(env, kv) {
			t.Errorf("missing %s in %v", kv, env)
		}
	}
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "NODE_EXTRA_CA_CERTS=") }) {
		t.Error("NODE_EXTRA_CA_CERTS not set")
	}
	env = ChildEnv([]string{"NODE_EXTRA_CA_CERTS=/mine.pem"}, "foo", &Service{}, map[string]int{"": 1})
	if !slices.Contains(env, "NODE_EXTRA_CA_CERTS=/mine.pem") {
		t.Error("NODE_EXTRA_CA_CERTS overridden")
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "data"), []byte("x"), 0o644)
	if p, err := lookPath("tool", []string{"PATH=/nope:" + bin}, "/"); err != nil || p != filepath.Join(bin, "tool") {
		t.Errorf("lookPath(tool) = %q, %v", p, err)
	}
	if _, err := lookPath("data", []string{"PATH=" + bin}, "/"); err == nil {
		t.Error("found a non-executable file")
	}
	if p, err := lookPath("./bin/tool", nil, dir); err != nil || p != filepath.Join(bin, "tool") {
		t.Errorf("lookPath(./bin/tool) = %q, %v", p, err)
	}
}
