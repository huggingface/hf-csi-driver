// Package util provides utilities for inter-process communication.
package util

import (
	"fmt"
	"net"
	"syscall"
)

// SendMsg sends a single file descriptor and a message over a Unix domain
// socket using SCM_RIGHTS ancillary data. Equivalent to SendMsgFds with a
// single-element slice; kept as a convenience for the existing callers.
func SendMsg(conn net.Conn, fd int, msg []byte) error {
	return SendMsgFds(conn, []int{fd}, msg)
}

// SendMsgFds sends a slice of file descriptors and a message over a Unix
// domain socket using a single SCM_RIGHTS cmsg. The receiver gets a copy
// of every fd in its own fd table. Used by sidecar mode to ship a primary
// /dev/fuse fd plus N pre-cloned fds so the sidecar can run multi-threaded
// without CAP_SYS_ADMIN (see hf-mount#94).
func SendMsgFds(conn net.Conn, fds []int, msg []byte) error {
	if len(fds) == 0 {
		return fmt.Errorf("SendMsgFds: empty fd list")
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("expected *net.UnixConn, got %T", conn)
	}

	f, err := uc.File()
	if err != nil {
		return fmt.Errorf("failed to get socket fd: %w", err)
	}
	defer func() { _ = f.Close() }()

	rights := syscall.UnixRights(fds...)
	if err := syscall.Sendmsg(int(f.Fd()), msg, rights, nil, 0); err != nil {
		return fmt.Errorf("sendmsg: %w", err)
	}
	return nil
}

// RecvMsg receives a file descriptor and a message from a Unix domain socket.
// The caller is responsible for closing the returned file descriptor.
func RecvMsg(conn net.Conn) (fd int, msg []byte, err error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, nil, fmt.Errorf("expected *net.UnixConn, got %T", conn)
	}

	f, err := uc.File()
	if err != nil {
		return -1, nil, fmt.Errorf("failed to get socket fd: %w", err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 4096)
	oob := make([]byte, syscall.CmsgSpace(4)) // space for one int32 fd

	n, oobn, _, _, err := syscall.Recvmsg(int(f.Fd()), buf, oob, 0)
	if err != nil {
		return -1, nil, fmt.Errorf("recvmsg: %w", err)
	}

	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, nil, fmt.Errorf("parse control message: %w", err)
	}
	if len(msgs) == 0 {
		return -1, nil, fmt.Errorf("no control message received")
	}

	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil {
		return -1, nil, fmt.Errorf("parse unix rights: %w", err)
	}
	if len(fds) == 0 {
		return -1, nil, fmt.Errorf("no file descriptor received")
	}

	return fds[0], buf[:n], nil
}
