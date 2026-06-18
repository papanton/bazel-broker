// Package cli holds brokerctl's command logic: config loading, the broker
// HTTP+WS client, output rendering, and stable exit codes. The cobra wiring in
// cmd/brokerctl is a thin dispatcher over this package so the logic stays
// unit-testable without cobra.
package cli

import (
	"errors"
	"fmt"
	"io"
)

// Exit codes are a STABLE scripting/`/verify` contract. A cliError carries a
// code; any other error maps to ExitUsage (1).
const (
	ExitOK             = 0 // success
	ExitUsage          = 1 // generic / usage error (also cobra arg errors)
	ExitConfig         = 2 // config missing / unreadable / invalid
	ExitUnavailable    = 3 // broker unreachable (connection refused / timeout / WS dial)
	ExitAuth           = 4 // broker rejected the token (401), incl. on the WS upgrade
	ExitBroker         = 5 // broker error: other 4xx (incl. 404), any 5xx except 501, undecodable
	ExitNotImplemented = 6 // endpoint reserved on this broker (HTTP 501; E3/E4/E5 not landed)
	ExitOpen           = 7 // `open` (Perfetto) failed
)

// cliError carries an exit code alongside its message.
type cliError struct {
	code int
	msg  string
}

func (e *cliError) Error() string { return e.msg }

// Code returns the exit code carried by a cliError.
func (e *cliError) Code() int { return e.code }

// wrap builds a cliError with a formatted message. %w is supported via fmt.Errorf
// semantics (the wrapped error is folded into the message string).
func wrap(code int, format string, a ...any) error {
	return &cliError{code: code, msg: fmt.Sprintf(format, a...)}
}

// ExitCodeFor prints err to stderr (prefixed "brokerctl:") and returns its exit
// code: the cliError's code, or ExitUsage for any other error. main() calls this.
func ExitCodeFor(w io.Writer, err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *cliError
	if errors.As(err, &ce) {
		fmt.Fprintln(w, "brokerctl: "+ce.msg)
		return ce.code
	}
	fmt.Fprintln(w, "brokerctl: "+err.Error())
	return ExitUsage
}
