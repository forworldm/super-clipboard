package apperr

import (
	"errors"
	"testing"
)

func TestValueError(t *testing.T) {
	err := NewValueError("直链码已存在，请刷新后再试")
	if err.Error() != "直链码已存在，请刷新后再试" {
		t.Fatalf("unexpected message %q", err.Error())
	}
	var valueError *ValueError
	if !errors.As(err, &valueError) {
		t.Fatalf("expected errors.As to unwrap ValueError")
	}
}

func TestHTTPError(t *testing.T) {
	err := NewHTTPError(404, "片段未找到")
	if err.Status != 404 || err.Detail != "片段未找到" {
		t.Fatalf("unexpected HTTPError %+v", err)
	}
	if err.Error() != "HTTP 404: 片段未找到" {
		t.Fatalf("unexpected Error() %q", err.Error())
	}
}
