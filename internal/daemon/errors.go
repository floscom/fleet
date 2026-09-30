package daemon

import (
	"errors"
	"fmt"

	fleetv1 "fleet/gen/fleetv1"
)

// apiError is an error with a protocol ErrorCode.
type apiError struct {
	code fleetv1.ErrorCode
	msg  string
}

func (e *apiError) Error() string { return e.msg }

// errf returns an apiError with a formatted message.
func errf(code fleetv1.ErrorCode, format string, args ...any) error {
	return &apiError{code: code, msg: fmt.Sprintf(format, args...)}
}

// errorCode maps err to its protocol code; unknown errors are INTERNAL.
func errorCode(err error) (fleetv1.ErrorCode, string) {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.code, ae.msg
	}
	return fleetv1.ErrorCode_ERROR_CODE_INTERNAL, err.Error()
}

// Shorthands for the codes used most.
const (
	codeInternal    = fleetv1.ErrorCode_ERROR_CODE_INTERNAL
	codeInvalid     = fleetv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT
	codeNotFound    = fleetv1.ErrorCode_ERROR_CODE_NOT_FOUND
	codeExists      = fleetv1.ErrorCode_ERROR_CODE_ALREADY_EXISTS
	codeUnauth      = fleetv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED
	codeDenied      = fleetv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED
	codeOutside     = fleetv1.ErrorCode_ERROR_CODE_OUTSIDE_ROOTS
	codeUnavailable = fleetv1.ErrorCode_ERROR_CODE_ADAPTER_UNAVAILABLE
	codePairing     = fleetv1.ErrorCode_ERROR_CODE_PAIRING_FAILED
)
