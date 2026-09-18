//
// Copyright 2014-2026 Cristian Maglie. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

package serial

import (
	"errors"
	"os"
	"testing"
	"time"
)

// TestOpenNonExistentPortReturnsPortNotFound verifies the error path when
// opening a port that does not exist.
func TestOpenNonExistentPortReturnsPortNotFound(t *testing.T) {
	_, err := Open("COM999", &Mode{BaudRate: 9600})
	var portErr *PortError
	if !errors.As(err, &portErr) {
		t.Fatalf("expected a PortError, got %v", err)
	}
	if portErr.Code() != PortNotFound {
		t.Fatalf("expected PortNotFound, got %v", portErr.Code())
	}
}

// TestWindowsPortFunctionality runs a functional check of the io contract
// on the first serial port available. The test is skipped when no port is
// available (e.g. on CI machines without serial ports).
//
// NOTE: the write subtest sends a test pattern to the port; on a machine
// where the port is connected to an active device this may disturb it.
func TestWindowsPortFunctionality(t *testing.T) {
	ports, err := GetPortsList()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ports) == 0 {
		t.Skip("no serial ports available")
	}
	portName := ports[0]

	port, err := Open(portName, &Mode{BaudRate: 115200})
	if err != nil {
		t.Skipf("cannot open %s: %v", portName, err)
	}
	defer port.Close()

	// Read timeout: with no data the Read must return os.ErrDeadlineExceeded
	// after about the configured timeout.
	t.Run("ReadTimeoutReturnsErrDeadlineExceeded", func(t *testing.T) {
		if err := port.SetReadTimeout(200 * time.Millisecond); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Drain any pending data first: a device attached to the port may
		// have sent something in the meantime.
		drain := make([]byte, 4096)
		for i := 0; i < 10; i++ {
			n, err := port.Read(drain)
			if err != nil {
				break // timeout reached: buffer is empty
			}
			if n == 0 {
				break
			}
		}

		start := time.Now()
		n, err := port.Read(drain)
		elapsed := time.Since(start)
		if err == nil {
			t.Skipf("port %s is receiving unsolicited data, timeout path cannot be tested", portName)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("expected os.ErrDeadlineExceeded, got %v (n=%d)", err, n)
		}
		if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("expected ~200ms, got %v", elapsed)
		}
	})

	// An empty buffer Read must return (0, nil).
	t.Run("ReadEmptyBuffer", func(t *testing.T) {
		n, err := port.Read(make([]byte, 0))
		if n != 0 || err != nil {
			t.Fatalf("Read([]) = (%d, %v), expected (0, nil)", n, err)
		}
		n, err = port.Read(nil)
		if n != 0 || err != nil {
			t.Fatalf("Read(nil) = (%d, %v), expected (0, nil)", n, err)
		}
	})

	// io.Writer contract: Write must transfer the whole buffer or return
	// a non-nil error when n < len(p).
	t.Run("WriteFullBuffer", func(t *testing.T) {
		// 8KB exercises the chunked write loop. Allow plenty of time for
		// a real 115200 baud port (8KB ~= 0.7s).
		if err := port.SetWriteTimeout(10 * time.Second); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		payload := make([]byte, 8192)
		for i := range payload {
			payload[i] = byte(i)
		}
		n, err := port.Write(payload)
		if n != len(payload) || err != nil {
			t.Fatalf("Write = (%d, %v), expected (%d, nil)", n, err, len(payload))
		}
	})

	t.Run("GetModemStatusBits", func(t *testing.T) {
		if _, err := port.GetModemStatusBits(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("Drain", func(t *testing.T) {
		if err := port.Drain(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// Closing twice must be a no-op.
	t.Run("CloseIsIdempotent", func(t *testing.T) {
		if err := port.Close(); err != nil {
			t.Fatalf("unexpected error closing the port: %v", err)
		}
		if err := port.Close(); err != nil {
			t.Fatalf("unexpected error closing the port twice: %v", err)
		}
	})

	// Read/Write after Close must return PortClosed.
	t.Run("WriteAfterClose", func(t *testing.T) {
		n, err := port.Write([]byte("test"))
		var portErr *PortError
		if !errors.As(err, &portErr) || portErr.Code() != PortClosed {
			t.Fatalf("expected PortError{PortClosed}, got %v (n=%d)", err, n)
		}
	})

	t.Run("ReadAfterClose", func(t *testing.T) {
		n, err := port.Read(make([]byte, 16))
		var portErr *PortError
		if !errors.As(err, &portErr) || portErr.Code() != PortClosed {
			t.Fatalf("expected PortError{PortClosed}, got %v (n=%d)", err, n)
		}
	})
}
