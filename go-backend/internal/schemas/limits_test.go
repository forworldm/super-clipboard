package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

// clipCreateBody renders a structurally valid text clip body, overriding only
// the fields a test cares about. json.Marshal is used instead of raw strings so
// multibyte and over-long values are always encoded correctly.
func clipCreateBody(t *testing.T, text string, accessToken string, environmentID string) []byte {
	t.Helper()
	body := map[string]interface{}{
		"type":          "text",
		"expiresAt":     1893456000000,
		"environmentId": environmentID,
		"payload":       map[string]interface{}{"text": text},
	}
	if accessToken != "" {
		body["accessToken"] = accessToken
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return encoded
}

func firstError(t *testing.T, validationError *ValidationError) ErrorDetail {
	t.Helper()
	if validationError == nil || len(validationError.Details) == 0 {
		t.Fatalf("expected a validation error")
	}
	return validationError.Details[0]
}

// TestClipTextAtLimitIsAccepted pins the boundary: exactly 64 KiB of ASCII text
// still fits in an inline text clip.
func TestClipTextAtLimitIsAccepted(t *testing.T) {
	text := strings.Repeat("a", MaxClipTextBytes)
	request, validationError := ParseClipCreateRequest(clipCreateBody(t, text, "", "env-1"))
	if validationError != nil {
		t.Fatalf("64 KiB of text must be accepted, got %s", validationError.Error())
	}
	if request.Payload.Text == nil || len(*request.Payload.Text) != MaxClipTextBytes {
		t.Fatalf("text payload not preserved")
	}
}

// TestClipTextOverLimitIsRejected covers the new upper bound on payload.text:
// it used to be unbounded, so only readBody (1 MiB) stood in the way.
func TestClipTextOverLimitIsRejected(t *testing.T) {
	text := strings.Repeat("a", MaxClipTextBytes+1)
	_, validationError := ParseClipCreateRequest(clipCreateBody(t, text, "", "env-1"))
	if validationError == nil {
		t.Fatalf("text above %d bytes must be rejected", MaxClipTextBytes)
	}
	detail := firstError(t, validationError)
	if detail.Type != "string_too_long" {
		t.Fatalf("unexpected error type %q", detail.Type)
	}
	if len(detail.Loc) != 3 || detail.Loc[2] != "text" {
		t.Fatalf("unexpected error location %v", detail.Loc)
	}
	// The message must point at the supported alternative: a file clip.
	if !strings.Contains(detail.Msg, "文件片段") || !strings.Contains(detail.Msg, "65536") {
		t.Fatalf("message should explain the bound and the remedy, got %q", detail.Msg)
	}
}

// TestClipTextByteBoundCatchesMultibyte ensures the byte ceiling, not just the
// character count, is enforced: 4-byte runes would otherwise buy 4x the budget.
func TestClipTextByteBoundCatchesMultibyte(t *testing.T) {
	text := strings.Repeat("汉", MaxClipTextBytes/3+1) // 3 bytes per rune
	if len([]rune(text)) > MaxClipTextRunes {
		t.Fatalf("test setup should stay under the character bound")
	}
	if len(text) <= MaxClipTextBytes {
		t.Fatalf("test setup should exceed the byte bound")
	}
	_, validationError := ParseClipCreateRequest(clipCreateBody(t, text, "", "env-1"))
	if validationError == nil {
		t.Fatalf("multibyte text above %d bytes must be rejected", MaxClipTextBytes)
	}
	if detail := firstError(t, validationError); detail.Type != "string_too_long" {
		t.Fatalf("unexpected error type %q", detail.Type)
	}
}

// TestAccessTokenMaxLength covers the 128 character/byte bound on the
// persistent token (clip create, token register and upload init share it).
func TestAccessTokenMaxLength(t *testing.T) {
	ok := strings.Repeat("t", MaxAccessTokenRunes)
	if _, validationError := ParseClipCreateRequest(clipCreateBody(t, "hi", ok, "env-1")); validationError != nil {
		t.Fatalf("a %d character token must be accepted, got %s", MaxAccessTokenRunes, validationError.Error())
	}

	tooLong := strings.Repeat("t", MaxAccessTokenRunes+1)
	_, validationError := ParseClipCreateRequest(clipCreateBody(t, "hi", tooLong, "env-1"))
	if validationError == nil {
		t.Fatalf("a token longer than %d characters must be rejected", MaxAccessTokenRunes)
	}
	detail := firstError(t, validationError)
	if detail.Type != "string_too_long" || detail.Loc[1] != "accessToken" {
		t.Fatalf("unexpected error %q at %v", detail.Type, detail.Loc)
	}

	// A multibyte token with fewer characters but more bytes is rejected too.
	multibyte := strings.Repeat("汉", MaxAccessTokenRunes)
	if _, err := ParseClipCreateRequest(clipCreateBody(t, "hi", multibyte, "env-1")); err == nil {
		t.Fatalf("a token above %d bytes must be rejected", MaxAccessTokenBytes)
	}
}

// TestAccessTokenStillHasMinimum keeps the historical min_length=7 intact.
func TestAccessTokenStillHasMinimum(t *testing.T) {
	_, validationError := ParseClipCreateRequest(clipCreateBody(t, "hi", "short", "env-1"))
	if validationError == nil {
		t.Fatalf("a 5 character token must still be rejected")
	}
	if detail := firstError(t, validationError); detail.Type != "string_too_short" {
		t.Fatalf("unexpected error type %q", detail.Type)
	}
}

// TestEnvironmentIDBound covers environmentId (1..64 characters and bytes).
func TestEnvironmentIDBound(t *testing.T) {
	if _, validationError := ParseClipCreateRequest(clipCreateBody(t, "hi", "", strings.Repeat("e", MaxEnvironmentIDBytes))); validationError != nil {
		t.Fatalf("a %d byte environmentId must be accepted, got %s", MaxEnvironmentIDBytes, validationError.Error())
	}
	if _, err := ParseClipCreateRequest(clipCreateBody(t, "hi", "", strings.Repeat("e", MaxEnvironmentIDBytes+1))); err == nil {
		t.Fatalf("an environmentId above %d bytes must be rejected", MaxEnvironmentIDBytes)
	}
}

// TestTokenRegisterTokenBound covers POST /api/tokens/register, which accepted
// an unbounded token as well.
func TestTokenRegisterTokenBound(t *testing.T) {
	okBody, _ := json.Marshal(map[string]interface{}{
		"token":         strings.Repeat("t", MaxAccessTokenRunes),
		"environmentId": "env-1",
	})
	if _, validationError := ParseTokenRegisterRequest(okBody); validationError != nil {
		t.Fatalf("a %d character token must be accepted, got %s", MaxAccessTokenRunes, validationError.Error())
	}

	badBody, _ := json.Marshal(map[string]interface{}{
		"token":         strings.Repeat("t", MaxAccessTokenRunes+1),
		"environmentId": "env-1",
	})
	if _, err := ParseTokenRegisterRequest(badBody); err == nil {
		t.Fatalf("a token longer than %d characters must be rejected", MaxAccessTokenRunes)
	}
}

// TestUploadInitSharesClipBounds makes sure the chunked upload path cannot be
// used to bypass the inline bounds.
func TestUploadInitSharesClipBounds(t *testing.T) {
	tooLongToken, _ := json.Marshal(map[string]interface{}{
		"filename":      "a.bin",
		"fileSize":      10,
		"environmentId": "env-1",
		"expiresAt":     1893456000000,
		"accessToken":   strings.Repeat("t", MaxAccessTokenRunes+1),
	})
	if _, err := ParseUploadInitRequest(tooLongToken); err == nil {
		t.Fatalf("upload init must reject a token longer than %d characters", MaxAccessTokenRunes)
	}

	tooLongName, _ := json.Marshal(map[string]interface{}{
		"filename":      strings.Repeat("n", MaxStoredFileNameBytes+1),
		"fileSize":      10,
		"environmentId": "env-1",
		"expiresAt":     1893456000000,
	})
	if _, err := ParseUploadInitRequest(tooLongName); err == nil {
		t.Fatalf("upload init must reject a filename longer than %d bytes", MaxStoredFileNameBytes)
	}

	valid, _ := json.Marshal(map[string]interface{}{
		"filename":      "a.bin",
		"fileSize":      10,
		"environmentId": "env-1",
		"expiresAt":     1893456000000,
		"accessToken":   strings.Repeat("t", MaxAccessTokenRunes),
	})
	if _, validationError := ParseUploadInitRequest(valid); validationError != nil {
		t.Fatalf("a valid init body must still parse, got %s", validationError.Error())
	}
}

// TestFileClipDataURLBound covers the inline base64 data URI ceiling.
func TestFileClipDataURLBound(t *testing.T) {
	buildBody := func(dataURL string) []byte {
		body, _ := json.Marshal(map[string]interface{}{
			"type":          "file",
			"expiresAt":     1893456000000,
			"environmentId": "env-1",
			"payload": map[string]interface{}{
				"file": map[string]interface{}{
					"name":    "a.txt",
					"size":    1,
					"type":    "text/plain",
					"dataUrl": dataURL,
				},
			},
		})
		return body
	}

	small := append([]byte("data:text/plain;base64,"), make([]byte, 32)...)
	if _, validationError := ParseClipCreateRequest(buildBody(string(small))); validationError != nil {
		t.Fatalf("a small data URI must be accepted, got %s", validationError.Error())
	}

	huge := strings.Repeat("A", MaxDataURLBytes+1)
	_, validationError := ParseClipCreateRequest(buildBody("data:text/plain;base64," + huge))
	if validationError == nil {
		t.Fatalf("a data URI above %d bytes must be rejected", MaxDataURLBytes)
	}
	if detail := firstError(t, validationError); detail.Type != "string_too_long" {
		t.Fatalf("unexpected error type %q", detail.Type)
	}
}

// TestBoundsAreDocumented guards the exported bound values used by the docs and
// by the Next.js console that mirrors these limits.
func TestBoundsAreDocumented(t *testing.T) {
	if ClipTextBound.MaxBytes != 64*1024 || ClipTextBound.MaxRunes != 64*1024 {
		t.Fatalf("clip text bound should be 64 KiB, got %+v", ClipTextBound)
	}
	if AccessTokenBound.MaxRunes != 128 || AccessTokenBound.MaxBytes != 128 {
		t.Fatalf("token bound should be 128, got %+v", AccessTokenBound)
	}
	if RequestIDBound.MaxRunes != 128 {
		t.Fatalf("requestId bound should be 128, got %+v", RequestIDBound)
	}
}
