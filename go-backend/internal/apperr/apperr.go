// Package apperr carries the error types that the Python backend expressed with
// built-in exceptions (ValueError) and FastAPI's HTTPException.
//
// Keeping them in a tiny shared package avoids import cycles between the
// repository, storage, utils and api layers, exactly like the flat Python
// package did.
package apperr

import (
	"errors"
	"fmt"
)

// ValueCode is a stable, language-agnostic classifier for ValueError.
type ValueCode string

const (
	CodeTokenInvalid       ValueCode = "token_invalid"
	CodeTokenVerifyFailed  ValueCode = "token_verify_failed"
	CodeTokenNotRegistered ValueCode = "token_not_registered"
	CodeTokenExpired       ValueCode = "token_expired"
	CodeTokenOccupied      ValueCode = "token_occupied"
	CodeAccessCodeConflict ValueCode = "access_code_conflict"
	CodeInvalidExpiresAt   ValueCode = "invalid_expires_at"
	CodeMissingEnvironment ValueCode = "missing_environment"
)

// ValueError mirrors Python's `ValueError`. The API layer translates it into a
// 400/409 response whose body is {"detail": <message>}.
type ValueError struct {
	Code    ValueCode
	Message string
}

// NewValueError builds a ValueError with a formatted message.
func NewValueError(format string, args ...interface{}) *ValueError {
	return &ValueError{Message: fmt.Sprintf(format, args...)}
}

// NewValueErrorCode builds a ValueError with a stable error code.
func NewValueErrorCode(code ValueCode, format string, args ...interface{}) *ValueError {
	return &ValueError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *ValueError) Error() string { return e.Message }

// IsValueCode checks whether err is a ValueError with one of the given codes.
func IsValueCode(err error, codes ...ValueCode) bool {
	var valueErr *ValueError
	if !errors.As(err, &valueErr) {
		return false
	}
	for _, code := range codes {
		if valueErr.Code == code {
			return true
		}
	}
	return false
}

// HTTPError mirrors fastapi.HTTPException: a status code plus a detail payload
// that is serialized as {"detail": <detail>}.
type HTTPError struct {
	Status int
	Detail string
}

// NewHTTPError builds an HTTPError.
func NewHTTPError(status int, detail string) *HTTPError {
	return &HTTPError{Status: status, Detail: detail}
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Detail)
}
