package portkit

import (
	"slices"
	"testing"
)

func TestAssignPortsListenPort(t *testing.T) {
	svc := &Service{
		Command:    []string{"server", "--port", "{port}", "--api", "{api}"},
		ListenPort: 7181,
		Subdomains: map[string]*Subdomain{"api": {Assign: true}},
	}
	ports, err := AssignPorts(svc)
	if err != nil {
		t.Fatal(err)
	}
	if ports[""] != 7181 {
		t.Errorf("main port = %d, want 7181", ports[""])
	}
	if ports["api"] == 0 || ports["api"] == 7181 {
		t.Errorf("api port = %d", ports["api"])
	}
	if !slices.Contains(ChildEnv(nil, "foo", svc, ports), "PORT=7181") {
		t.Error("PORT isn't 7181")
	}
	if argv := ExpandCommand(svc.Command, ports); argv[2] != "7181" {
		t.Errorf("argv = %q", argv)
	}
}
