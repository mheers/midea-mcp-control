//go:build !windows

package discovery

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

func setBroadcast(conn net.PacketConn) error {
	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		return errors.New("discovery socket does not support broadcast")
	}
	raw, err := udpConn.SyscallConn()
	if err != nil {
		return fmt.Errorf("access discovery socket: %w", err)
	}
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		socketErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return fmt.Errorf("access discovery socket: %w", err)
	}
	if socketErr != nil {
		return fmt.Errorf("enable discovery broadcast: %w", socketErr)
	}
	return nil
}
