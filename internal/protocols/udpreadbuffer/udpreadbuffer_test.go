package udpreadbuffer

import (
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetSucceeds(t *testing.T) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{})
	require.NoError(t, err)
	defer pc.Close()

	// a small, universally-grantable size to avoid depending on the test
	// host's net.core.rmem_max.
	outcome := Set(pc, 4096)
	require.True(t, outcome.OK())
	require.Contains(t, outcome.Describe(), "")
}

func TestSetSyscallFailureDescribesErrno(t *testing.T) {
	o := Outcome{Requested: 2097152, SyscallErr: syscall.EPERM}
	require.False(t, o.OK())
	desc := o.Describe()
	require.Contains(t, desc, syscall.EPERM.Error())
	require.Contains(t, desc, "permission/capability problem")
	require.Contains(t, desc, "will NOT fix this")
}

func TestSetKernelCapDescribesCapacityLimit(t *testing.T) {
	o := Outcome{Requested: 2097152, Actual: 212992}
	require.False(t, o.OK())
	desc := o.Describe()
	require.Contains(t, desc, "212992")
	require.Contains(t, desc, "capacity limit")
	require.Contains(t, desc, "raise net.core.rmem_max")
}
