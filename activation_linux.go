package portkit

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var (
	systemdOnce  sync.Once
	systemdFiles map[string][]*os.File
	systemdErr   error
)

// listenFDsStart is SD_LISTEN_FDS_START: the first passed fd.
const listenFDsStart = 3

// activateSockets returns the fds systemd passed with FileDescriptorName=name.
func activateSockets(name string) ([]*os.File, error) {
	systemdOnce.Do(func() { systemdFiles, systemdErr = readListenFDs() })
	if systemdErr != nil {
		return nil, systemdErr
	}
	return systemdFiles[name], nil
}

// readListenFDs reads LISTEN_PID, LISTEN_FDS and LISTEN_FDNAMES, then unsets
// them so child processes don't see them.
func readListenFDs() (map[string][]*os.File, error) {
	defer func() {
		os.Unsetenv("LISTEN_PID")
		os.Unsetenv("LISTEN_FDS")
		os.Unsetenv("LISTEN_FDNAMES")
	}()
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
		return nil, fmt.Errorf("not started by systemd socket activation (set PORTKIT_SOCKETS to run by hand)")
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("LISTEN_FDS: no sockets passed")
	}
	names := strings.Split(os.Getenv("LISTEN_FDNAMES"), ":")
	files := map[string][]*os.File{}
	for i := range n {
		fd := listenFDsStart + i
		// Dev servers started by the supervisor must not inherit port 443.
		syscall.CloseOnExec(fd)
		name := "unknown"
		if i < len(names) && names[i] != "" {
			name = names[i]
		}
		files[name] = append(files[name], os.NewFile(uintptr(fd), "systemd:"+name))
	}
	return files, nil
}

func transactionBegin() {}
func transactionEnd()   {}
