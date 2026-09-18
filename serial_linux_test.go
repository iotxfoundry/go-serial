//
// Copyright 2014-2026 Cristian Maglie. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

// Testing code idea and fix thanks to @angri
// https://github.com/bugst/go-serial/pull/42

package serial

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func startSocatAndWaitForPort(t *testing.T, ctx context.Context) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, "socat", "-D", "STDIO", "pty,link=/tmp/faketty")
	r, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Let our fake serial port node appear.
	// socat will write to stderr before starting transfer phase;
	// we don't really care what, just that it did, because then it's ready.
	buf := make([]byte, 1024)
	if _, err = r.Read(buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return cmd
}

// startSocatDualPty creates a pair of connected pseudo-terminals and waits
// until both links are ready. The port under test is opened on portPath,
// the peer endpoint on peerPath can be opened as a regular file.
func startSocatDualPty(t *testing.T, ctx context.Context, portPath, peerPath string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, "socat", "-D",
		"pty,link="+portPath+",raw,echo=0",
		"pty,link="+peerPath+",raw,echo=0")
	r, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	buf := make([]byte, 1024)
	if _, err = r.Read(buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return cmd
}

func TestSerialReadAndCloseConcurrency(t *testing.T) {

	// Run this test with race detector to actually test that
	// the correct multitasking behaviour is happening.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := startSocatAndWaitForPort(t, ctx)
	go cmd.Wait()

	port, err := Open("/tmp/faketty", &Mode{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	buf := make([]byte, 100)
	go port.Read(buf)
	// let port.Read to start
	time.Sleep(time.Millisecond * 1)
	port.Close()
}

func TestDoubleCloseIsNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := startSocatAndWaitForPort(t, ctx)
	go cmd.Wait()

	port, err := Open("/tmp/faketty", &Mode{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := port.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := port.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIoCopyFullBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := startSocatDualPty(t, ctx, "/tmp/faketty", "/tmp/peertty")
	go cmd.Wait()

	port, err := Open("/tmp/faketty", &Mode{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer port.Close()

	peer, err := os.OpenFile("/tmp/peertty", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer peer.Close()

	payload := make([]byte, 1<<20) // 1MB, way larger than any pty buffer
	for i := range payload {
		payload[i] = byte(i)
	}

	received := make([]byte, len(payload))
	done := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond) // let the tty buffer fill up
		_, err := io.ReadFull(peer, received)
		done <- err
	}()

	// io.Copy relies on the io.Writer contract: a short write with a nil
	// error would make it fail with io.ErrShortWrite.
	n, err := io.Copy(port, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("io.Copy wrote %d bytes, expected %d", n, len(payload))
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for peer to receive all data")
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("received data mismatch")
	}
}

func TestWriteTimeoutShortWriteReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := startSocatDualPty(t, ctx, "/tmp/faketty", "/tmp/peertty")
	go cmd.Wait()

	port, err := Open("/tmp/faketty", &Mode{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer port.Close()

	// Open the peer but never read from it, so the tty buffer fills up.
	peer, err := os.OpenFile("/tmp/peertty", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer peer.Close()

	if err := port.SetWriteTimeout(50 * time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	payload := make([]byte, 1<<20)
	n, err := port.Write(payload)
	if n < 0 || n > len(payload) {
		t.Fatalf("invalid byte count: %d", n)
	}
	if n == len(payload) {
		t.Fatal("expected a short write, all bytes reported written")
	}
	// io.Writer requires a non-nil error when n < len(p)
	if err == nil {
		t.Fatal("expected an error on short write, got nil")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected os.ErrDeadlineExceeded, got %v", err)
	}
}

func TestReadEmptyBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := startSocatDualPty(t, ctx, "/tmp/faketty", "/tmp/peertty")
	go cmd.Wait()

	port, err := Open("/tmp/faketty", &Mode{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer port.Close()

	// io.Reader: an empty buffer read must return (0, nil)
	if n, err := port.Read(make([]byte, 0)); n != 0 || err != nil {
		t.Fatalf("Read([]) = (%d, %v), expected (0, nil)", n, err)
	}
	if n, err := port.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = (%d, %v), expected (0, nil)", n, err)
	}
}

func TestWriteAfterClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := startSocatAndWaitForPort(t, ctx)
	go cmd.Wait()

	port, err := Open("/tmp/faketty", &Mode{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := port.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	n, err := port.Write([]byte("test"))
	if err == nil {
		t.Fatal("expected an error writing to a closed port, got nil")
	}
	var portErr *PortError
	if !errors.As(err, &portErr) || portErr.Code() != PortClosed {
		t.Fatalf("expected PortError{PortClosed}, got %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes written, got %d", n)
	}
}

func TestWriteAndCloseConcurrency(t *testing.T) {
	// Run this test with race detector to actually test that
	// the correct multitasking behaviour is happening.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := startSocatDualPty(t, ctx, "/tmp/faketty", "/tmp/peertty")
	go cmd.Wait()

	port, err := Open("/tmp/faketty", &Mode{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Peer is left closed: the tty buffer fills up and Write blocks.
	done := make(chan error, 1)
	go func() {
		_, err := port.Write(make([]byte, 1<<20))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the Write start and block
	port.Close()

	select {
	case <-done:
		// Write must return with any error once the port is closed.
	case <-time.After(5 * time.Second):
		t.Fatal("Write did not return after Close")
	}
}
