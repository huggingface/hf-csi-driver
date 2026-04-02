package util

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSendRecvMsg(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	// Create a temporary file to pass its fd.
	tmpFile, err := os.CreateTemp(dir, "fdtest")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer func() {
		_ = tmpFile.Close()
	}()

	if _, err := tmpFile.WriteString("hello from fd"); err != nil {
		t.Fatalf("write: %v", err)
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	sentMsg := []byte(`{"sourceType":"bucket","sourceId":"user/data"}`)

	// Sender goroutine: accept connection, send fd + message.
	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer func() {
			_ = conn.Close()
		}()
		errCh <- SendMsg(conn, int(tmpFile.Fd()), sentMsg)
	}()

	// Receiver: connect, receive fd + message.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	fd, msg, err := RecvMsg(conn)
	if err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	defer func() {
		_ = syscall.Close(fd)
	}()

	// Check sender didn't error.
	if err := <-errCh; err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	// Verify message.
	if string(msg) != string(sentMsg) {
		t.Errorf("message: got %q, want %q", msg, sentMsg)
	}

	// Verify we can read the file through the received fd.
	received := os.NewFile(uintptr(fd), "received")
	if _, err := received.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	buf := make([]byte, 64)
	n, err := received.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "hello from fd" {
		t.Errorf("file content via fd: got %q, want %q", got, "hello from fd")
	}
}

func TestSendRecvMsg_MultipleFDs(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	// Two separate files to pass sequentially.
	files := make([]*os.File, 2)
	for i := range files {
		f, err := os.CreateTemp(dir, "fd")
		if err != nil {
			t.Fatalf("create temp file %d: %v", i, err)
		}
		defer func() {
			_ = f.Close()
		}()
		if _, err := f.WriteString("file-" + string(rune('A'+i))); err != nil {
			t.Fatalf("write: %v", err)
		}
		files[i] = f
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	// Capture fds before goroutine to avoid race with deferred Close.
	fd0 := int(files[0].Fd())
	fd1 := int(files[1].Fd())

	// Sender: send two fds sequentially on the same connection.
	go func() {
		conn, _ := listener.Accept()
		defer func() {
			_ = conn.Close()
		}()
		_ = SendMsg(conn, fd0, []byte("msg0"))
		_ = SendMsg(conn, fd1, []byte("msg1"))
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	for i := 0; i < 2; i++ {
		fd, msg, err := RecvMsg(conn)
		if err != nil {
			t.Fatalf("RecvMsg[%d]: %v", i, err)
		}
		defer func() {
			_ = syscall.Close(fd)
		}()

		expected := "msg" + string(rune('0'+i))
		if string(msg) != expected {
			t.Errorf("msg[%d]: got %q, want %q", i, msg, expected)
		}
	}
}
