package api

import (
	"context"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/config"
	"github.com/pixia1234/super-clipboard/backend/internal/models"
	"github.com/pixia1234/super-clipboard/backend/internal/repository"
)

// TestCorsPreflightRejectsUnknownMethod matches Starlette's 400 failure path.
func TestCorsPreflightRejectsUnknownMethod(t *testing.T) {
	app := newTestApp(t, nil)

	preflight := httptest.NewRequest(http.MethodOptions, "/api/clips", nil)
	preflight.Header.Set("Origin", "https://clip.example.com")
	preflight.Header.Set("Access-Control-Request-Method", "TRACE")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, preflight)
	requireStatus(t, recorder, http.StatusBadRequest)
	if recorder.Body.String() != "Disallowed CORS method" {
		t.Fatalf("unexpected preflight body %q", recorder.Body.String())
	}
}

// TestCorsPreflightWithoutRequestedHeaders uses the wildcard Allow-Headers fallback.
func TestCorsPreflightWithoutRequestedHeaders(t *testing.T) {
	app := newTestApp(t, nil)

	preflight := httptest.NewRequest(http.MethodOptions, "/api/clips", nil)
	preflight.Header.Set("Origin", "https://clip.example.com")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, preflight)
	requireStatus(t, recorder, http.StatusOK)
	if recorder.Header().Get("Access-Control-Allow-Headers") != "*" {
		t.Fatalf("expected wildcard allow headers, got %q", recorder.Header().Get("Access-Control-Allow-Headers"))
	}
}

// TestHeadHealthz checks Starlette's implicit HEAD-on-GET behaviour.
func TestHeadHealthz(t *testing.T) {
	app := newTestApp(t, nil)
	recorder := do(t, app, http.MethodHead, "/healthz", nil)
	requireStatus(t, recorder, http.StatusOK)
}

// TestTextWhitespaceIsPreserved documents that create_clip stores the text as
// submitted; only emptiness is validated (Python does not TrimSpace the payload).
func TestTextWhitespaceIsPreserved(t *testing.T) {
	app := newTestApp(t, nil)
	body := map[string]interface{}{
		"type":          "text",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  2,
		"environmentId": "owner-ws",
		"payload":       map[string]interface{}{"text": "  padded  \n"},
	}
	recorder := do(t, app, http.MethodPost, "/api/clips", body)
	requireStatus(t, recorder, http.StatusCreated)
	payload := decode(t, recorder)["payload"].(map[string]interface{})
	if payload["text"] != "  padded  \n" {
		t.Fatalf("whitespace should be preserved, got %q", payload["text"])
	}
}

// TestJSONDoesNotHTMLEscape keeps FastAPI's json.dumps wire format.
func TestJSONDoesNotHTMLEscape(t *testing.T) {
	app := newTestApp(t, nil)
	body := map[string]interface{}{
		"type":          "text",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  1,
		"environmentId": "owner-html",
		"payload":       map[string]interface{}{"text": "<b>Tom & Jerry</b>"},
	}
	recorder := do(t, app, http.MethodPost, "/api/clips", body)
	requireStatus(t, recorder, http.StatusCreated)
	raw := recorder.Body.String()
	if !strings.Contains(raw, `"<b>Tom & Jerry</b>"`) {
		t.Fatalf("JSON should not HTML-escape text, got %s", raw)
	}
	if strings.Contains(raw, `\u003c`) {
		t.Fatalf("JSON should not use encoding/json HTML escapes, got %s", raw)
	}
}

// TestDirectLinkHTMLEscapesOnlyAngleBrackets matches build_text_clip_html.
func TestDirectLinkHTMLEscapesOnlyAngleBrackets(t *testing.T) {
	app := newTestApp(t, nil)
	code := "11111"
	body := textClipBody("owner-html-page", &code, 3)
	body["payload"] = map[string]interface{}{"text": `say "hi" & <bye>`}
	recorder := do(t, app, http.MethodPost, "/api/clips", body)
	requireStatus(t, recorder, http.StatusCreated)

	page := do(t, app, http.MethodGet, "/"+code, nil)
	requireStatus(t, page, http.StatusOK)
	html := page.Body.String()
	if !strings.Contains(html, "say \"hi\"") {
		t.Fatalf("quotes should be preserved, got %s", html)
	}
	if !strings.Contains(html, "amp;") || !strings.Contains(html, "lt;bye") {
		t.Fatalf("expected &, <, > to be escaped, got %s", html)
	}
	if strings.Contains(html, "<bye>") {
		t.Fatalf("raw <bye> leaked into HTML: %s", html)
	}
}

// TestInactiveClipGetDeletesAnd404 covers GET /api/clips/{id} after the download
// limit is reached without going through the HTTP increment handler (which would
// have already deleted the row).
func TestInactiveClipGetDeletesAnd404(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-inactive"
	recorder := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, nil, 1))
	requireStatus(t, recorder, http.StatusCreated)
	clipID := decode(t, recorder)["id"].(string)

	if _, reached, err := app.Repo.IncrementDownloads(clipID, environmentID); err != nil || !reached {
		t.Fatalf("expected the clip to hit its limit: reached=%v err=%v", reached, err)
	}

	fetched := do(t, app, http.MethodGet, "/api/clips/"+clipID+"?environmentId="+environmentID, nil)
	requireStatus(t, fetched, http.StatusNotFound)
	if decode(t, fetched)["detail"] != "片段已过期或达到下载次数" {
		t.Fatalf("unexpected detail %v", decode(t, fetched))
	}

	again := do(t, app, http.MethodGet, "/api/clips/"+clipID+"?environmentId="+environmentID, nil)
	requireStatus(t, again, http.StatusNotFound)
	if decode(t, again)["detail"] != "片段未找到" {
		t.Fatalf("clip should have been deleted, got %v", decode(t, again))
	}
}

// TestMissingStoredFileReturns410 covers the on-disk file disappearing.
func TestMissingStoredFileReturns410(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-lost"
	fileContent := []byte("temp")
	recorder := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "file",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  5,
		"environmentId": environmentID,
		"payload": map[string]interface{}{
			"file": map[string]interface{}{
				"name":    "temp.txt",
				"size":    len(fileContent),
				"type":    "text/plain",
				"dataUrl": "data:text/plain;base64," + base64.StdEncoding.EncodeToString(fileContent),
			},
		},
	})
	requireStatus(t, recorder, http.StatusCreated)
	clipID := decode(t, recorder)["id"].(string)

	entries, err := os.ReadDir(app.Settings.FileStorageDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected a stored file, err=%v entries=%d", err, len(entries))
	}
	if err := os.Remove(filepath.Join(app.Settings.FileStorageDir, entries[0].Name())); err != nil {
		t.Fatalf("unable to remove stored file: %v", err)
	}

	download := do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+environmentID, nil)
	requireStatus(t, download, http.StatusGone)
	if decode(t, download)["detail"] != "文件已丢失" {
		t.Fatalf("unexpected detail %v", decode(t, download))
	}
}

// TestDirectFileLinkServesThenExpires covers GET /{code} for a file clip,
// including that the last permitted download still returns the bytes (delete
// happens after the response is written).
func TestDirectFileLinkServesThenExpires(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-direct-file"
	code := "22222"
	fileContent := []byte("direct-file")
	recorder := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "file",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  1,
		"accessCode":    code,
		"environmentId": environmentID,
		"payload": map[string]interface{}{
			"file": map[string]interface{}{
				"name":    "direct.bin",
				"size":    len(fileContent),
				"type":    "application/octet-stream",
				"dataUrl": "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(fileContent),
			},
		},
	})
	requireStatus(t, recorder, http.StatusCreated)

	first := do(t, app, http.MethodGet, "/"+code, nil)
	requireStatus(t, first, http.StatusOK)
	if first.Body.String() != string(fileContent) {
		t.Fatalf("unexpected body %q", first.Body.String())
	}
	if disp := first.Header().Get("Content-Disposition"); !strings.Contains(disp, "direct.bin") {
		t.Fatalf("expected an attachment filename, got %q", disp)
	}

	second := do(t, app, http.MethodGet, "/"+code, nil)
	requireStatus(t, second, http.StatusNotFound)
}

// TestFileClipWithoutPayloadReturns410 covers a file row that has no stored file.
func TestFileClipWithoutPayloadReturns410(t *testing.T) {
	app := newTestApp(t, nil)
	code := "33333"
	_, err := app.Repo.CreateClip(repository.CreateClipParams{
		ClipType:      models.ClipTypeFile,
		ExpiresAtMs:   futureTimestamp(1),
		AccessCode:    &code,
		EnvironmentID: "owner-empty-file",
	})
	if err != nil {
		t.Fatalf("unable to seed file clip: %v", err)
	}

	recorder := do(t, app, http.MethodGet, "/"+code, nil)
	requireStatus(t, recorder, http.StatusGone)
	if decode(t, recorder)["detail"] != "文件数据缺失" {
		t.Fatalf("unexpected detail %v", decode(t, recorder))
	}
}

// TestCaptchaMisconfiguredReturns500 matches main.py when the provider is set
// without a secret.
func TestCaptchaMisconfiguredReturns500(t *testing.T) {
	app := newTestApp(t, func(settings *config.Settings) {
		settings.CaptchaProvider = "turnstile"
		settings.CaptchaSecret = ""
	})
	recorder := do(t, app, http.MethodPost, "/api/clips", textClipBody("owner-captcha-500", nil, 1))
	requireStatus(t, recorder, http.StatusInternalServerError)
	if decode(t, recorder)["detail"] != "验证码服务未正确配置" {
		t.Fatalf("unexpected detail %v", decode(t, recorder))
	}
}

// TestTokenTooShortReturns422 covers pydantic min_length=7 on accessToken / token.
func TestTokenTooShortReturns422(t *testing.T) {
	app := newTestApp(t, nil)

	create := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "text",
		"expiresAt":     futureTimestamp(1),
		"accessToken":   "short",
		"environmentId": "owner-short",
		"payload":       map[string]interface{}{"text": "x"},
	})
	requireStatus(t, create, http.StatusUnprocessableEntity)

	register := do(t, app, http.MethodPost, "/api/tokens/register", map[string]interface{}{"token": "abc"})
	requireStatus(t, register, http.StatusUnprocessableEntity)
}

// TestStaticFileDotDotInNameIsServed ensures ".." inside a filename is not
// mistaken for path traversal.
func TestStaticFileDotDotInNameIsServed(t *testing.T) {
	app := newTestApp(t, nil)
	if err := os.MkdirAll(app.Settings.StaticRoot, 0o755); err != nil {
		t.Fatalf("unable to create static root: %v", err)
	}
	name := "vendor..chunk.js"
	if err := os.WriteFile(filepath.Join(app.Settings.StaticRoot, name), []byte("ok"), 0o644); err != nil {
		t.Fatalf("unable to write static file: %v", err)
	}
	app = NewApp(app.Settings, app.Repo)
	app.logger = log.New(io.Discard, "", 0)

	recorder := do(t, app, http.MethodGet, "/static/"+name, nil)
	requireStatus(t, recorder, http.StatusOK)
	if recorder.Body.String() != "ok" {
		t.Fatalf("unexpected body %q", recorder.Body.String())
	}

	traversal := do(t, app, http.MethodGet, "/static/../../etc/passwd", nil)
	requireStatus(t, traversal, http.StatusNotFound)
}

// TestStaticDirectoryIsNotListed matches StaticFiles(html=False).
func TestStaticDirectoryIsNotListed(t *testing.T) {
	app := newTestApp(t, nil)
	if err := os.MkdirAll(app.Settings.StaticRoot, 0o755); err != nil {
		t.Fatalf("unable to create static root: %v", err)
	}
	app = NewApp(app.Settings, app.Repo)
	app.logger = log.New(io.Discard, "", 0)

	recorder := do(t, app, http.MethodGet, "/static", nil)
	requireStatus(t, recorder, http.StatusNotFound)
}

// TestRecoverMiddlewareTurnsPanicInto500 covers the recover wrapper.
func TestRecoverMiddlewareTurnsPanicInto500(t *testing.T) {
	app := newTestApp(t, nil)
	handler := app.recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	request := httptest.NewRequest(http.MethodGet, "/panic", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	requireStatus(t, recorder, http.StatusInternalServerError)
}

// TestGetClipByCodeInactiveReturns404 covers GET /api/clips/code/{code} after expiry.
func TestGetClipByCodeInactiveReturns404(t *testing.T) {
	app := newTestApp(t, nil)
	code := "44444"
	environmentID := "owner-code-inactive"
	recorder := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, &code, 1))
	requireStatus(t, recorder, http.StatusCreated)
	clipID := decode(t, recorder)["id"].(string)

	if _, _, err := app.Repo.IncrementDownloads(clipID, environmentID); err != nil {
		t.Fatalf("unable to exhaust clip: %v", err)
	}

	byCode := do(t, app, http.MethodGet, "/api/clips/code/"+code, nil)
	requireStatus(t, byCode, http.StatusNotFound)
	if decode(t, byCode)["detail"] != "直链不存在或已过期" {
		t.Fatalf("unexpected detail %v", decode(t, byCode))
	}
}

// TestRawDirectLinkForText covers GET /{code}/raw after a token-less short code.
func TestRawDirectLinkForText(t *testing.T) {
	app := newTestApp(t, nil)
	code := "55555"
	recorder := do(t, app, http.MethodPost, "/api/clips", textClipBody("owner-raw", &code, 3))
	requireStatus(t, recorder, http.StatusCreated)

	raw := do(t, app, http.MethodGet, "/"+code+"/raw", nil)
	requireStatus(t, raw, http.StatusOK)
	if raw.Body.String() != "hello fastapi" {
		t.Fatalf("unexpected raw body %q", raw.Body.String())
	}
}

// TestUnicodeFilenameUsesRFC5987 covers non-ASCII Content-Disposition.
func TestUnicodeFilenameUsesRFC5987(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-unicode"
	fileContent := []byte("x")
	recorder := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "file",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  2,
		"environmentId": environmentID,
		"payload": map[string]interface{}{
			"file": map[string]interface{}{
				"name":    "笔记.txt",
				"size":    1,
				"type":    "text/plain",
				"dataUrl": "data:text/plain;base64," + base64.StdEncoding.EncodeToString(fileContent),
			},
		},
	})
	requireStatus(t, recorder, http.StatusCreated)
	clipID := decode(t, recorder)["id"].(string)

	download := do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+environmentID, nil)
	requireStatus(t, download, http.StatusOK)
	disposition := download.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "filename*=utf-8''") {
		t.Fatalf("expected RFC 5987 filename, got %q", disposition)
	}
}

// TestRequestBodyTooLargeReturns413 covers the MaxBytesReader guard.
func TestRequestBodyTooLargeReturns413(t *testing.T) {
	app := newTestApp(t, nil)
	original := maxBodyBytes
	maxBodyBytes = 64
	defer func() { maxBodyBytes = original }()

	request := httptest.NewRequest(http.MethodPost, "/api/clips", strings.NewReader(strings.Repeat("a", 128)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	requireStatus(t, recorder, http.StatusRequestEntityTooLarge)
	if decode(t, recorder)["detail"] != "请求体过大" {
		t.Fatalf("unexpected detail %v", decode(t, recorder))
	}
}

// TestOwnerDotCodeIdentifier covers the owner.12345 direct-link form.
func TestOwnerDotCodeIdentifier(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-dot"
	code := "66666"
	recorder := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, &code, 5))
	requireStatus(t, recorder, http.StatusCreated)

	page := do(t, app, http.MethodGet, "/"+environmentID+"."+code, nil)
	requireStatus(t, page, http.StatusOK)
	if !strings.Contains(page.Body.String(), "hello fastapi") {
		t.Fatalf("expected clip content, got %s", page.Body.String())
	}
}

// TestAppServeHTTP covers *App as an http.Handler.
func TestAppServeHTTP(t *testing.T) {
	app := newTestApp(t, nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	requireStatus(t, recorder, http.StatusOK)
}

// TestStatusRecorderFlushAndUnwrap keeps http.ResponseController working.
func TestStatusRecorderFlushAndUnwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	wrapped := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}
	if wrapped.Unwrap() != inner {
		t.Fatalf("Unwrap should return the inner writer")
	}
	wrapped.Flush()
}

// TestRunWithContextShutdown covers startup purge, the cleanup worker and graceful stop.
func TestRunWithContextShutdown(t *testing.T) {
	app := newTestApp(t, func(settings *config.Settings) {
		settings.AppHost = "127.0.0.1"
		settings.AppPort = 0
		settings.CleanupIntervalSeconds = 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := app.RunWithContext(ctx); err != nil {
		t.Fatalf("RunWithContext should shut down cleanly, got %v", err)
	}
}
