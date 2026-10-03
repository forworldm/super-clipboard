package utils

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
)

// TestBuildTextClipHTML mirrors build_text_clip_html.
func TestBuildTextClipHTML(t *testing.T) {
	code := "12345"
	createdAt := time.Date(2024, 5, 17, 8, 9, 10, 0, time.UTC)
	html := BuildTextClipHTML("<b>Tom & Jerry</b>", createdAt, 3, &code)

	if !strings.HasPrefix(html, "<!doctype html>\n<html lang=\"zh-CN\">") {
		t.Fatalf("unexpected document head:\n%s", html[:60])
	}
	if !strings.Contains(html, "<title>Super Clipboard 直链 12345</title>") {
		t.Fatalf("title should embed the access code:\n%s", html)
	}
	if !strings.Contains(html, "<pre>&lt;b&gt;Tom &amp; Jerry&lt;/b&gt;</pre>") {
		t.Fatalf("content should escape &, < and > only:\n%s", html)
	}
	if !strings.Contains(html, "下载次数 3") {
		t.Fatalf("footer should expose the download count:\n%s", html)
	}
	if !strings.Contains(html, "创建于 "+createdAt.Local().Format("2006-01-02 15:04:05")) {
		t.Fatalf("footer should expose the local creation time:\n%s", html)
	}
	if !strings.Contains(html, "body { font-family: -apple-system") {
		t.Fatalf("css braces should not be escaped:\n%s", html)
	}

	anonymous := BuildTextClipHTML("plain", createdAt, 0, nil)
	if !strings.Contains(anonymous, "<title>Super Clipboard 直链 </title>") {
		t.Fatalf("missing code should render an empty title:\n%s", anonymous)
	}
}

// TestBuildBaseURL mirrors build_base_url.
func TestBuildBaseURL(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.com/54321?x=1", nil)
	if got := BuildBaseURL(request); got != "http://example.com" {
		t.Fatalf("unexpected base url %q", got)
	}

	request = httptest.NewRequest(http.MethodGet, "http://example.com/54321", nil)
	request.TLS = &tls.ConnectionState{}
	if got := BuildBaseURL(request); got != "https://example.com" {
		t.Fatalf("tls requests should use https, got %q", got)
	}

	request = httptest.NewRequest(http.MethodGet, "http://example.com/54321", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	if got := BuildBaseURL(request); got != "https://example.com" {
		t.Fatalf("forwarded proto should win, got %q", got)
	}

	request = httptest.NewRequest(http.MethodGet, "http://clip.example.com:8080/54321", nil)
	if got := BuildBaseURL(request); got != "http://clip.example.com:8080" {
		t.Fatalf("the host header port should be preserved, got %q", got)
	}

	request = httptest.NewRequest(http.MethodGet, "http://example.com/54321", nil)
	request.Header.Set("X-Forwarded-Proto", "https, http")
	request.Header.Set("X-Forwarded-Host", "clips.example.net, inner")
	if got := BuildBaseURL(request); got != "https://clips.example.net" {
		t.Fatalf("forwarded proto+host should win, got %q", got)
	}
}

// TestExtractClientIP mirrors _extract_client_ip.
func TestExtractClientIP(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	request.RemoteAddr = "203.0.113.9:5555"
	if got := ExtractClientIP(request); got != "203.0.113.9" {
		t.Fatalf("expected the peer address, got %q", got)
	}

	request.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.9")
	if got := ExtractClientIP(request); got != "198.51.100.7" {
		t.Fatalf("expected the first forwarded entry, got %q", got)
	}

	request.Header.Set("X-Forwarded-For", "   ")
	if got := ExtractClientIP(request); got != "203.0.113.9" {
		t.Fatalf("a blank header should fall back to the peer, got %q", got)
	}
}

// TestIsPrivateAddress mirrors the proxy guard in create_clip.
func TestIsPrivateAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.10", "172.17.0.2", "172.15.0.1"} {
		if !IsPrivateAddress(address) {
			t.Fatalf("%s should be treated as private", address)
		}
	}
	for _, address := range []string{"", "8.8.8.8", "203.0.113.9", "11.0.0.1"} {
		if IsPrivateAddress(address) {
			t.Fatalf("%s should not be treated as private", address)
		}
	}
}

// TestVerifyCaptchaTokenShortCircuits covers the branches that do not require
// an outbound request (disabled provider, missing token, bypass token).
func TestVerifyCaptchaTokenShortCircuits(t *testing.T) {
	token := "abc"

	if err := VerifyCaptchaToken(context.Background(), nil, CaptchaOptions{}); err != nil {
		t.Fatalf("captcha disabled should be a no-op, got %v", err)
	}
	if err := VerifyCaptchaToken(context.Background(), nil, CaptchaOptions{Provider: "turnstile"}); err != nil {
		t.Fatalf("a missing secret disables the check, got %v", err)
	}

	err := VerifyCaptchaToken(context.Background(), nil, CaptchaOptions{Provider: "turnstile", Secret: "s3cret"})
	assertHTTPError(t, err, http.StatusBadRequest, "缺少验证码，请重新验证后再试")

	empty := ""
	err = VerifyCaptchaToken(context.Background(), &empty, CaptchaOptions{Provider: "turnstile", Secret: "s3cret"})
	assertHTTPError(t, err, http.StatusBadRequest, "缺少验证码，请重新验证后再试")

	err = VerifyCaptchaToken(context.Background(), &token, CaptchaOptions{
		Provider:    "turnstile",
		Secret:      "s3cret",
		BypassToken: "abc",
	})
	if err != nil {
		t.Fatalf("the bypass token should be accepted, got %v", err)
	}

	err = VerifyCaptchaToken(context.Background(), &token, CaptchaOptions{Provider: "hcaptcha", Secret: "s3cret"})
	assertHTTPError(t, err, http.StatusBadRequest, "验证码服务未配置")
}

// TestVerifyCaptchaTokenAgainstStubServer exercises the siteverify round trip.
func TestVerifyCaptchaTokenAgainstStubServer(t *testing.T) {
	var receivedForm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("unable to parse form: %v", err)
		}
		receivedForm = r.PostForm.Encode()
		if strings.Contains(receivedForm, "response=good") {
			_, _ = w.Write([]byte(`{"success": true}`))
			return
		}
		_, _ = w.Write([]byte(`{"success": false, "error-codes": ["invalid-input-response", "timeout-or-duplicate"]}`))
	}))
	defer server.Close()

	original := captchaEndpoints["turnstile"]
	captchaEndpoints["turnstile"] = server.URL
	defer func() { captchaEndpoints["turnstile"] = original }()

	good := "good-token"
	if err := VerifyCaptchaToken(context.Background(), &good, CaptchaOptions{Provider: "turnstile", Secret: "s3cret", RemoteIP: "203.0.113.9"}); err != nil {
		t.Fatalf("a successful verification should pass, got %v", err)
	}
	if !strings.Contains(receivedForm, "secret=s3cret") || !strings.Contains(receivedForm, "remoteip=203.0.113.9") {
		t.Fatalf("unexpected siteverify payload %q", receivedForm)
	}

	bad := "bad-token"
	err := VerifyCaptchaToken(context.Background(), &bad, CaptchaOptions{Provider: "turnstile", Secret: "s3cret"})
	assertHTTPError(t, err, http.StatusBadRequest, "验证码校验失败（invalid-input-response, timeout-or-duplicate）")
}

// TestVerifyCaptchaTokenInvalidJSON covers the JSON decode failure branch.
func TestVerifyCaptchaTokenInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	original := captchaEndpoints["recaptcha"]
	captchaEndpoints["recaptcha"] = server.URL
	defer func() { captchaEndpoints["recaptcha"] = original }()

	token := "whatever"
	err := VerifyCaptchaToken(context.Background(), &token, CaptchaOptions{Provider: "recaptcha", Secret: "s3cret"})
	assertHTTPError(t, err, http.StatusBadRequest, "验证码校验失败")
}

func assertHTTPError(t *testing.T, err error, status int, detail string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected HTTP %d %q, got nil", status, detail)
	}
	var httpError *apperr.HTTPError
	if !errors.As(err, &httpError) {
		t.Fatalf("expected an HTTPError, got %T (%v)", err, err)
	}
	if httpError.Status != status || httpError.Detail != detail {
		t.Fatalf("expected HTTP %d %q, got %d %q", status, detail, httpError.Status, httpError.Detail)
	}
}
