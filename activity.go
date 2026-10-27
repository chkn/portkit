package portkit

import (
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// activity counts open connections and running children. Each one holds an
// XPC transaction on macOS, and when both counts reach zero Caddy exits
// after idle_exit; launchd or systemd starts it again on the next connection.
var activity = &activityTracker{}

type activityTracker struct {
	mu       sync.Mutex
	conns    int
	children int
	idleExit time.Duration // 0 means never
	timer    *time.Timer
	gen      int
}

func (a *activityTracker) connOpened()   { a.change(1, 0) }
func (a *activityTracker) connClosed()   { a.change(-1, 0) }
func (a *activityTracker) childStarted() { a.change(0, 1) }
func (a *activityTracker) childExited()  { a.change(0, -1) }

func (a *activityTracker) change(conns, children int) {
	if conns+children > 0 {
		transactionBegin()
	} else {
		transactionEnd()
	}
	a.mu.Lock()
	a.conns += conns
	a.children += children
	a.reschedule()
	a.mu.Unlock()
}

func (a *activityTracker) setIdleExit(d time.Duration) {
	a.mu.Lock()
	a.idleExit = d
	a.reschedule()
	a.mu.Unlock()
}

// reschedule arms the idle timer when nothing is active. a.mu must be held.
func (a *activityTracker) reschedule() {
	a.gen++
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	if a.conns > 0 || a.children > 0 || a.idleExit <= 0 {
		return
	}
	gen := a.gen
	a.timer = time.AfterFunc(a.idleExit, func() {
		a.mu.Lock()
		idle := gen == a.gen
		a.mu.Unlock()
		if !idle {
			return
		}
		caddy.Log().Named("portkit").Info("idle, exiting", zap.Duration("idle_exit", a.idleExit))
		// SIGTERM runs Caddy's normal graceful shutdown, which exits with status 0.
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	})
}

func (a *activityTracker) snapshot() (conns, children int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conns, a.children
}
