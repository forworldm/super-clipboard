// Package apperr carries the error types that the Python backend expressed with
// built-in exceptions (ValueError) and FastAPI's HTTPException.
//
// Keeping them in a tiny shared package avoids import cycles between the
// repository, storage, utils and api layers, exactly like the flat Python
// package did.
package apperr

import "fmt"

// ValueError mirrors Python's `ValueError`. The API layer translates it into a
// 400/409 response whose body is {"detail": <message>}.
type ValueError struct {
	Message string
}

// NewValueError builds a ValueError with a formatted message.
func NewValueError(format string, args ...interface{}) *ValueError {
	return &ValueError{Message: fmt.Sprintf(format, args...)}
}

func (e *ValueError) Error() string { return e.Message }

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
