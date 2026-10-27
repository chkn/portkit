package portkit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	maxLogSize = 10 << 20 // rotate at 10 MB, keeping one old file
	tailLines  = 50
)

// logFile is an append-only service log that rotates to <name>.log.1.
type logFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openLog(path string) (*logFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &logFile{path: path, f: f, size: fi.Size()}, nil
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > maxLogSize {
		l.rotate()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *logFile) rotate() {
	l.f.Close()
	_ = os.Rename(l.path, l.path+".1")
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	l.f, l.size = f, 0
}

func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// lineRing keeps the last n lines of output.
type lineRing struct {
	mu    sync.Mutex
	n     int
	lines []string
}

func newLineRing(n int) *lineRing { return &lineRing{n: n} }

func (r *lineRing) add(line string) {
	r.mu.Lock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.n {
		r.lines = r.lines[len(r.lines)-r.n:]
	}
	r.mu.Unlock()
}

func (r *lineRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

func (r *lineRing) reset() {
	r.mu.Lock()
	r.lines = nil
	r.mu.Unlock()
}
