package plugin

import (
	"errors"
	"fmt"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/relational/api"
)

// Health-check and request steps a failure is reported against.
const (
	stepSettings    = "settings"
	stepOpenCluster = "open cluster"
	stepSQL         = "SELECT 1"
	stepOTel        = "OTel store layout"
)

// StepError names the step that failed and keeps its cause.
type StepError struct {
	Step string
	Err  error
}

func (e *StepError) Error() string { return e.Step + ": " + describeError(e.Err) }
func (e *StepError) Unwrap() error { return e.Err }

// errorCode extracts the FDB error code or SQLSTATE an error carries.
func errorCode(err error) string {
	var se *StepError
	if errors.As(err, &se) {
		err = se.Err
	}
	var ae *api.Error
	if errors.As(err, &ae) && ae.Code != "" {
		return "SQLSTATE " + string(ae.Code)
	}
	var fe fdb.Error
	if errors.As(err, &fe) {
		return fmt.Sprintf("FDB error %d", fe.Code)
	}
	var we *wire.FDBError
	if errors.As(err, &we) {
		return fmt.Sprintf("FDB error %d", we.Code)
	}
	return ""
}

// describeError is err's message with its FDB code or SQLSTATE appended
// when the message does not already state it.
func describeError(err error) string {
	msg := err.Error()
	if code := errorCode(err); code != "" && !strings.Contains(msg, code) {
		msg += " (" + code + ")"
	}
	return msg
}
