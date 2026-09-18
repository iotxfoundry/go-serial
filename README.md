[![Build Status](https://github.com/bugst/go-serial/workflows/test/badge.svg)](https://github.com/bugst/go-serial/actions?workflow=test)

# go.bug.st/serial

A cross-platform serial port library for Go.

## Documentation and examples

See the package documentation here: https://pkg.go.dev/go.bug.st/serial

## io compatibility (fork additions)

This fork makes the `Port` interface fully compliant with the standard
`io` package contracts (`Port` embeds `io.ReadWriteCloser`), so it can be
safely used with `io.Copy`, `bufio` and the other io helpers:

- `Write` writes the whole buffer or returns a non-nil error when
  `n < len(p)`; the write timeout is a deadline applied to the whole
  `Write` call (net.Conn-like semantics).
- Read/Write timeouts return `os.ErrDeadlineExceeded`
  (detectable with `errors.Is`).
- `Read` with an empty buffer returns `(0, nil)`.

Compared to upstream, the fork additionally provides:

- `SetWriteTimeout(timeout time.Duration)` — write timeout support on
  unix and Windows. Note: this is an API addition; code that implements
  the `Port` interface directly must add this method.
- Support for Exar XR USB serial adapters (`/dev/ttyXRUSB*`) on Linux.

## Credits

:sparkles: Thanks to all awesome [contributors]! :sparkles:

## License

This software is released under the [BSD 3-clause license].

[contributors]: https://github.com/bugst/go-serial/graphs/contributors
[BSD 3-clause license]: https://github.com/bugst/go-serial/blob/master/LICENSE

