//go:build !darwin && !linux

package portkit

import (
	"errors"
	"os"
)

func activateSockets(string) ([]*os.File, error) {
	return nil, errors.New("socket activation needs macOS or Linux (set PORTKIT_SOCKETS to run by hand)")
}

func transactionBegin() {}
func transactionEnd()   {}
