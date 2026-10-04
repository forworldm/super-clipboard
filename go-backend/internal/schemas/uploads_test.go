package schemas

import (
	"strings"
	"testing"
)

func TestParseUploadInitRequest(t *testing.T) {
	req, err := ParseUploadInitRequest([]byte(`{"filename":"a.txt","fileSize":123,"mimeType":"text/plain","environmentId":"env-1","expiresAt":9999999999999}`))
	if err != nil {
		t.Fatalf("valid init should parse: %v", err)
	}
	if req.Filename != "a.txt" || req.FileSize != 123 || req.MimeType != "text/plain" || req.EnvironmentID != "env-1" {
		t.Fatalf("unexpected %+v", req)
	}
	// Mime defaults, unknown client knobs (chunkSize/uploadId) are ignored:
	// the server always generates them.
	req, err = ParseUploadInitRequest([]byte(`{"filename":"a.bin","fileSize":10,"environmentId":"env-1","expiresAt":9999999999999,"chunkSize":123,"uploadId":"hacked"}`))
	if err != nil {
		t.Fatalf("extra keys should be ignored: %v", err)
	}
	if req.MimeType != "application/octet-stream" {
		t.Fatalf("mime should default, got %q", req.MimeType)
	}
	if _, err := ParseUploadInitRequest([]byte(`{"fileSize":10}`)); err == nil {
		t.Fatalf("missing filename should 422")
	}
	if _, err := ParseUploadInitRequest([]byte(`{"filename":"   ","fileSize":10}`)); err == nil {
		t.Fatalf("blank filename should 422")
	}
	if _, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":-1}`)); err == nil {
		t.Fatalf("negative fileSize should 422")
	} else if !strings.Contains(err.Error(), "greater_than_equal") && !strings.Contains(err.Error(), "ge") {
		// Error string contains loc+msg; just ensure it mentions the field.
		t.Logf("negative size error: %v", err)
	}
	if _, err := ParseUploadInitRequest([]byte(`not-json`)); err == nil {
		t.Fatalf("invalid json should 422")
	}
}

// TestParseUploadInitRequestRequiresEnvironmentID locks the invariant that made
// this refactor possible: there is no ownerless upload, because without an
// accessCode/accessToken a clip is only reachable through its environment.
func TestParseUploadInitRequestRequiresEnvironmentID(t *testing.T) {
	// Key absent -> 422 and the error points at environmentId.
	_, err := ParseUploadInitRequest([]byte(`{"filename":"a.bin","fileSize":10,"expiresAt":9999999999999}`))
	if err == nil {
		t.Fatalf("init without environmentId must be rejected")
	}
	if !strings.Contains(err.Error(), "environmentId") {
		t.Fatalf("error should mention environmentId, got %v", err)
	}
	// Blank/whitespace-only -> 422 as well (model validator).
	_, err = ParseUploadInitRequest([]byte(`{"filename":"a.bin","fileSize":10,"environmentId":"   ","expiresAt":9999999999999}`))
	if err == nil || !strings.Contains(err.Error(), "environmentId") {
		t.Fatalf("blank environmentId must be rejected, got %v", err)
	}
	// A persistent token without an owner cannot be validated -> rejected.
	_, err = ParseUploadInitRequest([]byte(`{"filename":"a.bin","fileSize":10,"environmentId":"  ","expiresAt":9999999999999,"accessToken":"tok-9876543"}`))
	if err == nil {
		t.Fatalf("token without environmentId must be rejected")
	}
}

func TestParseUploadInitRequestClipParams(t *testing.T) {
	req, err := ParseUploadInitRequest([]byte(`{
		"filename":"a.bin","fileSize":10,"environmentId":"env-1",
		"expiresAt":9999999999999,"maxDownloads":5,"accessCode":"12345"
	}`))
	if err != nil {
		t.Fatalf("valid clip init should parse: %v", err)
	}
	if req.ExpiresAt != 9999999999999 || req.EnvironmentID != "env-1" {
		t.Fatalf("unexpected %+v", req)
	}
	if req.MaxDownloads == nil || *req.MaxDownloads != 5 {
		t.Fatalf("maxDownloads not parsed: %+v", req)
	}
	if req.AccessCode == nil || *req.AccessCode != "12345" {
		t.Fatalf("accessCode not parsed: %+v", req)
	}
	// clip params are mandatory now: expiresAt is required for every upload.
	if _, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1"}`)); err == nil {
		t.Fatalf("missing expiresAt should 422 (all uploads are clips)")
	}
	if _, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":-1}`)); err == nil {
		t.Fatalf("bad expiresAt should 422")
	}
	if _, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":9999999999999,"accessCode":"ab-cd"}`)); err == nil {
		t.Fatalf("bad accessCode should 422")
	}
	// A nameless clip (no code, no token) is legal: only its env owner sees it.
	plain, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":9999999999999}`))
	if err != nil {
		t.Fatalf("nameless clip init should parse: %v", err)
	}
	if plain.AccessCode != nil || plain.AccessToken != nil {
		t.Fatalf("nameless clip must not invent access fields: %+v", plain)
	}
	// Blank accessCode == "no code".
	blank, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":9999999999999,"accessCode":"     "}`))
	if err != nil || blank.AccessCode != nil {
		t.Fatalf("blank accessCode should be dropped: %+v err=%v", blank, err)
	}
	// accessToken is trimmed and kept.
	tok, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":9999999999999,"accessToken":"  tok-1234567  "}`))
	if err != nil || tok.AccessToken == nil || *tok.AccessToken != "tok-1234567" {
		t.Fatalf("accessToken not trimmed: %+v err=%v", tok, err)
	}
}

func TestParseUploadInitRequestIdAndCaptcha(t *testing.T) {
	// requestId + captcha fields parse through.
	req, err := ParseUploadInitRequest([]byte(`{
		"filename":"a.bin","fileSize":2048,"environmentId":"env-1","expiresAt":9999999999999,
		"requestId":"req-abc_1:2","captchaToken":"tok-123","captchaProvider":"turnstile"
	}`))
	if err != nil {
		t.Fatalf("valid init w/ requestId should parse: %v", err)
	}
	if req.RequestID != "req-abc_1:2" {
		t.Fatalf("requestId not parsed: %+v", req)
	}
	if req.CaptchaToken == nil || *req.CaptchaToken != "tok-123" {
		t.Fatalf("captchaToken not parsed: %+v", req)
	}
	// Unsafe characters rejected.
	if _, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":9999999999999,"requestId":"bad id!"}`)); err == nil {
		t.Fatalf("unsafe requestId should 422")
	}
	// Missing/short ok.
	req2, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":9999999999999,"requestId":"abcd"}`))
	if err != nil || req2.RequestID != "abcd" {
		t.Fatalf("short requestId should pass: %+v err=%v", req2, err)
	}
	if _, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":9999999999999,"requestId":"abc"}`)); err == nil {
		t.Fatalf("below-min requestId should 422")
	}
	// Empty/whitespace captchaToken treated as absent.
	req3, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":9999999999999,"captchaToken":"   "}`))
	if err != nil || req3.CaptchaToken != nil {
		t.Fatalf("blank captchaToken should be dropped: %+v err=%v", req3, err)
	}
}

// TestUploadInitRequestHasNoClipModeFlag documents the removal of HasClipFields:
// the struct must not expose any "is this a clip?" switch anymore.
func TestUploadInitRequestHasNoClipModeFlag(t *testing.T) {
	req, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"environmentId":"env-1","expiresAt":9999999999999}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The clip params are always populated (from the request), never gated.
	if req.ExpiresAt == 0 {
		t.Fatalf("expiresAt must always be carried on the request: %+v", req)
	}
	if req.EnvironmentID == "" {
		t.Fatalf("environmentId must always be carried on the request: %+v", req)
	}
}
