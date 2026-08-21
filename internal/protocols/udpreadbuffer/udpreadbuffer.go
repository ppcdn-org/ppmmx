// Package udpreadbuffer wraps gortsplib's readbuffer.SetReadBuffer with
// diagnostics that distinguish a permission/capability failure (the
// setsockopt(SO_RCVBUF) syscall itself returns an error, e.g. EPERM) from a
// kernel capacity limit (the syscall succeeds but net.core.rmem_max silently
// caps the value below what was requested). The two require completely
// different fixes - raising rmem_max does nothing for the former, and
// granting capabilities does nothing for the latter - so telling them apart
// from the log line alone is the point of this package.
package udpreadbuffer

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/bluenviron/gortsplib/v5/pkg/readbuffer"
)

// Outcome is the result of attempting to set a UDP socket's read buffer size.
type Outcome struct {
	Requested int
	// Actual is the buffer size read back after a successful setsockopt
	// call. Zero if the syscall itself failed.
	Actual int
	// SyscallErr is the raw error returned by SetReadBuffer()/setsockopt(),
	// if the syscall itself failed. Nil if the syscall succeeded but the
	// kernel capped the value (the net.core.rmem_max case).
	SyscallErr error
}

// OK reports whether the kernel honored the requested size exactly.
func (o Outcome) OK() bool {
	return o.SyscallErr == nil && o.Actual == o.Requested
}

// Describe formats o as a human-readable diagnostic line, including the raw
// errno when available.
func (o Outcome) Describe() string {
	if o.SyscallErr != nil {
		var errno syscall.Errno
		if errors.As(o.SyscallErr, &errno) {
			return fmt.Sprintf(
				"setsockopt(SO_RCVBUF, %d) failed with errno %d (%s): this is a "+
					"permission/capability problem (e.g. missing CAP_NET_ADMIN, seccomp, "+
					"gVisor/Fargate-style sandboxing), not a capacity limit - "+
					"raising net.core.rmem_max will NOT fix this",
				o.Requested, int(errno), errno.Error())
		}
		return fmt.Sprintf("setsockopt(SO_RCVBUF, %d) failed: %v", o.Requested, o.SyscallErr)
	}

	return fmt.Sprintf(
		"setsockopt(SO_RCVBUF, %d) succeeded but the kernel silently capped the "+
			"buffer to %d bytes: this is a capacity limit (net.core.rmem_max on this "+
			"host allows at most ~%d bytes) - raise net.core.rmem_max to fix this",
		o.Requested, o.Actual, o.Actual)
}

// Set attempts to set pc's read buffer to requested bytes and returns a
// diagnostic Outcome regardless of the result.
func Set(pc readbuffer.PacketConn, requested int) Outcome {
	o := Outcome{Requested: requested}

	err := pc.SetReadBuffer(requested)
	if err != nil {
		o.SyscallErr = err
		return o
	}

	actual, err := readbuffer.ReadBuffer(pc)
	if err != nil {
		o.SyscallErr = err
		return o
	}
	o.Actual = actual

	return o
}
