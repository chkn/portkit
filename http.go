package portkit

import (
	"errors"
	"net/http"
	"strconv"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

const (
	varService   = "portkit.service"
	varSubdomain = "portkit.subdomain"
)

func loadApp(ctx caddy.Context) (*App, error) {
	a, err := ctx.App("portkit")
	if err != nil {
		return nil, err
	}
	return a.(*App), nil
}

// requestService returns the service the matcher found for r.
func requestService(app *App, r *http.Request) (name, sub string, svc *Service) {
	if n, ok := caddyhttp.GetVar(r.Context(), varService).(string); ok && n != "" {
		sub, _ = caddyhttp.GetVar(r.Context(), varSubdomain).(string)
		return n, sub, app.Services[n]
	}
	return app.lookup(r.Host)
}

// Matcher matches requests whose host is a registered service with a live
// process or port, and records the service for the handlers after it.
type Matcher struct {
	app *App
}

func (Matcher) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.portkit",
		New: func() caddy.Module { return new(Matcher) },
	}
}

func (m *Matcher) Provision(ctx caddy.Context) (err error) {
	m.app, err = loadApp(ctx)
	return err
}

func (m *Matcher) MatchWithError(r *http.Request) (bool, error) {
	name, sub, svc := m.app.lookup(r.Host)
	if svc == nil {
		return false, nil
	}
	if svc.Owner != nil && !svc.Owner.alive() {
		owners.remove(name, svc.Owner)
		return false, nil
	}
	caddyhttp.SetVar(r.Context(), varService, name)
	caddyhttp.SetVar(r.Context(), varSubdomain, sub)
	return true, nil
}

func (m *Matcher) Match(r *http.Request) bool {
	ok, _ := m.MatchWithError(r)
	return ok
}

// StartHandler runs before reverse_proxy. For an on-demand service it starts
// the process and waits for its port, and counts the request as activity for
// the idle timeout. For the other modes it checks that something listens on
// the port. Either way it shows a page instead of proxying when the service
// can't be reached.
type StartHandler struct {
	app *App
}

func (StartHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.portkit_start",
		New: func() caddy.Module { return new(StartHandler) },
	}
}

func (h *StartHandler) Provision(ctx caddy.Context) (err error) {
	h.app, err = loadApp(ctx)
	return err
}

func (h *StartHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	name, sub, svc := requestService(h.app, r)
	if svc == nil {
		return next.ServeHTTP(w, r)
	}
	host := name
	if sub != "" {
		host = sub + "." + name
	}
	if svc.Mode == ModeOnDemand {
		done := sup.Begin(name, svc)
		defer done()
		if _, err := sup.Ensure(r.Context(), name, svc, sub); err != nil {
			if errors.Is(err, r.Context().Err()) {
				return nil // the client went away
			}
			var ce *CrashError
			if errors.As(err, &ce) {
				return servePage(w, r, http.StatusBadGateway, crashPage(host, name, ce))
			}
			return err
		}
	} else if port := svc.subPort(sub); port != 0 {
		if _, ok := probes.check(port); !ok {
			return servePage(w, r, http.StatusBadGateway, notListeningPage(host, name, svc, port))
		}
	}
	return next.ServeHTTP(w, r)
}

// Upstreams is the reverse_proxy upstream source: it finds the service for
// the request's host, starts it if needed, waits until its port is known
// and returns the exact address the process listens on.
type Upstreams struct {
	app *App
}

func (Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.portkit",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

func (u *Upstreams) Provision(ctx caddy.Context) (err error) {
	u.app, err = loadApp(ctx)
	return err
}

func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	name, sub, svc := requestService(u.app, r)
	if svc == nil {
		return nil, errNoService
	}
	var addr string
	var port int
	if svc.Mode == ModeOnDemand {
		res, err := sup.Ensure(r.Context(), name, svc, sub)
		if err != nil {
			return nil, err
		}
		addr, port = res.Addr, res.Port
	} else {
		port = svc.subPort(sub)
		var ok bool
		if addr, ok = probes.addr(port); !ok {
			addr = "127.0.0.1:" + strconv.Itoa(port)
		}
	}
	// r is reverse_proxy's copy of the request, so this changes only the
	// Host sent upstream; X-Forwarded-Host keeps the original.
	if svc.HostHeader != "" {
		r.Host = expandHostHeader(svc.HostHeader, port)
	}
	return []*reverseproxy.Upstream{{Dial: addr}}, nil
}

// ResetCache runs when a dial failed and reverse_proxy is about to retry:
// detection runs again.
func (u *Upstreams) ResetCache(r *http.Request) error {
	name, sub, svc := requestService(u.app, r)
	if svc == nil {
		return nil
	}
	if svc.Mode == ModeOnDemand {
		sup.Redetect(name, sub)
	} else {
		probes.forget(svc.subPort(sub))
	}
	return nil
}

// probes caches which loopback address accepts connections for a fixed port.
var probes = &probeCache{addrs: map[int]string{}}

type probeCache struct {
	mu    sync.Mutex
	addrs map[int]string
}

func (c *probeCache) addr(port int) (string, bool) {
	c.mu.Lock()
	a, ok := c.addrs[port]
	c.mu.Unlock()
	if ok {
		return a, true
	}
	a, ok = probePort(port)
	if ok {
		c.mu.Lock()
		c.addrs[port] = a
		c.mu.Unlock()
	}
	return a, ok
}

// check probes the port now and updates the cache.
func (c *probeCache) check(port int) (string, bool) {
	a, ok := probePort(port)
	c.mu.Lock()
	if ok {
		c.addrs[port] = a
	} else {
		delete(c.addrs, port)
	}
	c.mu.Unlock()
	return a, ok
}

func (c *probeCache) forget(port int) {
	c.mu.Lock()
	delete(c.addrs, port)
	c.mu.Unlock()
}

var (
	_ caddyhttp.RequestMatcherWithError  = (*Matcher)(nil)
	_ caddyhttp.MiddlewareHandler        = (*StartHandler)(nil)
	_ reverseproxy.CachingUpstreamSource = (*Upstreams)(nil)
	_ caddy.Provisioner                  = (*Upstreams)(nil)
)
