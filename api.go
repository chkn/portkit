package portkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
)

// APIHandler serves https://portkit.localhost: the discovery endpoint at
// /services and an index page at /.
type APIHandler struct {
	app *App
}

func (APIHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.portkit_api",
		New: func() caddy.Module { return new(APIHandler) },
	}
}

func (h *APIHandler) Provision(ctx caddy.Context) (err error) {
	h.app, err = loadApp(ctx)
	return err
}

// DiscoveredService is one entry of GET /services. Commands and
// environments are left out.
type DiscoveredService struct {
	Name       string            `json:"name"`
	URL        string            `json:"url"`
	Mode       string            `json:"mode"`
	Cwd        string            `json:"cwd,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Subdomains map[string]string `json:"subdomains,omitempty"`
	Running    bool              `json:"running"`
}

func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	switch r.URL.Path {
	case "/services":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return caddyhttp.Error(http.StatusMethodNotAllowed, nil)
		}
		list, err := discover(h.app.Services, r.URL.Query()["label"], r.URL.Query().Get("cwd"))
		if err != nil {
			return caddyhttp.Error(http.StatusBadRequest, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"services": list})
	case "/":
		return servePage(w, r, http.StatusOK, indexPage(h.app.Services))
	}
	return caddyhttp.Error(http.StatusNotFound, nil)
}

// discover lists services that have all the given labels ("key=value") and,
// if cwd is set, whose working directory is cwd or one of its parents,
// deepest first.
func discover(services map[string]*Service, labels []string, cwd string) ([]DiscoveredService, error) {
	want := map[string]string{}
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return nil, fmt.Errorf("label filter %q must be key=value", l)
		}
		want[k] = v
	}
	if cwd != "" {
		cwd = filepath.Clean(cwd)
	}
	out := []DiscoveredService{}
	for _, name := range sortedNames(services) {
		s := services[name]
		if s.Owner != nil && !s.Owner.alive() {
			continue
		}
		match := true
		for k, v := range want {
			if s.Labels[k] != v {
				match = false
			}
		}
		if !match {
			continue
		}
		if cwd != "" {
			c := filepath.Clean(s.Cwd)
			if s.Cwd == "" || (c != cwd && !strings.HasPrefix(cwd, strings.TrimSuffix(c, "/")+"/")) {
				continue
			}
		}
		d := DiscoveredService{Name: name, URL: ServiceURL(name), Mode: s.Mode, Cwd: s.Cwd, Labels: s.Labels, Running: running(name, s)}
		for sub := range s.Subdomains {
			if d.Subdomains == nil {
				d.Subdomains = map[string]string{}
			}
			d.Subdomains[sub] = ServiceURL(sub + "." + name)
		}
		out = append(out, d)
	}
	if cwd != "" {
		sort.SliceStable(out, func(i, j int) bool { return len(out[i].Cwd) > len(out[j].Cwd) })
	}
	return out, nil
}

// running reports whether a service is up now, without starting it.
func running(name string, s *Service) bool {
	switch s.Mode {
	case ModeOnDemand:
		st := sup.State(name).State
		return st == "running" || st == "starting"
	case ModePort:
		_, ok := probePort(s.Port)
		return ok
	default:
		return s.Owner.alive()
	}
}

// Permission allows on-demand certificates for any name under .localhost
// with up to four labels, registered or not, so the help page loads for
// unregistered names. At most 10 new unregistered names a minute get one,
// so a runaway script can't fill the disk.
type Permission struct {
	app *App
}

func (Permission) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "tls.permission.portkit",
		New: func() caddy.Module { return new(Permission) },
	}
}

func (p *Permission) Provision(ctx caddy.Context) (err error) {
	p.app, err = loadApp(ctx)
	return err
}

const newNamesPerMinute = 10

var certNames = &nameLimiter{allowed: map[string]bool{}}

type nameLimiter struct {
	mu      sync.Mutex
	allowed map[string]bool
	recent  []time.Time
}

func (p *Permission) CertificateAllowed(_ context.Context, host string) error {
	name, ok := nameFromHost(host)
	if !ok {
		return fmt.Errorf("%s is not under .localhost", host)
	}
	if name != reservedName {
		labels := strings.Split(name, ".")
		if len(labels) > maxLabels {
			return fmt.Errorf("%s has more than %d labels under .localhost", host, maxLabels)
		}
		for _, l := range labels {
			if !ValidLabel(l) {
				return fmt.Errorf("%s is not a valid name", host)
			}
		}
	}
	if _, _, svc := p.app.lookup(host); svc != nil || name == reservedName {
		return nil
	}
	return certNames.allow(name, time.Now())
}

func (l *nameLimiter) allow(name string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.allowed[name] {
		return nil
	}
	cutoff := now.Add(-time.Minute)
	for len(l.recent) > 0 && l.recent[0].Before(cutoff) {
		l.recent = l.recent[1:]
	}
	if len(l.recent) >= newNamesPerMinute {
		return fmt.Errorf("more than %d new unregistered names in a minute", newNamesPerMinute)
	}
	l.recent = append(l.recent, now)
	l.allowed[name] = true
	return nil
}

var (
	_ caddyhttp.MiddlewareHandler = (*APIHandler)(nil)
	_ caddytls.OnDemandPermission = (*Permission)(nil)
)
