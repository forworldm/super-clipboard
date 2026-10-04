// Package schemas is the Go port of backend/schemas.py.
//
// The pydantic models are reproduced with explicit parsing helpers so that the
// HTTP surface stays identical: valid payloads produce the same camelCase JSON
// and invalid payloads produce FastAPI's 422 body, i.e.
// {"detail": [{"type": ..., "loc": [...], "msg": ..., "input": ...}]}.
package schemas

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// pydanticDocBase is the URL prefix pydantic v2 puts in the "url" field.
const pydanticDocBase = "https://errors.pydantic.dev/2.12/v/"

// knownPydanticErrors lists the built-in error types that carry a doc URL.
var knownPydanticErrors = map[string]bool{
	"missing":            true,
	"string_type":        true,
	"string_too_short":   true,
	"string_too_long":    true,
	"int_type":           true,
	"int_parsing":        true,
	"int_from_float":     true,
	"greater_than":       true,
	"greater_than_equal": true,
	"literal_error":      true,
	"value_error":        true,
	"model_type":         true,
	"json_invalid":       true,
}

// ErrorDetail mirrors one entry of FastAPI's RequestValidationError payload.
type ErrorDetail struct {
	Type  string        `json:"type"`
	Loc   []interface{} `json:"loc"`
	Msg   string        `json:"msg"`
	Input interface{}   `json:"input"`
	Ctx   interface{}   `json:"ctx,omitempty"`
	URL   string        `json:"url,omitempty"`
}

// ValidationError mirrors pydantic's ValidationError; the API layer renders it
// as a 422 response.
type ValidationError struct {
	Details []ErrorDetail
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Details))
	for _, detail := range e.Details {
		parts = append(parts, fmt.Sprintf("%v: %s", detail.Loc, detail.Msg))
	}
	return strings.Join(parts, "; ")
}

// IsEmpty reports whether no error was collected.
func (e *ValidationError) IsEmpty() bool { return e == nil || len(e.Details) == 0 }

func newValidationError(details ...ErrorDetail) *ValidationError {
	return &ValidationError{Details: details}
}

func detail(kind string, loc []interface{}, msg string, input interface{}) ErrorDetail {
	item := ErrorDetail{Type: kind, Loc: loc, Msg: msg, Input: input}
	if knownPydanticErrors[kind] {
		item.URL = pydanticDocBase + kind
	}
	return item
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func decodeInput(raw json.RawMessage) interface{} {
	if raw == nil {
		return nil
	}
	var value interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	return value
}

func isNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// objectFields is the working set of a decoded JSON object plus the validation
// errors collected while reading its members.
type objectFields struct {
	loc  []interface{}
	raw  map[string]json.RawMessage
	errs []ErrorDetail
}

func parseObject(body []byte, loc []interface{}) (*objectFields, *ValidationError) {
	trimmed := bytesTrimSpace(body)
	if len(trimmed) == 0 {
		return nil, newValidationError(detail("json_invalid", loc, "JSON decode error", nil))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, newValidationError(ErrorDetail{
			Type:  "json_invalid",
			Loc:   loc,
			Msg:   "JSON decode error",
			Input: string(trimmed),
			URL:   pydanticDocBase + "json_invalid",
		})
	}
	return &objectFields{loc: loc, raw: fields}, nil
}

func bytesTrimSpace(body []byte) []byte {
	start := 0
	end := len(body)
	for start < end && unicode.IsSpace(rune(body[start])) {
		start++
	}
	for end > start && unicode.IsSpace(rune(body[end-1])) {
		end--
	}
	return body[start:end]
}

func (o *objectFields) add(item ErrorDetail) { o.errs = append(o.errs, item) }

func (o *objectFields) path(name string) []interface{} {
	path := make([]interface{}, 0, len(o.loc)+1)
	path = append(path, o.loc...)
	return append(path, name)
}

func (o *objectFields) result() *ValidationError {
	if len(o.errs) == 0 {
		return nil
	}
	return &ValidationError{Details: o.errs}
}

// requiredString implements `name: str = Field(min_length=..., max_length=...)`.
// maxLen == 0 means "unbounded", which is only acceptable for fields that are
// bounded by the request body itself; prefer requiredBound with an explicit
// byte cap for anything a client can inflate.
func (o *objectFields) requiredString(name string, minLen, maxLen int) (string, bool) {
	return o.requiredBound(name, stringBound{MinRunes: minLen, MaxRunes: maxLen})
}

// optionalString implements `name: Optional[str] = Field(default=None, ...)`.
// A missing key or an explicit null both yield (nil, true).
func (o *objectFields) optionalString(name string, minLen, maxLen int) (*string, bool) {
	return o.optionalBound(name, stringBound{MinRunes: minLen, MaxRunes: maxLen})
}

// requiredBound is requiredString with both a character bound (pydantic's
// max_length) and a UTF-8 byte bound (see stringBound).
func (o *objectFields) requiredBound(name string, bound stringBound) (string, bool) {
	raw, present := o.raw[name]
	if !present {
		o.add(detail("missing", o.path(name), "Field required", nil))
		return "", false
	}
	value, ok := o.readString(name, raw)
	if !ok {
		return "", false
	}
	return value, o.checkBound(name, value, bound)
}

// optionalBound is optionalString with both a character and a byte bound.
// A missing key or an explicit null both yield (nil, true).
func (o *objectFields) optionalBound(name string, bound stringBound) (*string, bool) {
	raw, present := o.raw[name]
	if !present || isNull(raw) {
		return nil, true
	}
	value, ok := o.readString(name, raw)
	if !ok {
		return nil, false
	}
	if !o.checkBound(name, value, bound) {
		return nil, false
	}
	return &value, true
}

func (o *objectFields) readString(name string, raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		o.add(detail("string_type", o.path(name), "Input should be a valid string", decodeInput(raw)))
		return "", false
	}
	return value, true
}

func (o *objectFields) checkStringBounds(name string, value string, minLen, maxLen int) bool {
	return o.checkBound(name, value, stringBound{MinRunes: minLen, MaxRunes: maxLen})
}

// stringBound is the accepted size of one client supplied string.
//
// MaxRunes mirrors pydantic's max_length (characters) so the historical error
// messages and the frontend's error mapping stay intact. MaxBytes is the UTF-8
// byte ceiling that actually protects the process: without it a payload of
// 4-byte characters would pass a character count while costing four times the
// memory, the JSON body and the database row. Zero means "no bound".
type stringBound struct {
	MinRunes int
	MaxRunes int
	MaxBytes int
	// TooLongMessage replaces the generic wording when either upper bound is
	// exceeded. Fields whose bound is a usability decision rather than a pure
	// format rule (oversized clip text, inline file data) use it to name the
	// supported alternative instead of only reporting a number.
	TooLongMessage string
}

// checkBound enforces a stringBound. The character check keeps pydantic's
// exact wording, and both overflow paths report `string_too_long` so clients
// keep treating them as the same size rejection.
func (o *objectFields) checkBound(name string, value string, bound stringBound) bool {
	length := utf8.RuneCountInString(value)
	if bound.MinRunes > 0 && length < bound.MinRunes {
		o.add(detail("string_too_short", o.path(name),
			fmt.Sprintf("String should have at least %d character%s", bound.MinRunes, plural(bound.MinRunes)), value))
		return false
	}
	if bound.MaxRunes > 0 && length > bound.MaxRunes {
		o.add(detail("string_too_long", o.path(name), bound.tooLongMessage(func() string {
			return fmt.Sprintf("String should have at most %d character%s", bound.MaxRunes, plural(bound.MaxRunes))
		}), value))
		return false
	}
	if bound.MaxBytes > 0 && len(value) > bound.MaxBytes {
		o.add(detail("string_too_long", o.path(name), bound.tooLongMessage(func() string {
			return fmt.Sprintf("String should have at most %d byte%s", bound.MaxBytes, plural(bound.MaxBytes))
		}), value))
		return false
	}
	return true
}

// tooLongMessage returns the custom hint when one is configured.
func (b stringBound) tooLongMessage(fallback func() string) string {
	if b.TooLongMessage != "" {
		return b.TooLongMessage
	}
	return fallback()
}

// requiredInt implements `name: int = Field(gt=..., ge=...)`.
func (o *objectFields) requiredInt(name string, gt, ge *int64) (int64, bool) {
	raw, present := o.raw[name]
	if !present {
		o.add(detail("missing", o.path(name), "Field required", nil))
		return 0, false
	}
	value, ok := o.readInt(name, raw)
	if !ok {
		return 0, false
	}
	return value, o.checkIntBounds(name, value, raw, gt, ge)
}

// optionalInt implements `name: Optional[int] = Field(default=None, gt=...)`.
func (o *objectFields) optionalInt(name string, gt, ge *int64) (*int, bool) {
	raw, present := o.raw[name]
	if !present || isNull(raw) {
		return nil, true
	}
	value, ok := o.readInt(name, raw)
	if !ok {
		return nil, false
	}
	if !o.checkIntBounds(name, value, raw, gt, ge) {
		return nil, false
	}
	asInt := int(value)
	return &asInt, true
}

func (o *objectFields) readInt(name string, raw json.RawMessage) (int64, bool) {
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		if number != math.Trunc(number) || math.IsInf(number, 0) || math.IsNaN(number) {
			o.add(detail("int_from_float", o.path(name),
				"Input should be a valid integer, got a number with a fractional part", number))
			return 0, false
		}
		if number > math.MaxInt64 || number < math.MinInt64 {
			o.add(detail("int_parsing", o.path(name),
				"Input should be a valid integer, unable to represent string as an integer", number))
			return 0, false
		}
		return int64(number), true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		parsed, parseErr := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if parseErr != nil {
			o.add(detail("int_parsing", o.path(name),
				"Input should be a valid integer, unable to parse string as an integer", text))
			return 0, false
		}
		return parsed, true
	}
	o.add(detail("int_type", o.path(name), "Input should be a valid integer", decodeInput(raw)))
	return 0, false
}

func (o *objectFields) checkIntBounds(name string, value int64, raw json.RawMessage, gt, ge *int64) bool {
	if gt != nil && value <= *gt {
		o.add(withCtx(detail("greater_than", o.path(name),
			fmt.Sprintf("Input should be greater than %d", *gt), decodeInput(raw)), map[string]interface{}{"gt": *gt}))
		return false
	}
	if ge != nil && value < *ge {
		o.add(withCtx(detail("greater_than_equal", o.path(name),
			fmt.Sprintf("Input should be greater than or equal to %d", *ge), decodeInput(raw)),
			map[string]interface{}{"ge": *ge}))
		return false
	}
	return true
}

func withCtx(item ErrorDetail, ctx interface{}) ErrorDetail {
	item.Ctx = ctx
	return item
}

// literal implements `name: Literal[...]`.
func (o *objectFields) literal(name string, allowed []string, required bool) (string, bool) {
	message := "Input should be " + quotedList(allowed)
	raw, present := o.raw[name]
	if !present {
		if !required {
			return "", true
		}
		o.add(detail("missing", o.path(name), "Field required", nil))
		return "", false
	}
	if isNull(raw) {
		if !required {
			return "", true
		}
		o.add(detail("literal_error", o.path(name), message, nil))
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		o.add(detail("literal_error", o.path(name), message, decodeInput(raw)))
		return "", false
	}
	for _, candidate := range allowed {
		if candidate == value {
			return value, true
		}
	}
	o.add(detail("literal_error", o.path(name), message, value))
	return "", false
}

// optionalLiteral implements `name: Optional[Literal[...]] = Field(default=None)`.
func (o *objectFields) optionalLiteral(name string, allowed []string) (*string, bool) {
	raw, present := o.raw[name]
	if !present || isNull(raw) {
		return nil, true
	}
	value, ok := o.literal(name, allowed, true)
	if !ok {
		return nil, false
	}
	return &value, true
}

func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "'"+value+"'")
	}
	return strings.Join(quoted, " or ")
}

// object implements a nested BaseModel field.
func (o *objectFields) object(name string, required bool) (*objectFields, bool) {
	raw, present := o.raw[name]
	if !present {
		if required {
			o.add(detail("missing", o.path(name), "Field required", nil))
			return nil, false
		}
		return nil, true
	}
	if isNull(raw) {
		if required {
			o.add(detail("model_type", o.path(name), "Input should be a valid dictionary or instance of "+name, nil))
			return nil, false
		}
		return nil, true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		o.add(detail("model_type", o.path(name), "Input should be a valid dictionary or instance of "+name, decodeInput(raw)))
		return nil, false
	}
	return &objectFields{loc: o.path(name), raw: fields}, true
}

// merge appends the errors collected by a nested object.
func (o *objectFields) merge(nested *objectFields) {
	if nested == nil {
		return
	}
	o.errs = append(o.errs, nested.errs...)
}

// modelError reproduces a `@model_validator(mode="after")` failure.
func (o *objectFields) modelError(message string, input interface{}) {
	o.add(detail("value_error", o.loc, "Value error, "+message, input))
}

// fieldError reproduces a `@field_validator` failure.
func (o *objectFields) fieldError(name string, message string, input interface{}) {
	o.add(detail("value_error", o.path(name), "Value error, "+message, input))
}

// isAlphanumeric mirrors Python's `str.isalnum()` (unicode aware).
func isAlphanumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
