package portkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// AdminAPI adds runtime endpoints to Caddy's admin API, beside the standard
// /config/apps/portkit/... endpoints that hold the records:
//
//	GET  /portkit/info   Caddy's process, paths and activity
//	GET  /portkit/state  whether each service is running, and on which port
//	POST /portkit/stop   {"name": ...}: stop an on-demand process
//	POST /portkit/adopt  register a running server (see Adopt)
type AdminAPI struct{}

func (AdminAPI) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "admin.api.portkit",
		New: func() caddy.Module { return new(AdminAPI) },
	}
}

var processStarted = time.Now()

func (AdminAPI) Routes() []caddy.AdminRoute {
	return []caddy.AdminRoute{
		{Pattern: "/portkit/info", Handler: caddy.AdminHandlerFunc(handleInfo)},
		{Pattern: "/portkit/state", Handler: caddy.AdminHandlerFunc(handleState)},
		{Pattern: "/portkit/stop", Handler: caddy.AdminHandlerFunc(handleStop)},
		{Pattern: "/portkit/adopt", Handler: caddy.AdminHandlerFunc(handleAdopt)},
	}
}

// Info is the response of GET /portkit/info.
type Info struct {
	PID         int       `json:"pid"`
	Version     string    `json:"version"`
	Started     time.Time `json:"started"`
	Home        string    `json:"home"`
	CACert      string    `json:"ca_cert"`
	Connections int       `json:"connections"`
	Children    int       `json:"children"`
	IdleExit    string    `json:"idle_exit"`
}

func handleInfo(w http.ResponseWriter, r *http.Request) error {
	conns, kids := activity.snapshot()
	_, version := caddy.Version()
	idle := defaultIdleExit.String()
	if a := currentApp.Load(); a != nil {
		idle = "never"
		if a.idleExit > 0 {
			idle = a.idleExit.String()
		}
	}
	return writeJSON(w, Info{
		PID: os.Getpid(), Version: version, Started: processStarted, Home: Home(),
		CACert: CACertPath(), Connections: conns, Children: kids, IdleExit: idle,
	})
}

// ServiceState is the runtime state of one service in GET /portkit/state.
type ServiceState struct {
	ProcState
	Mode string `json:"mode"`
}

func handleState(w http.ResponseWriter, r *http.Request) error {
	out := map[string]ServiceState{}
	if a := currentApp.Load(); a != nil {
		for name, s := range a.Services {
			out[name] = stateOf(name, s)
		}
	}
	return writeJSON(w, out)
}

func stateOf(name string, s *Service) ServiceState {
	st := ServiceState{Mode: s.Mode}
	switch s.Mode {
	case ModeOnDemand:
		st.ProcState = sup.State(name)
	case ModePort:
		st.State = "stopped"
		if addr, ok := probePort(s.Port); ok {
			st.State, st.Addr = "running", addr
		}
	default:
		st.State, st.PID = "exited", s.Owner.PID
		if s.Owner.alive() {
			st.State = "running"
			st.Addr, _ = probePort(s.Port)
		}
	}
	return st
}

func handleStop(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: errors.New("use POST")}
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: err}
	}
	a := currentApp.Load()
	if a == nil || a.Services[req.Name] == nil {
		return caddy.APIError{HTTPStatus: http.StatusNotFound, Err: fmt.Errorf("no service named %s", req.Name)}
	}
	if a.Services[req.Name].Mode != ModeOnDemand {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: fmt.Errorf("%s isn't an on-demand service; portkit doesn't run its process", req.Name)}
	}
	stopped, reset := sup.Stop(req.Name)
	return writeJSON(w, map[string]bool{"stopped": stopped, "reset": reset})
}

// AdoptRequest is the body of POST /portkit/adopt.
type AdoptRequest struct {
	Name       string            `json:"name"`
	Port       int               `json:"port"`
	Cwd        string            `json:"cwd,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Subdomains map[string]int    `json:"subdomains,omitempty"`
	Owner      Owner             `json:"owner"`
}

// AdoptResponse says what adopt did.
type AdoptResponse struct {
	Name   string   `json:"name"`
	URL    string   `json:"url"`
	Action string   `json:"action"` // "merged", "created" or "took over"
	Record *Service `json:"record"`
}

func handleAdopt(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: errors.New("use POST")}
	}
	var req AdoptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: err}
	}
	a := currentApp.Load()
	if a == nil {
		return caddy.APIError{HTTPStatus: http.StatusServiceUnavailable, Err: errors.New("portkit app not running")}
	}
	if uid, err := ProcessUID(req.Owner.PID); err != nil || uid != os.Getuid() || !req.Owner.alive() {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: fmt.Errorf("process %d isn't a running process of this user", req.Owner.PID)}
	}
	name, rec, action, err := planAdopt(a.Services, req, stateOf)
	if err != nil {
		return caddy.APIError{HTTPStatus: http.StatusConflict, Err: err}
	}
	if err := newSelfClient().do(http.MethodPost, servicePath(name), rec, nil); err != nil {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: err}
	}
	return writeJSON(w, AdoptResponse{Name: name, URL: ServiceURL(name), Action: action, Record: rec})
}

// planAdopt decides what `portkit adopt` does:
//
//   - a service already routes to the port: merge labels and subdomains
//     into it, keeping its name and owner;
//   - the name is free, or held by a record whose owner exited: a new
//     "adopted" record;
//   - the name is an on-demand or fixed-port service that isn't running:
//     take it while the owner lives, then restore it;
//   - the name is a running service on another port: a conflict.
func planAdopt(services map[string]*Service, req AdoptRequest, state func(string, *Service) ServiceState) (string, *Service, string, error) {
	if err := ValidateName(req.Name); err != nil {
		return "", nil, "", err
	}
	subs := map[string]*Subdomain{}
	for sub, port := range req.Subdomains {
		subs[sub] = &Subdomain{Port: port}
	}

	for _, name := range sortedNames(services) {
		s := services[name]
		if !routesTo(name, s, req.Port, state) {
			continue
		}
		merged := *s
		merged.Labels = maps.Clone(s.Labels)
		if merged.Labels == nil && len(req.Labels) > 0 {
			merged.Labels = map[string]string{}
		}
		maps.Copy(merged.Labels, req.Labels)
		if len(subs) > 0 {
			merged.Subdomains = maps.Clone(s.Subdomains)
			if merged.Subdomains == nil {
				merged.Subdomains = map[string]*Subdomain{}
			}
			maps.Copy(merged.Subdomains, subs)
		}
		return name, &merged, "merged", nil
	}

	rec := &Service{
		Mode:   ModeAdopted,
		Port:   req.Port,
		Cwd:    req.Cwd,
		Labels: req.Labels,
		Owner:  &req.Owner,
	}
	if len(subs) > 0 {
		rec.Subdomains = subs
	}
	existing := services[req.Name]
	if existing == nil || (existing.Owner != nil && !existing.Owner.alive()) {
		if existing != nil && existing.Replaces != nil {
			rec.Replaces = existing.Replaces
		}
		return req.Name, rec, "created", nil
	}
	if st := state(req.Name, existing); st.State == "running" || st.State == "starting" {
		return "", nil, "", fmt.Errorf("%s.localhost already belongs to a running %s service on another port; pick another name", req.Name, modeLabel(existing.Mode))
	}
	if existing.Owner != nil {
		return "", nil, "", fmt.Errorf("%s.localhost already belongs to process %d", req.Name, existing.Owner.PID)
	}
	rec.Replaces = existing
	return req.Name, rec, "took over", nil
}

// routesTo reports whether a service currently routes its main host to port.
func routesTo(name string, s *Service, port int, state func(string, *Service) ServiceState) bool {
	switch s.Mode {
	case ModeOnDemand:
		_, p, err := net.SplitHostPort(state(name, s).Addr)
		return err == nil && p == strconv.Itoa(port)
	default:
		if s.Owner != nil && !s.Owner.alive() {
			return false
		}
		return s.Port == port
	}
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

var _ caddy.AdminRouter = AdminAPI{}
