package portkit

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// OwnerOf describes a running process as an Owner.
func OwnerOf(pid int) (*Owner, error) {
	started, err := ProcessStartTime(pid)
	if err != nil {
		return nil, err
	}
	return &Owner{PID: pid, Started: started}, nil
}

// alive reports whether the owner process is still running.
func (o *Owner) alive() bool {
	if o == nil || o.PID <= 0 {
		return false
	}
	started, err := ProcessStartTime(o.PID)
	return err == nil && started == o.Started
}

func (o *Owner) key() string { return strconv.Itoa(o.PID) + "@" + o.Started }

// owners watches the owners of "foreground" and "adopted" records and drops
// each record when its owner exits. It is package-level so watches survive
// config reloads.
var owners = &ownerWatcher{watching: map[string]bool{}, removing: map[string]bool{}}

type ownerWatcher struct {
	mu       sync.Mutex
	watching map[string]bool
	removing map[string]bool
}

// sync checks every owned record: dead owners are removed now, live ones
// are watched until they exit.
func (w *ownerWatcher) sync(services map[string]*Service) {
	for name, s := range services {
		o := s.Owner
		if o == nil {
			continue
		}
		if !o.alive() {
			w.remove(name, o)
			continue
		}
		w.mu.Lock()
		k := o.key()
		if w.watching[k] {
			w.mu.Unlock()
			continue
		}
		w.watching[k] = true
		w.mu.Unlock()
		go func(o Owner) {
			if err := waitExit(o.PID); err != nil {
				// Fall back to polling if the watch couldn't be set up.
				for o.alive() {
					time.Sleep(2 * time.Second)
				}
			}
			w.mu.Lock()
			delete(w.watching, o.key())
			w.mu.Unlock()
			w.removeAllOwnedBy(&o)
		}(*o)
	}
}

func (w *ownerWatcher) removeAllOwnedBy(o *Owner) {
	app := currentApp.Load()
	if app == nil {
		return
	}
	for name, s := range app.Services {
		if s.Owner != nil && *s.Owner == *o {
			w.remove(name, o)
		}
	}
}

// remove drops the record for name if o still owns it, restoring the record
// it replaced, if any. It runs in the background because it changes the
// config, which can't happen during a config load.
func (w *ownerWatcher) remove(name string, o *Owner) {
	k := name + "/" + o.key()
	w.mu.Lock()
	if w.removing[k] {
		w.mu.Unlock()
		return
	}
	w.removing[k] = true
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.removing, k)
			w.mu.Unlock()
		}()
		log := caddy.Log().Named("portkit")
		c := newSelfClient()
		for attempt := range 5 {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
			}
			err := removeOwned(c, name, o)
			if err == nil {
				log.Info("owner exited; record removed", zap.String("service", name), zap.Int("pid", o.PID))
				return
			}
			var ae *adminError
			if errors.As(err, &ae) && ae.Status < 500 {
				return // already gone or changed
			}
			log.Debug("removing record", zap.String("service", name), zap.Error(err))
		}
	}()
}

// removeOwned deletes or restores the record for name if o still owns it.
func removeOwned(c *adminClient, name string, o *Owner) error {
	var s Service
	if err := c.do(http.MethodGet, servicePath(name), nil, &s); err != nil {
		return err
	}
	if s.Owner == nil || *s.Owner != *o {
		return nil
	}
	if s.Replaces != nil {
		return c.do(http.MethodPost, servicePath(name), s.Replaces, nil)
	}
	return c.do(http.MethodDelete, servicePath(name), nil, nil)
}
