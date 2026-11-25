package gsa

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"syscall"
)

// DeviceTransportError carries only retry classification, never request URLs,
// credentials, response bodies or raw network-error text.
type DeviceTransportError struct {
	Retryable bool
	Canceled  bool
}

func (e *DeviceTransportError) Error() string { return "gsa: account-device transport failed" }

func deviceTransportFailure(err error) error {
	canceled := errors.Is(err, context.Canceled)
	var network net.Error
	// Winsock errors are distinct from Go's portable errno constants. In
	// particular, net.OpError.Temporary does not mark read resets temporary.
	windowsDisconnect := false
	if runtime.GOOS == "windows" {
		for _, code := range []syscall.Errno{64, 10051, 10053, 10054, 10060, 10061, 10065} {
			windowsDisconnect = windowsDisconnect || errors.Is(err, code)
		}
	}
	retryable := !canceled && (errors.Is(err, context.DeadlineExceeded) ||
		windowsDisconnect ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.As(err, &network) && (network.Timeout() || network.Temporary()))
	return &DeviceTransportError{Retryable: retryable, Canceled: canceled}
}
