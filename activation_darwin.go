package portkit

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libSystem functions, loaded with purego so CGO_ENABLED=0 builds work.
var (
	libOnce              sync.Once
	libErr               error
	launchActivateSocket func(name string, fds **int32, cnt *uintptr) int32
	xpcTransactionBegin  func()
	xpcTransactionEnd    func()
	libcFree             func(unsafe.Pointer)
)

func loadLibSystem() error {
	libOnce.Do(func() {
		lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			libErr = fmt.Errorf("loading libSystem: %w", err)
			return
		}
		purego.RegisterLibFunc(&launchActivateSocket, lib, "launch_activate_socket")
		purego.RegisterLibFunc(&xpcTransactionBegin, lib, "xpc_transaction_begin")
		purego.RegisterLibFunc(&xpcTransactionEnd, lib, "xpc_transaction_end")
		purego.RegisterLibFunc(&libcFree, lib, "free")
	})
	return libErr
}

// activateSockets asks launchd for the sockets named in the plist's Sockets
// dictionary. launchd binds them, so port 443 needs no root.
func activateSockets(name string) ([]*os.File, error) {
	if err := loadLibSystem(); err != nil {
		return nil, err
	}
	var fds *int32
	var cnt uintptr
	if rc := launchActivateSocket(name, &fds, &cnt); rc != 0 {
		err := syscall.Errno(rc)
		if errors.Is(err, syscall.ESRCH) {
			return nil, fmt.Errorf("socket %q: not started by launchd (set PORTKIT_SOCKETS to run by hand)", name)
		}
		return nil, fmt.Errorf("launch_activate_socket(%q): %w", name, err)
	}
	defer libcFree(unsafe.Pointer(fds))
	files := make([]*os.File, 0, cnt)
	for _, fd := range unsafe.Slice(fds, cnt) {
		// Dev servers started by the supervisor must not inherit port 443.
		syscall.CloseOnExec(int(fd))
		files = append(files, os.NewFile(uintptr(fd), "launchd:"+name))
	}
	return files, nil
}

// transactionBegin and transactionEnd keep launchd from killing Caddy under
// memory pressure (EnablePressuredExit) while a connection or child is open.
func transactionBegin() {
	if loadLibSystem() == nil {
		xpcTransactionBegin()
	}
}

func transactionEnd() {
	if loadLibSystem() == nil {
		xpcTransactionEnd()
	}
}
