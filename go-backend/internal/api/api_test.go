package api

import (
	"encoding/base64"
	"encoding/json"
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
	"github.com/pixia1234/super-clipboard/backend/internal/repository"
)

// newTestApp mirrors build_client() from backend/tests/test_clips.py.
func newTestApp(t *testing.T, mutate func(*config.Settings)) *App {
	t.Helper()
	dir := t.TempDir()

	settings := config.Defaults()
	settings.DatabasePath = filepath.Join(dir, "clips.db")
	settings.FileStorageDir = filepath.Join(dir, "files")
	settings.StaticRoot = filepath.Join(dir, "static")
	settings.CleanupIntervalSeconds = 3600
	if mutate != nil {
		mutate(settings)
	}

	repo, err := repository.NewClipRepository(settings)
	if err != nil {
		t.Fatalf("unable to open repository: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	app := NewApp(settings, repo)
	app.logger = log.New(io.Discard, "", 0)
	return app
}

func futureTimestamp(hours int) int64 {
	return time.Now().UTC().Add(time.Duration(hours) * time.Hour).UnixMilli()
}

func do(t *testing.T, app *App, method string, target string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("unable to encode request body: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	}
	request := httptest.NewRequest(method, target, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unable to decode response %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func requireStatus(t *testing.T, recorder *httptest.ResponseRecorder, expected int) {
	t.Helper()
	if recorder.Code != expected {
		t.Fatalf("expected status %d, got %d (body: %s)", expected, recorder.Code, recorder.Body.String())
	}
}

func textClipBody(environmentID string, code *string, maxDownloads int) map[string]interface{} {
	body := map[string]interface{}{
		"type":          "text",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  maxDownloads,
		"environmentId": environmentID,
		"payload":       map[string]interface{}{"text": "hello fastapi"},
	}
	if code != nil {
		body["accessCode"] = *code
	}
	return body
}

// TestCreateAndFetchTextClip ports test_create_and_fetch_text_clip.
func TestCreateAndFetchTextClip(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-12345"
	code := "54321"

	recorder := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, &code, 2))
	requireStatus(t, recorder, http.StatusCreated)
	clip := decode(t, recorder)

	if clip["type"] != "text" {
		t.Fatalf("expected type text, got %v", clip["type"])
	}
	if clip["maxDownloads"] != float64(2) {
		t.Fatalf("expected maxDownloads 2, got %v", clip["maxDownloads"])
	}
	payload, ok := clip["payload"].(map[string]interface{})
	if !ok || payload["text"] != "hello fastapi" {
		t.Fatalf("unexpected payload %v", clip["payload"])
	}
	if clip["accessCode"] != code {
		t.Fatalf("expected accessCode %s, got %v", code, clip["accessCode"])
	}
	if clip["accessToken"] != nil {
		t.Fatalf("expected accessToken null, got %v", clip["accessToken"])
	}
	if clip["directUrl"] != "http://example.com/"+code {
		t.Fatalf("unexpected directUrl %v", clip["directUrl"])
	}

	listRecorder := do(t, app, http.MethodGet, "/api/clips?environmentId="+environmentID, nil)
	requireStatus(t, listRecorder, http.StatusOK)
	items := decode(t, listRecorder)["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].(map[string]interface{})["id"] != clip["id"] {
		t.Fatalf("listed clip id mismatch: %v vs %v", items[0], clip["id"])
	}

	otherRecorder := do(t, app, http.MethodGet, "/api/clips?environmentId=another", nil)
	requireStatus(t, otherRecorder, http.StatusOK)
	otherItems := decode(t, otherRecorder)["items"].([]interface{})
	if len(otherItems) != 0 {
		t.Fatalf("expected empty list for foreign environment, got %v", otherItems)
	}

	direct := do(t, app, http.MethodGet, "/"+code, nil)
	requireStatus(t, direct, http.StatusOK)
	if !strings.Contains(direct.Body.String(), "hello fastapi") {
		t.Fatalf("direct link body missing content: %s", direct.Body.String())
	}
	if contentType := direct.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Fatalf("unexpected content type %q", contentType)
	}

	second := do(t, app, http.MethodGet, "/"+code, nil)
	requireStatus(t, second, http.StatusOK)

	exhausted := do(t, app, http.MethodGet, "/"+code, nil)
	requireStatus(t, exhausted, http.StatusNotFound)
}

// TestFileClipDownloadLimit ports test_file_clip_download_limit.
func TestFileClipDownloadLimit(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-file"
	fileContent := []byte("sample file")
	dataURL := "data:text/plain;base64," + base64.StdEncoding.EncodeToString(fileContent)

	recorder := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "file",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  1,
		"environmentId": environmentID,
		"payload": map[string]interface{}{
			"file": map[string]interface{}{
				"name":    "sample.txt",
				"size":    len(fileContent),
				"type":    "text/plain",
				"dataUrl": dataURL,
			},
		},
	})
	requireStatus(t, recorder, http.StatusCreated)
	clip := decode(t, recorder)
	clipID := clip["id"].(string)

	filePayload, ok := clip["payload"].(map[string]interface{})["file"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a file payload, got %v", clip["payload"])
	}
	if filePayload["name"] != "sample.txt" || filePayload["size"] != float64(len(fileContent)) {
		t.Fatalf("unexpected file payload %v", filePayload)
	}
	if filePayload["type"] != "text/plain" {
		t.Fatalf("unexpected mime %v", filePayload["type"])
	}
	expectedDownloadURL := "http://example.com/api/clips/" + clipID + "/file?environmentId=" + environmentID
	if filePayload["downloadUrl"] != expectedDownloadURL {
		t.Fatalf("unexpected downloadUrl %v", filePayload["downloadUrl"])
	}

	download := do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+environmentID, nil)
	requireStatus(t, download, http.StatusOK)
	if download.Body.String() != string(fileContent) {
		t.Fatalf("unexpected download body %q", download.Body.String())
	}
	if disposition := download.Header().Get("Content-Disposition"); disposition != `attachment; filename="sample.txt"` {
		t.Fatalf("unexpected content disposition %q", disposition)
	}

	second := do(t, app, http.MethodGet, "/api/clips/"+clipID+"/file?environmentId="+environmentID, nil)
	if second.Code != http.StatusNotFound && second.Code != http.StatusGone {
		t.Fatalf("expected 404/410 after exhausting downloads, got %d", second.Code)
	}
}

// TestTokenDirectAccess ports test_token_direct_access.
func TestTokenDirectAccess(t *testing.T) {
	app := newTestApp(t, nil)

	register := do(t, app, http.MethodPost, "/api/tokens/register", map[string]interface{}{"token": "pixia1234"})
	requireStatus(t, register, http.StatusOK)
	registered := decode(t, register)
	environmentID := registered["environmentId"].(string)
	if environmentID == "" {
		t.Fatalf("expected a generated environmentId, got %v", registered)
	}
	if registered["token"] != "pixia1234" {
		t.Fatalf("unexpected token %v", registered["token"])
	}
	if registered["lastUsedAt"] != nil {
		t.Fatalf("expected lastUsedAt null, got %v", registered["lastUsedAt"])
	}

	recorder := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "text",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  3,
		"accessToken":   "pixia1234",
		"environmentId": environmentID,
		"payload":       map[string]interface{}{"text": "hello from token"},
	})
	requireStatus(t, recorder, http.StatusCreated)
	clip := decode(t, recorder)
	if clip["accessToken"] != "pixia1234" {
		t.Fatalf("unexpected accessToken %v", clip["accessToken"])
	}
	if clip["accessCode"] != nil {
		t.Fatalf("expected accessCode null, got %v", clip["accessCode"])
	}

	direct := do(t, app, http.MethodGet, "/pixia1234", nil)
	requireStatus(t, direct, http.StatusOK)
	if !strings.Contains(direct.Body.String(), "hello from token") {
		t.Fatalf("direct token link missing content: %s", direct.Body.String())
	}

	raw := do(t, app, http.MethodGet, "/pixia1234/raw", nil)
	requireStatus(t, raw, http.StatusOK)
	if raw.Body.String() != "hello from token" {
		t.Fatalf("unexpected raw body %q", raw.Body.String())
	}
	if contentType := raw.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Fatalf("unexpected raw content type %q", contentType)
	}
}

// TestTokenRegisterConflict ports test_token_register_conflict.
func TestTokenRegisterConflict(t *testing.T) {
	app := newTestApp(t, nil)

	first := do(t, app, http.MethodPost, "/api/tokens/register", map[string]interface{}{"token": "repeat123"})
	requireStatus(t, first, http.StatusOK)
	environmentID := decode(t, first)["environmentId"].(string)

	repeatSame := do(t, app, http.MethodPost, "/api/tokens/register", map[string]interface{}{
		"token":         "repeat123",
		"environmentId": environmentID,
	})
	requireStatus(t, repeatSame, http.StatusOK)
	if decode(t, repeatSame)["environmentId"] != environmentID {
		t.Fatalf("owner should be stable, got %v", decode(t, repeatSame))
	}

	conflict := do(t, app, http.MethodPost, "/api/tokens/register", map[string]interface{}{"token": "repeat123"})
	requireStatus(t, conflict, http.StatusConflict)
	detail := decode(t, conflict)["detail"].(string)
	if !strings.Contains(detail, "被其他设备占用") {
		t.Fatalf("unexpected conflict detail %q", detail)
	}
}

// TestCaptchaRequiredWhenEnabled ports test_captcha_required_when_enabled.
func TestCaptchaRequiredWhenEnabled(t *testing.T) {
	app := newTestApp(t, func(settings *config.Settings) {
		settings.CaptchaProvider = "turnstile"
		settings.CaptchaSecret = "dummy-secret"
		settings.CaptchaBypassToken = "pass-me"
		settings.CaptchaSiteKey = "dummy-site-key"
	})
	environmentID := "captcha-owner"

	missing := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, nil, 1))
	requireStatus(t, missing, http.StatusBadRequest)
	if !strings.Contains(decode(t, missing)["detail"].(string), "验证码") {
		t.Fatalf("unexpected detail %v", decode(t, missing))
	}

	body := textClipBody(environmentID, nil, 1)
	body["payload"] = map[string]interface{}{"text": "captcha ok"}
	body["captchaToken"] = "pass-me"
	body["captchaProvider"] = "turnstile"
	ok := do(t, app, http.MethodPost, "/api/clips", body)
	requireStatus(t, ok, http.StatusCreated)
	payload := decode(t, ok)["payload"].(map[string]interface{})
	if payload["text"] != "captcha ok" {
		t.Fatalf("unexpected payload %v", payload)
	}
}

// TestConfigEndpoint ports test_config_endpoint.
func TestConfigEndpoint(t *testing.T) {
	app := newTestApp(t, func(settings *config.Settings) {
		settings.CaptchaProvider = "turnstile"
		settings.CaptchaSecret = "dummy-secret"
		settings.CaptchaSiteKey = "site-key-demo"
	})

	recorder := do(t, app, http.MethodGet, "/api/config", nil)
	requireStatus(t, recorder, http.StatusOK)
	payload := decode(t, recorder)
	if payload["captchaProvider"] != "turnstile" {
		t.Fatalf("unexpected captchaProvider %v", payload["captchaProvider"])
	}
	if payload["captchaSiteKey"] != "site-key-demo" {
		t.Fatalf("unexpected captchaSiteKey %v", payload["captchaSiteKey"])
	}
}

// TestConfigEndpointWithoutCaptcha checks the default (disabled) configuration.
func TestConfigEndpointWithoutCaptcha(t *testing.T) {
	app := newTestApp(t, nil)
	recorder := do(t, app, http.MethodGet, "/api/config", nil)
	requireStatus(t, recorder, http.StatusOK)
	payload := decode(t, recorder)
	if payload["captchaProvider"] != nil || payload["captchaSiteKey"] != nil {
		t.Fatalf("expected nulls, got %v", payload)
	}
}

// TestHealthz covers GET /healthz.
func TestHealthz(t *testing.T) {
	app := newTestApp(t, nil)
	recorder := do(t, app, http.MethodGet, "/healthz", nil)
	requireStatus(t, recorder, http.StatusOK)
	payload := decode(t, recorder)
	if payload["ok"] != true {
		t.Fatalf("expected ok true, got %v", payload)
	}
	if _, ok := payload["timestamp"].(float64); !ok {
		t.Fatalf("expected numeric timestamp, got %v", payload["timestamp"])
	}
}

// TestIndexWithoutStaticBundle covers GET / when dist/ has not been built.
func TestIndexWithoutStaticBundle(t *testing.T) {
	app := newTestApp(t, nil)
	recorder := do(t, app, http.MethodGet, "/", nil)
	requireStatus(t, recorder, http.StatusOK)
	payload := decode(t, recorder)
	if payload["name"] != "Super Clipboard API" || payload["ok"] != true {
		t.Fatalf("unexpected index payload %v", payload)
	}
}

// TestIndexServesStaticBundle covers the FileResponse branch of GET /.
func TestIndexServesStaticBundle(t *testing.T) {
	app := newTestApp(t, nil)
	if err := os.MkdirAll(app.Settings.StaticRoot, 0o755); err != nil {
		t.Fatalf("unable to create static root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(app.Settings.StaticRoot, "index.html"), []byte("<html>bundle</html>"), 0o644); err != nil {
		t.Fatalf("unable to write index.html: %v", err)
	}

	// The mount list is computed at construction time, so rebuild the app the
	// same way `python -m backend` would after `npm run build`.
	app = NewApp(app.Settings, app.Repo)
	app.logger = log.New(io.Discard, "", 0)

	recorder := do(t, app, http.MethodGet, "/", nil)
	requireStatus(t, recorder, http.StatusOK)
	if recorder.Body.String() != "<html>bundle</html>" {
		t.Fatalf("unexpected index body %q", recorder.Body.String())
	}

	asset := do(t, app, http.MethodGet, "/static/index.html", nil)
	requireStatus(t, asset, http.StatusOK)
	if asset.Body.String() != "<html>bundle</html>" {
		t.Fatalf("unexpected static body %q", asset.Body.String())
	}
}

// TestCreateClipValidationErrors checks the pydantic compatible 422 payloads.
func TestCreateClipValidationErrors(t *testing.T) {
	app := newTestApp(t, nil)

	cases := []struct {
		name string
		body map[string]interface{}
		want string
	}{
		{
			name: "missing text payload",
			body: map[string]interface{}{
				"type":          "text",
				"expiresAt":     futureTimestamp(1),
				"environmentId": "owner-1",
				"payload":       map[string]interface{}{},
			},
			want: "文本片段需要 text 字段",
		},
		{
			name: "text clip carrying a file",
			body: map[string]interface{}{
				"type":          "text",
				"expiresAt":     futureTimestamp(1),
				"environmentId": "owner-1",
				"payload": map[string]interface{}{
					"text": "value",
					"file": map[string]interface{}{"name": "a.txt", "size": 1, "dataUrl": "data:text/plain;base64,YQ=="},
				},
			},
			want: "文本片段不应包含文件数据",
		},
		{
			name: "file clip without file",
			body: map[string]interface{}{
				"type":          "file",
				"expiresAt":     futureTimestamp(1),
				"environmentId": "owner-1",
				"payload":       map[string]interface{}{},
			},
			want: "文件片段需要 file 数据",
		},
		{
			name: "access code with symbols",
			body: map[string]interface{}{
				"type":          "text",
				"expiresAt":     futureTimestamp(1),
				"accessCode":    "ab-cd",
				"environmentId": "owner-1",
				"payload":       map[string]interface{}{"text": "value"},
			},
			want: "直链码需由字母或数字组成",
		},
		{
			name: "expires in the past",
			body: map[string]interface{}{
				"type":          "text",
				"expiresAt":     -1,
				"environmentId": "owner-1",
				"payload":       map[string]interface{}{"text": "value"},
			},
			want: "greater_than",
		},
		{
			name: "unknown type",
			body: map[string]interface{}{
				"type":          "image",
				"expiresAt":     futureTimestamp(1),
				"environmentId": "owner-1",
				"payload":       map[string]interface{}{"text": "value"},
			},
			want: "literal_error",
		},
		{
			name: "invalid data url",
			body: map[string]interface{}{
				"type":          "file",
				"expiresAt":     futureTimestamp(1),
				"environmentId": "owner-1",
				"payload": map[string]interface{}{
					"file": map[string]interface{}{"name": "a.txt", "size": 1, "dataUrl": "https://example.com/a.txt"},
				},
			},
			want: "file dataUrl must be a base64 data URI",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := do(t, app, http.MethodPost, "/api/clips", testCase.body)
			requireStatus(t, recorder, http.StatusUnprocessableEntity)
			if !strings.Contains(recorder.Body.String(), testCase.want) {
				t.Fatalf("expected %q in body %s", testCase.want, recorder.Body.String())
			}
			var payload struct {
				Detail []map[string]interface{} `json:"detail"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("detail should be a list: %v", err)
			}
			if len(payload.Detail) == 0 {
				t.Fatalf("expected at least one validation error")
			}
		})
	}
}

// TestMissingEnvironmentIdQuery checks the required query parameter handling.
func TestMissingEnvironmentIdQuery(t *testing.T) {
	app := newTestApp(t, nil)

	recorder := do(t, app, http.MethodGet, "/api/clips", nil)
	requireStatus(t, recorder, http.StatusUnprocessableEntity)
	if !strings.Contains(recorder.Body.String(), `"query"`) {
		t.Fatalf("expected a query level error, got %s", recorder.Body.String())
	}

	blank := do(t, app, http.MethodGet, "/api/clips?environmentId=%20", nil)
	requireStatus(t, blank, http.StatusBadRequest)
	if decode(t, blank)["detail"] != "environmentId 缺失" {
		t.Fatalf("unexpected detail %v", decode(t, blank))
	}
}

// TestDuplicateAccessCodeConflict covers the 409 mapping in create_clip.
func TestDuplicateAccessCodeConflict(t *testing.T) {
	app := newTestApp(t, nil)
	code := "77777"
	environmentID := "owner-dup"

	first := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, &code, 5))
	requireStatus(t, first, http.StatusCreated)

	second := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, &code, 5))
	requireStatus(t, second, http.StatusConflict)
	if decode(t, second)["detail"] != "直链码已存在，请刷新后再试" {
		t.Fatalf("unexpected detail %v", decode(t, second))
	}
}

// TestTokenFlowOnCreateClip covers ensure_token_owner + auto registration.
func TestTokenFlowOnCreateClip(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "env-token-flow"

	// Unregistered token: main.py registers it on the fly and returns 201.
	recorder := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "text",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  5,
		"accessToken":   "brandnewtoken",
		"environmentId": environmentID,
		"payload":       map[string]interface{}{"text": "auto registered"},
	})
	requireStatus(t, recorder, http.StatusCreated)

	// Same owner can reuse the token.
	reuse := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "text",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  5,
		"accessToken":   "brandnewtoken",
		"environmentId": environmentID,
		"payload":       map[string]interface{}{"text": "reused"},
	})
	requireStatus(t, reuse, http.StatusCreated)

	// A different device is rejected with 409.
	conflict := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "text",
		"expiresAt":     futureTimestamp(1),
		"maxDownloads":  5,
		"accessToken":   "brandnewtoken",
		"environmentId": "someone-else",
		"payload":       map[string]interface{}{"text": "intruder"},
	})
	requireStatus(t, conflict, http.StatusConflict)
	if decode(t, conflict)["detail"] != "持久 Token 已被其他设备占用，请稍后重试" {
		t.Fatalf("unexpected detail %v", decode(t, conflict))
	}
}

// TestDeleteAndDownloadTracking covers DELETE /api/clips/{id} and the
// /download counter endpoint.
func TestDeleteAndDownloadTracking(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-delete"

	recorder := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, nil, 5))
	requireStatus(t, recorder, http.StatusCreated)
	clipID := decode(t, recorder)["id"].(string)

	fetched := do(t, app, http.MethodGet, "/api/clips/"+clipID+"?environmentId="+environmentID, nil)
	requireStatus(t, fetched, http.StatusOK)
	if decode(t, fetched)["id"] != clipID {
		t.Fatalf("unexpected clip %v", decode(t, fetched))
	}

	foreign := do(t, app, http.MethodGet, "/api/clips/"+clipID+"?environmentId=intruder", nil)
	requireStatus(t, foreign, http.StatusNotFound)
	if decode(t, foreign)["detail"] != "片段未找到" {
		t.Fatalf("unexpected detail %v", decode(t, foreign))
	}

	increment := do(t, app, http.MethodPost, "/api/clips/"+clipID+"/download?environmentId="+environmentID, nil)
	requireStatus(t, increment, http.StatusOK)
	incrementPayload := decode(t, increment)
	if incrementPayload["removed"] != false {
		t.Fatalf("expected removed false, got %v", incrementPayload["removed"])
	}
	clipPayload := incrementPayload["clip"].(map[string]interface{})
	if clipPayload["downloadCount"] != float64(1) {
		t.Fatalf("expected downloadCount 1, got %v", clipPayload["downloadCount"])
	}

	missing := do(t, app, http.MethodPost, "/api/clips/"+clipID+"/download?environmentId=intruder", nil)
	requireStatus(t, missing, http.StatusNotFound)

	deleted := do(t, app, http.MethodDelete, "/api/clips/"+clipID+"?environmentId="+environmentID, nil)
	requireStatus(t, deleted, http.StatusOK)
	if decode(t, deleted)["ok"] != true {
		t.Fatalf("expected ok true, got %v", decode(t, deleted))
	}

	again := do(t, app, http.MethodDelete, "/api/clips/"+clipID+"?environmentId="+environmentID, nil)
	requireStatus(t, again, http.StatusNotFound)
}

// TestDownloadExhaustionReturns410 covers the 410 branch of /download: the
// clip that reaches its limit is destroyed right away, so the next call is 404.
func TestDownloadExhaustionReturns410(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-exhaust"

	recorder := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, nil, 2))
	requireStatus(t, recorder, http.StatusCreated)
	clipID := decode(t, recorder)["id"].(string)

	first := do(t, app, http.MethodPost, "/api/clips/"+clipID+"/download?environmentId="+environmentID, nil)
	requireStatus(t, first, http.StatusOK)
	if decode(t, first)["removed"] != false {
		t.Fatalf("expected removed false, got %v", decode(t, first))
	}

	second := do(t, app, http.MethodPost, "/api/clips/"+clipID+"/download?environmentId="+environmentID, nil)
	requireStatus(t, second, http.StatusGone)
	if decode(t, second)["detail"] != "片段已过期或销毁" {
		t.Fatalf("unexpected detail %v", decode(t, second))
	}

	third := do(t, app, http.MethodPost, "/api/clips/"+clipID+"/download?environmentId="+environmentID, nil)
	requireStatus(t, third, http.StatusNotFound)
	if decode(t, third)["detail"] != "片段未找到" {
		t.Fatalf("unexpected detail %v", decode(t, third))
	}
}

// TestOwnerScopedShortCode covers the `owner.code` identifier syntax and the
// /api/clips/code/{access_code} endpoint.
func TestOwnerScopedShortCode(t *testing.T) {
	app := newTestApp(t, nil)
	environmentID := "owner-scoped"
	code := "24680"

	recorder := do(t, app, http.MethodPost, "/api/clips", textClipBody(environmentID, &code, 10))
	requireStatus(t, recorder, http.StatusCreated)
	clipID := decode(t, recorder)["id"].(string)

	byCode := do(t, app, http.MethodGet, "/api/clips/code/"+code, nil)
	requireStatus(t, byCode, http.StatusOK)
	if decode(t, byCode)["id"] != clipID {
		t.Fatalf("unexpected clip %v", decode(t, byCode))
	}

	scoped := do(t, app, http.MethodGet, "/"+environmentID+"."+code+"/raw", nil)
	requireStatus(t, scoped, http.StatusOK)
	if scoped.Body.String() != "hello fastapi" {
		t.Fatalf("unexpected raw body %q", scoped.Body.String())
	}

	unknown := do(t, app, http.MethodGet, "/nope0000", nil)
	requireStatus(t, unknown, http.StatusNotFound)
	if decode(t, unknown)["detail"] != "直链不存在或已过期" {
		t.Fatalf("unexpected detail %v", decode(t, unknown))
	}
}

// TestMethodNotAllowedAndSlashRedirect covers Starlette's 405/307 behaviour.
func TestMethodNotAllowedAndSlashRedirect(t *testing.T) {
	app := newTestApp(t, nil)

	recorder := do(t, app, http.MethodPost, "/healthz", nil)
	requireStatus(t, recorder, http.StatusMethodNotAllowed)
	if decode(t, recorder)["detail"] != "Method Not Allowed" {
		t.Fatalf("unexpected detail %v", decode(t, recorder))
	}
	if allow := recorder.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("expected Allow header, got %q", allow)
	}

	redirect := do(t, app, http.MethodGet, "/api/clips/?environmentId=owner", nil)
	requireStatus(t, redirect, http.StatusTemporaryRedirect)
	if location := redirect.Header().Get("Location"); location != "/api/clips?environmentId=owner" {
		t.Fatalf("unexpected redirect location %q", location)
	}

	missing := do(t, app, http.MethodGet, "/api/unknown", nil)
	requireStatus(t, missing, http.StatusNotFound)
	if decode(t, missing)["detail"] != "Not Found" {
		t.Fatalf("unexpected detail %v", decode(t, missing))
	}
}

// TestCorsBehaviour mirrors CORSMiddleware(allow_origins=["*"], ...).
func TestCorsBehaviour(t *testing.T) {
	app := newTestApp(t, nil)

	request := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	request.Header.Set("Origin", "https://clip.example.com")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	requireStatus(t, recorder, http.StatusOK)
	if origin := recorder.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Fatalf("unexpected allow origin %q", origin)
	}

	preflight := httptest.NewRequest(http.MethodOptions, "/api/clips", nil)
	preflight.Header.Set("Origin", "https://clip.example.com")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflightRecorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(preflightRecorder, preflight)
	requireStatus(t, preflightRecorder, http.StatusOK)
	if preflightRecorder.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight missing allow origin")
	}
	if preflightRecorder.Header().Get("Access-Control-Max-Age") != "600" {
		t.Fatalf("preflight missing max age")
	}
}

// TestFileSizeLimit covers the max_file_size_bytes guard.
func TestFileSizeLimit(t *testing.T) {
	app := newTestApp(t, func(settings *config.Settings) {
		settings.MaxFileSizeBytes = 8
	})
	oversized := base64.StdEncoding.EncodeToString([]byte("0123456789"))

	recorder := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "file",
		"expiresAt":     futureTimestamp(1),
		"environmentId": "owner-size",
		"payload": map[string]interface{}{
			"file": map[string]interface{}{
				"name":    "big.txt",
				"size":    10,
				"type":    "text/plain",
				"dataUrl": "data:text/plain;base64," + oversized,
			},
		},
	})
	requireStatus(t, recorder, http.StatusBadRequest)
	if decode(t, recorder)["detail"] != "文件体积超过限制" {
		t.Fatalf("unexpected detail %v", decode(t, recorder))
	}

	entries, err := os.ReadDir(app.Settings.FileStorageDir)
	if err != nil {
		t.Fatalf("unable to inspect storage dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected upload should be removed, found %d file(s)", len(entries))
	}
}

// TestInvalidBase64Payload covers storage.ParseDataURL failures.
func TestInvalidBase64Payload(t *testing.T) {
	app := newTestApp(t, nil)

	recorder := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "file",
		"expiresAt":     futureTimestamp(1),
		"environmentId": "owner-b64",
		"payload": map[string]interface{}{
			"file": map[string]interface{}{
				"name":    "broken.txt",
				"size":    1,
				"type":    "text/plain",
				"dataUrl": "data:text/plain;base64,!!!not-base64!!!",
			},
		},
	})
	requireStatus(t, recorder, http.StatusBadRequest)
	if decode(t, recorder)["detail"] != "文件数据解码失败" {
		t.Fatalf("unexpected detail %v", decode(t, recorder))
	}
}
