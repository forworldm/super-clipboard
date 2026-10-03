package schemas

import (
	"strings"
	"testing"
)

func TestParseUploadInitRequest(t *testing.T) {
	req, err := ParseUploadInitRequest([]byte(`{"filename":"a.txt","fileSize":123,"mimeType":"text/plain","environmentId":"env-1"}`))
	if err != nil {
		t.Fatalf("valid init should parse: %v", err)
	}
	if req.Filename != "a.txt" || req.FileSize != 123 || req.MimeType != "text/plain" || req.EnvironmentID != "env-1" {
		t.Fatalf("unexpected %+v", req)
	}
	// Mime defaults, unknown client knobs (chunkSize/uploadId) are ignored:
	// the server always generates them.
	req, err = ParseUploadInitRequest([]byte(`{"filename":"a.bin","fileSize":10,"chunkSize":123,"uploadId":"hacked"}`))
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

func TestParseUploadInitRequestIdAndCaptcha(t *testing.T) {
	// requestId + captcha fields parse through.
	req, err := ParseUploadInitRequest([]byte(`{
		"filename":"a.bin","fileSize":2048,
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
	if _, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"requestId":"bad id!"}`)); err == nil {
		t.Fatalf("unsafe requestId should 422")
	}
	// Missing/short ok.
	req2, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"requestId":"abcd"}`))
	if err != nil || req2.RequestID != "abcd" {
		t.Fatalf("short requestId should pass: %+v err=%v", req2, err)
	}
	if _, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"requestId":"abc"}`)); err == nil {
		t.Fatalf("below-min requestId should 422")
	}
	// Empty/whitespace captchaToken treated as absent.
	req3, err := ParseUploadInitRequest([]byte(`{"filename":"a","fileSize":1,"captchaToken":"   "}`))
	if err != nil || req3.CaptchaToken != nil {
		t.Fatalf("blank captchaToken should be dropped: %+v err=%v", req3, err)
	}
}

func TestParseUploadCompleteIgnoresCaptcha(t *testing.T) {
	// Bodies carrying only deprecated captcha keys behave as file-only.
	params, err := ParseUploadCompleteRequest([]byte(`{"captchaToken":"tok","captchaProvider":"turnstile"}`))
	if err != nil {
		t.Fatalf("captcha-only complete should be file-only: %v", err)
	}
	if params.HasClipFields {
		t.Fatalf("captcha-only complete must not be treated as clip params")
	}
}

func TestParseUploadCompleteRequest(t *testing.T) {
	// Empty bodies mean file-only assembly.
	for _, body := range []string{"", "   ", "{}", "null"} {
		params, err := ParseUploadCompleteRequest([]byte(body))
		if err != nil {
			t.Fatalf("body %q should be file-only: %v", body, err)
		}
		if params.HasClipFields {
			t.Fatalf("body %q should not have clip fields", body)
		}
	}
	params, err := ParseUploadCompleteRequest([]byte(`{"environmentId":"env-1","expiresAt":9999999999999,"maxDownloads":5,"accessCode":"12345"}`))
	if err != nil {
		t.Fatalf("valid clip params: %v", err)
	}
	if !params.HasClipFields || params.EnvironmentID != "env-1" || params.ExpiresAt != 9999999999999 {
		t.Fatalf("unexpected %+v", params)
	}
	if params.MaxDownloads == nil || *params.MaxDownloads != 5 {
		t.Fatalf("maxDownloads not parsed: %+v", params)
	}
	if _, err := ParseUploadCompleteRequest([]byte(`{"environmentId":"env-1","expiresAt":-1}`)); err == nil {
		t.Fatalf("bad expiresAt should 422")
	}
	if _, err := ParseUploadCompleteRequest([]byte(`{"environmentId":"env-1","expiresAt":9999999999999,"accessCode":"ab-cd"}`)); err == nil {
		t.Fatalf("bad accessCode should 422")
	}
	if _, err := ParseUploadCompleteRequest([]byte(`{"expiresAt":9999999999999}`)); err == nil {
		t.Fatalf("missing environmentId should 422")
	}
}
