//go:build !darwin && !linux

package portkit

import "errors"

var errUnsupported = errors.New("unsupported platform")

func ListeningSockets(int) ([]Socket, error) { return nil, errUnsupported }
func ProcessStartTime(int) (string, error)   { return "", errUnsupported }
func ProcessUID(int) (int, error)            { return 0, errUnsupported }
func waitExit(int) error                     { return errUnsupported }
