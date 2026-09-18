//
// Copyright 2014-2026 Cristian Maglie. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

//go:build linux || darwin || freebsd || openbsd

package serial

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial/unixutils"
	"golang.org/x/sys/unix"
)

type unixPort struct {
	handle int

	readTimeout  time.Duration
	writeTimeout time.Duration
	// timeoutMu protects readTimeout and writeTimeout from concurrent
	// access between Set*Timeout and Read/Write. It must not be the same
	// lock as closeLock, otherwise Set*Timeout would block behind a Read
	// held in select for the whole timeout.
	timeoutMu sync.Mutex

	closeLock   sync.RWMutex
	closeSignal *unixutils.Pipe
	opened      uint32
}

func (port *unixPort) Close() error {
	if !atomic.CompareAndSwapUint32(&port.opened, 1, 0) {
		return nil
	}

	port.releaseExclusiveAccess()

	if port.closeSignal != nil {
		// Send close signal to all pending Read/Write (if any)
		port.closeSignal.Write([]byte{0})

		// Wait for all pending Read/Write to complete. This cannot
		// deadlock because the port is in non-blocking mode: Read/Write
		// sleep in select(2), which the signal above wakes up, they never
		// sleep inside a read/write syscall.
		port.closeLock.Lock()
		defer port.closeLock.Unlock()

		// No syscall can be in flight on the handle anymore: close it now
		// so the descriptor number cannot be reused while Read/Write are
		// still running. Also close the signaling pipe even if closing
		// the handle fails, so blocked operations are always woken up.
		if err := unix.Close(port.handle); err != nil {
			port.closeSignal.Close()
			return err
		}
		return port.closeSignal.Close()
	}
	return unix.Close(port.handle)
}

func (port *unixPort) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	port.closeLock.RLock()
	defer port.closeLock.RUnlock()
	if atomic.LoadUint32(&port.opened) != 1 {
		return 0, &PortError{code: PortClosed}
	}

	port.timeoutMu.Lock()
	readTimeout := port.readTimeout
	port.timeoutMu.Unlock()

	var deadline time.Time
	if readTimeout != NoTimeout {
		deadline = time.Now().Add(readTimeout)
	}

	fds := unixutils.NewFDSet(port.handle, port.closeSignal.ReadFD())
	for {
		timeout := time.Duration(-1)
		if readTimeout != NoTimeout {
			timeout = time.Until(deadline)
			if timeout < 0 {
				// a negative timeout means "no-timeout" in Select(...)
				timeout = 0
			}
		}
		res, err := unixutils.Select(fds, nil, fds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if res.IsReadable(port.closeSignal.ReadFD()) {
			return 0, &PortError{code: PortClosed}
		}
		if !res.IsReadable(port.handle) {
			// Timeout happened
			return 0, os.ErrDeadlineExceeded
		}
		n, err := unix.Read(port.handle, p)
		if err == unix.EINTR {
			continue
		}
		if err == unix.EAGAIN {
			// Spurious readability notification on the non-blocking port
			continue
		}
		// Linux: when the port is disconnected during a read operation
		// the port is left in a "readable with zero-length-data" state.
		// https://stackoverflow.com/a/34945814/1655275
		if n == 0 && err == nil {
			return 0, &PortError{code: PortClosed}
		}
		if n < 0 { // Do not return -1 unix errors
			n = 0
		}
		return n, err
	}
}

func (port *unixPort) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	port.closeLock.RLock()
	defer port.closeLock.RUnlock()
	if atomic.LoadUint32(&port.opened) != 1 {
		return 0, &PortError{code: PortClosed}
	}

	port.timeoutMu.Lock()
	writeTimeout := port.writeTimeout
	port.timeoutMu.Unlock()

	// The write timeout is an overall deadline for the whole Write call
	// (net.Conn-like semantics), so it is tracked across the loop below
	// where the buffer may be written in multiple chunks.
	var deadline time.Time
	if writeTimeout != NoTimeout {
		deadline = time.Now().Add(writeTimeout)
	}

	// Wait for the port to become writable or for the close signal.
	// The read end of the close signal pipe is watched for readiness:
	// Close() writes one byte into the pipe to wake up this select and
	// report the port as closed. The pipe write end must NOT be put in
	// the write set since an empty pipe is always writable and would
	// prevent the select from blocking at all.
	rfds := unixutils.NewFDSet(port.closeSignal.ReadFD())
	wfds := unixutils.NewFDSet(port.handle)
	total := 0
	for total < len(p) {
		timeout := time.Duration(-1)
		if !deadline.IsZero() {
			timeout = time.Until(deadline)
			if timeout < 0 {
				// a negative timeout means "no-timeout" in Select(...)
				timeout = 0
			}
		}
		res, err := unixutils.Select(rfds, wfds, wfds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return total, err
		}
		if res.IsReadable(port.closeSignal.ReadFD()) {
			return total, &PortError{code: PortClosed}
		}
		if res.IsError(port.handle) {
			// The port reported an exceptional condition (e.g. it has
			// been disconnected)
			return total, &PortError{code: PortClosed}
		}
		if !res.IsWritable(port.handle) {
			// Timeout happened
			return total, os.ErrDeadlineExceeded
		}
		n, err := unix.Write(port.handle, p[total:])
		if err == unix.EINTR {
			continue
		}
		if err == unix.EAGAIN {
			// Not enough space in the port buffer: wait for writability.
			continue
		}
		if n < 0 { // Do not return -1 unix errors
			n = 0
		}
		total += n
		// Linux: when the port is disconnected during a read operation
		// the port is left in a "readable with zero-length-data" state.
		// https://stackoverflow.com/a/34945814/1655275
		if n == 0 && err == nil {
			return total, &PortError{code: PortClosed}
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (port *unixPort) Break(t time.Duration) error {
	if err := unix.IoctlSetInt(port.handle, ioctlTiocsbrk, 0); err != nil {
		return err
	}

	time.Sleep(t)

	if err := unix.IoctlSetInt(port.handle, ioctlTioccbrk, 0); err != nil {
		return err
	}

	return nil
}

func (port *unixPort) SetMode(mode *Mode) error {
	settings, err := port.getTermSettings()
	if err != nil {
		return err
	}
	if err := setTermSettingsParity(mode.Parity, settings); err != nil {
		return err
	}
	if err := setTermSettingsDataBits(mode.DataBits, settings); err != nil {
		return err
	}
	if err := setTermSettingsStopBits(mode.StopBits, settings); err != nil {
		return err
	}
	requireSpecialBaudrate := false
	if err, special := setTermSettingsBaudrate(mode.BaudRate, settings); err != nil {
		return err
	} else if special {
		requireSpecialBaudrate = true
	}
	if err := port.setTermSettings(settings); err != nil {
		return err
	}
	if requireSpecialBaudrate {
		// MacOSX require this one to be the last operation otherwise an
		// 'Invalid serial port' error is produced.
		if err := port.setSpecialBaudrate(uint32(mode.BaudRate)); err != nil {
			return err
		}
	}
	return nil
}

func (port *unixPort) SetDTR(dtr bool) error {
	status, err := port.getModemBitsStatus()
	if err != nil {
		return err
	}
	if dtr {
		status |= unix.TIOCM_DTR
	} else {
		status &^= unix.TIOCM_DTR
	}
	return port.setModemBitsStatus(status)
}

func (port *unixPort) SetRTS(rts bool) error {
	status, err := port.getModemBitsStatus()
	if err != nil {
		return err
	}
	if rts {
		status |= unix.TIOCM_RTS
	} else {
		status &^= unix.TIOCM_RTS
	}
	return port.setModemBitsStatus(status)
}

func (port *unixPort) SetReadTimeout(timeout time.Duration) error {
	if timeout < 0 && timeout != NoTimeout {
		return &PortError{code: InvalidTimeoutValue}
	}
	port.timeoutMu.Lock()
	port.readTimeout = timeout
	port.timeoutMu.Unlock()
	return nil
}

func (port *unixPort) SetWriteTimeout(timeout time.Duration) error {
	if timeout < 0 && timeout != NoTimeout {
		return &PortError{code: InvalidTimeoutValue}
	}
	port.timeoutMu.Lock()
	port.writeTimeout = timeout
	port.timeoutMu.Unlock()
	return nil
}

func (port *unixPort) GetModemStatusBits() (*ModemStatusBits, error) {
	status, err := port.getModemBitsStatus()
	if err != nil {
		return nil, err
	}
	return &ModemStatusBits{
		CTS: (status & unix.TIOCM_CTS) != 0,
		DCD: (status & unix.TIOCM_CD) != 0,
		DSR: (status & unix.TIOCM_DSR) != 0,
		RI:  (status & unix.TIOCM_RI) != 0,
	}, nil
}

func nativeOpen(portName string, mode *Mode) (*unixPort, error) {
	h, err := unix.Open(portName, unix.O_RDWR|unix.O_NOCTTY|unix.O_NDELAY, 0)
	if err != nil {
		switch err {
		case unix.EBUSY:
			return nil, &PortError{code: PortBusy}
		case unix.EACCES:
			return nil, &PortError{code: PermissionDenied}
		}
		return nil, err
	}
	port := &unixPort{
		handle:       h,
		opened:       1,
		readTimeout:  NoTimeout,
		writeTimeout: NoTimeout,
	}

	// Setup serial port
	settings, err := port.getTermSettings()
	if err != nil {
		port.Close()
		return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error getting term settings: %w", err)}
	}

	// Set raw mode
	setRawMode(settings)

	// Explicitly disable RTS/CTS flow control
	setTermSettingsCtsRts(false, settings)

	if err = port.setTermSettings(settings); err != nil {
		port.Close()
		return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error setting term settings: %w", err)}
	}

	if mode.InitialStatusBits != nil {
		status, err := port.getModemBitsStatus()
		if err != nil {
			port.Close()
			return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error getting modem bits status: %w", err)}
		}
		if mode.InitialStatusBits.DTR {
			status |= unix.TIOCM_DTR
		} else {
			status &^= unix.TIOCM_DTR
		}
		if mode.InitialStatusBits.RTS {
			status |= unix.TIOCM_RTS
		} else {
			status &^= unix.TIOCM_RTS
		}
		if err := port.setModemBitsStatus(status); err != nil {
			port.Close()
			return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error setting modem bits status: %w", err)}
		}
	}

	// MacOSX require that this operation is the last one otherwise an
	// 'Invalid serial port' error is returned... don't know why...
	if err := port.SetMode(mode); err != nil {
		port.Close()
		return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error configuring port: %w", err)}
	}

	// Keep the port in non-blocking mode: reads and writes are gated by
	// the select(2) loops in Read/Write. This is required to give the
	// write timeout a chance to trigger: with a blocking file descriptor
	// a write(2) to a serial port whose buffer is full would sleep inside
	// the kernel until the whole buffer can be transferred, ignoring any
	// deadline. unix.Write then returns EAGAIN when the buffer is full
	// and the select loop below waits for writability instead.
	unix.SetNonblock(h, true)

	port.acquireExclusiveAccess()

	// This pipe is used as a signal to cancel blocking Read
	if pipe, err := unixutils.NewPipe(); err != nil {
		port.Close()
		return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error opening signaling pipe: %w", err)}
	} else {
		port.closeSignal = pipe
	}

	return port, nil
}

func nativeGetPortsList() ([]string, error) {
	files, err := os.ReadDir(devFolder)
	if err != nil {
		return nil, err
	}

	ports := make([]string, 0, len(files))
	for _, f := range files {
		// Skip folders
		if f.IsDir() {
			continue
		}

		// Keep only devices with the correct name
		if !osPortFilter.MatchString(f.Name()) {
			continue
		}

		portName := devFolder + "/" + f.Name()

		// Check if serial port is real or is a placeholder serial port "ttySxx" or "ttyHSxx"
		if strings.HasPrefix(f.Name(), "ttyS") || strings.HasPrefix(f.Name(), "ttyHS") {
			port, err := nativeOpen(portName, &Mode{})
			if err != nil {
				continue
			} else {
				port.Close()
			}
		}

		// Save serial port in the resulting list
		ports = append(ports, portName)
	}

	return ports, nil
}

// termios manipulation functions

func setTermSettingsParity(parity Parity, settings *unix.Termios) error {
	switch parity {
	case NoParity:
		settings.Cflag &^= unix.PARENB
		settings.Cflag &^= unix.PARODD
		settings.Cflag &^= tcCMSPAR
		settings.Iflag &^= unix.INPCK
	case OddParity:
		settings.Cflag |= unix.PARENB
		settings.Cflag |= unix.PARODD
		settings.Cflag &^= tcCMSPAR
		settings.Iflag |= unix.INPCK
	case EvenParity:
		settings.Cflag |= unix.PARENB
		settings.Cflag &^= unix.PARODD
		settings.Cflag &^= tcCMSPAR
		settings.Iflag |= unix.INPCK
	case MarkParity:
		if tcCMSPAR == 0 {
			return &PortError{code: InvalidParity}
		}
		settings.Cflag |= unix.PARENB
		settings.Cflag |= unix.PARODD
		settings.Cflag |= tcCMSPAR
		settings.Iflag |= unix.INPCK
	case SpaceParity:
		if tcCMSPAR == 0 {
			return &PortError{code: InvalidParity}
		}
		settings.Cflag |= unix.PARENB
		settings.Cflag &^= unix.PARODD
		settings.Cflag |= tcCMSPAR
		settings.Iflag |= unix.INPCK
	default:
		return &PortError{code: InvalidParity}
	}
	return nil
}

func setTermSettingsDataBits(bits int, settings *unix.Termios) error {
	databits, ok := databitsMap[bits]
	if !ok {
		return &PortError{code: InvalidDataBits}
	}
	// Remove previous databits setting
	settings.Cflag &^= unix.CSIZE
	// Set requested databits
	settings.Cflag |= databits
	return nil
}

func setTermSettingsStopBits(bits StopBits, settings *unix.Termios) error {
	switch bits {
	case OneStopBit:
		settings.Cflag &^= unix.CSTOPB
	case OnePointFiveStopBits:
		return &PortError{code: InvalidStopBits}
	case TwoStopBits:
		settings.Cflag |= unix.CSTOPB
	default:
		return &PortError{code: InvalidStopBits}
	}
	return nil
}

func setTermSettingsCtsRts(enable bool, settings *unix.Termios) {
	if enable {
		settings.Cflag |= tcCRTSCTS
	} else {
		settings.Cflag &^= tcCRTSCTS
	}
}

func setRawMode(settings *unix.Termios) {
	// Set local mode
	settings.Cflag |= unix.CREAD
	settings.Cflag |= unix.CLOCAL

	// Set raw mode
	settings.Lflag &^= unix.ICANON
	settings.Lflag &^= unix.ECHO
	settings.Lflag &^= unix.ECHOE
	settings.Lflag &^= unix.ECHOK
	settings.Lflag &^= unix.ECHONL
	settings.Lflag &^= unix.ECHOCTL
	settings.Lflag &^= unix.ECHOPRT
	settings.Lflag &^= unix.ECHOKE
	settings.Lflag &^= unix.ISIG
	settings.Lflag &^= unix.IEXTEN

	settings.Iflag &^= unix.IXON
	settings.Iflag &^= unix.IXOFF
	settings.Iflag &^= unix.IXANY
	settings.Iflag &^= unix.INPCK
	settings.Iflag &^= unix.IGNPAR
	settings.Iflag &^= unix.PARMRK
	settings.Iflag &^= unix.ISTRIP
	settings.Iflag &^= unix.IGNBRK
	settings.Iflag &^= unix.BRKINT
	settings.Iflag &^= unix.INLCR
	settings.Iflag &^= unix.IGNCR
	settings.Iflag &^= unix.ICRNL
	settings.Iflag &^= tcIUCLC

	settings.Oflag &^= unix.OPOST

	// Block reads until at least one char is available (no timeout)
	settings.Cc[unix.VMIN] = 1
	settings.Cc[unix.VTIME] = 0
}

// native syscall wrapper functions

func (port *unixPort) getTermSettings() (*unix.Termios, error) {
	return unix.IoctlGetTermios(port.handle, ioctlTcgetattr)
}

func (port *unixPort) setTermSettings(settings *unix.Termios) error {
	return unix.IoctlSetTermios(port.handle, ioctlTcsetattr, settings)
}

func (port *unixPort) getModemBitsStatus() (int, error) {
	return unix.IoctlGetInt(port.handle, unix.TIOCMGET)
}

func (port *unixPort) setModemBitsStatus(status int) error {
	return unix.IoctlSetPointerInt(port.handle, unix.TIOCMSET, status)
}

func (port *unixPort) acquireExclusiveAccess() error {
	return unix.IoctlSetInt(port.handle, unix.TIOCEXCL, 0)
}

func (port *unixPort) releaseExclusiveAccess() error {
	return unix.IoctlSetInt(port.handle, unix.TIOCNXCL, 0)
}
