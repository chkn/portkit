package portkit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// The portkit network serves the sockets that launchd or systemd activated,
// by name: "portkit/https:443" is the socket named "https" and
// "portkit/admin" the one named "admin". The port only tells Caddy that the
// server is HTTPS.
//
// For development and tests, PORTKIT_SOCKETS binds the sockets it names
// itself instead, e.g. "https=127.0.0.1:8443,https=[::1]:8443,admin=unix:a.sock".
// Names it doesn't list are still activated.

var (
	socketsMu   sync.Mutex
	socketFiles = map[string][]*os.File{}
)

// activatedSockets returns the sockets for name, activating them on first use
// and caching them for the life of the process.
func activatedSockets(name string) ([]*os.File, error) {
	socketsMu.Lock()
	defer socketsMu.Unlock()
	if files, ok := socketFiles[name]; ok {
		return files, nil
	}
	var files []*os.File
	var err error
	if spec := os.Getenv("PORTKIT_SOCKETS"); spec != "" {
		files, err = bindSockets(spec, name)
	}
	if err == nil && len(files) == 0 {
		files, err = activateSockets(name)
	}
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no activated sockets named %q", name)
	}
	socketFiles[name] = files
	return files, nil
}

// bindSockets binds the PORTKIT_SOCKETS entries for name.
func bindSockets(spec, name string) ([]*os.File, error) {
	var files []*os.File
	for entry := range strings.SplitSeq(spec, ",") {
		n, addr, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			return nil, fmt.Errorf("PORTKIT_SOCKETS: bad entry %q", entry)
		}
		if n != name {
			continue
		}
		network := "tcp"
		if path, isUnix := strings.CutPrefix(addr, "unix:"); isUnix {
			network, addr = "unix", path
			_ = os.Remove(addr)
		}
		ln, err := net.Listen(network, addr)
		if err != nil {
			return nil, err
		}
		var f *os.File
		switch l := ln.(type) {
		case *net.TCPListener:
			f, err = l.File()
		case *net.UnixListener:
			l.SetUnlinkOnClose(false)
			if err = os.Chmod(addr, 0o600); err == nil {
				f, err = l.File()
			}
		}
		ln.Close()
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// getListener is the caddy.ListenerFunc for the portkit network. Each call
// duplicates the activated sockets, so closing the listener on a config
// reload leaves the sockets that launchd or systemd holds open.
func getListener(_ context.Context, _, host, _ string, _ uint, _ net.ListenConfig) (any, error) {
	files, err := activatedSockets(host)
	if err != nil {
		return nil, err
	}
	lns := make([]net.Listener, 0, len(files))
	for _, f := range files {
		ln, err := net.FileListener(f)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return nil, fmt.Errorf("socket %q: %w", host, err)
		}
		lns = append(lns, ln)
	}
	return newListener(host, lns), nil
}

// listener merges several sockets (IPv4 and IPv6) into one net.Listener and
// tracks the connections it accepts.
type listener struct {
	name  string
	lns   []net.Listener
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newListener(name string, lns []net.Listener) *listener {
	l := &listener{name: name, lns: lns, conns: make(chan net.Conn), done: make(chan struct{})}
	for _, ln := range lns {
		go l.acceptLoop(ln)
	}
	if name == "admin" {
		adminListeners.add(l)
	}
	return l
}

func (l *listener) acceptLoop(ln net.Listener) {
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Back off on errors such as EMFILE rather than spinning.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			caddy.Log().Named("portkit").Warn("accept failed", zap.String("socket", l.name), zap.Error(err))
			time.Sleep(delay)
			continue
		}
		delay = 0
		l.deliver(c)
	}
}

// deliver hands c to Accept, or closes it if the listener closes first.
func (l *listener) deliver(c net.Conn) bool {
	select {
	case l.conns <- c:
		return true
	case <-l.done:
		c.Close()
		return false
	}
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, &net.OpError{Op: "accept", Net: "portkit", Err: net.ErrClosed}
	default:
	}
	select {
	case c := <-l.conns:
		return trackConn(c), nil
	case <-l.done:
		return nil, &net.OpError{Op: "accept", Net: "portkit", Err: net.ErrClosed}
	}
}

func (l *listener) Close() error {
	l.once.Do(func() {
		close(l.done)
		for _, ln := range l.lns {
			ln.Close()
		}
		if l.name == "admin" {
			adminListeners.remove(l)
		}
	})
	return nil
}

func (l *listener) Addr() net.Addr {
	if len(l.lns) == 0 {
		return &net.UnixAddr{Name: l.name, Net: "portkit"}
	}
	return l.lns[0].Addr()
}

// trackedConn counts as activity until it is closed.
type trackedConn struct {
	net.Conn
	once sync.Once
}

func trackConn(c net.Conn) net.Conn {
	activity.connOpened()
	return &trackedConn{Conn: c}
}

func (c *trackedConn) Close() error {
	c.once.Do(activity.connClosed)
	return c.Conn.Close()
}

// adminListeners are the open admin listeners, newest last. During a config
// reload the new one opens before the old one closes.
var adminListeners listenerSet

type listenerSet struct {
	mu  sync.Mutex
	lns []*listener
}

func (s *listenerSet) add(l *listener) {
	s.mu.Lock()
	s.lns = append(s.lns, l)
	s.mu.Unlock()
}

func (s *listenerSet) remove(l *listener) {
	s.mu.Lock()
	for i, x := range s.lns {
		if x == l {
			s.lns = append(s.lns[:i], s.lns[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

func (s *listenerSet) newest() *listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lns) == 0 {
		return nil
	}
	return s.lns[len(s.lns)-1]
}

// dialSelf connects to this process's own admin API without going through
// the socket file, so the plugin can change its own config.
func dialSelf(ctx context.Context) (net.Conn, error) {
	l := adminListeners.newest()
	if l == nil {
		return nil, errors.New("admin API is not listening")
	}
	server, client := net.Pipe()
	delivered := make(chan bool, 1)
	go func() { delivered <- l.deliver(server) }()
	select {
	case ok := <-delivered:
		if !ok {
			client.Close()
			return nil, errors.New("admin listener closed")
		}
		return client, nil
	case <-ctx.Done():
		client.Close()
		return nil, ctx.Err()
	}
}
