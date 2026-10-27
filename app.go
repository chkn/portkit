package portkit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

const defaultIdleExit = 2 * time.Hour

// currentApp is the most recently started app, for code that runs outside a
// request, such as owner watches.
var currentApp atomic.Pointer[App]

var exitOnce sync.Once

// App is apps.portkit: the service registry and Caddy's idle exit.
type App struct {
	// IdleExit is how long Caddy waits with no connections and no running
	// children before it exits: a duration such as "2h", or "never".
	IdleExit string `json:"idle_exit,omitempty"`

	// Services maps each name to its record.
	Services map[string]*Service `json:"services,omitempty"`

	idleExit time.Duration
	logger   *zap.Logger
}

func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "portkit",
		New: func() caddy.Module { return new(App) },
	}
}

func (a *App) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	switch a.IdleExit {
	case "":
		a.idleExit = defaultIdleExit
	case "never":
		a.idleExit = 0
	default:
		d, err := caddy.ParseDuration(a.IdleExit)
		if err != nil || d <= 0 {
			return fmt.Errorf("bad idle_exit %q", a.IdleExit)
		}
		a.idleExit = d
	}
	if a.Services == nil {
		a.Services = map[string]*Service{}
	}
	for name, s := range a.Services {
		if s == nil {
			return fmt.Errorf("service %s: empty record", name)
		}
		if err := s.provision(name); err != nil {
			return err
		}
	}
	return checkConflicts(a.Services)
}

func (a *App) Start() error {
	currentApp.Store(a)
	children.reapOrphans()
	exitOnce.Do(func() {
		caddy.OnExit(func(context.Context) { sup.stopAll() })
	})
	activity.setIdleExit(a.idleExit)
	sup.reconcile(a.Services)
	owners.sync(a.Services)
	return nil
}

// Stop does nothing: Caddy stops the old app on every config reload, and
// the supervisor's processes must survive that.
func (a *App) Stop() error { return nil }

// lookup finds the service for a request host.
func (a *App) lookup(host string) (name, sub string, svc *Service) {
	return lookup(a.Services, host)
}

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
)
