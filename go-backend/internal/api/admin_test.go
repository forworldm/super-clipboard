package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pixia1234/super-clipboard/backend/internal/config"
)

func doAdmin(t *testing.T, app *App, method string, target string, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	return recorder
}

func createTextClipForEnv(t *testing.T, app *App, environmentID string, text string) string {
	t.Helper()
	rec := do(t, app, http.MethodPost, "/api/clips", map[string]interface{}{
		"type":          "text",
		"expiresAt":     futureTimestamp(2),
		"maxDownloads":  5,
		"environmentId": environmentID,
		"payload":       map[string]interface{}{"text": text},
	})
	requireStatus(t, rec, http.StatusCreated)
	return decode(t, rec)["id"].(string)
}

func TestAdminAPIDisabledReturns404(t *testing.T) {
	app := newTestApp(t, nil)
	rec := doAdmin(t, app, http.MethodGet, "/api/admin/environments", "whatever")
	requireStatus(t, rec, http.StatusNotFound)
	if decode(t, rec)["detail"] != "Not Found" {
		t.Fatalf("unexpected detail %v", decode(t, rec))
	}
}

func TestAdminAPIUnauthorized(t *testing.T) {
	app := newTestApp(t, func(s *config.Settings) {
		s.AdminAPIKey = "super-secret-admin-key"
	})

	missing := doAdmin(t, app, http.MethodGet, "/api/admin/environments", "")
	requireStatus(t, missing, http.StatusUnauthorized)
	if missing.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("missing WWW-Authenticate header")
	}
	if decode(t, missing)["detail"] != "Unauthorized" {
		t.Fatalf("unexpected detail %v", decode(t, missing))
	}

	wrong := doAdmin(t, app, http.MethodGet, "/api/admin/environments", "wrong-token")
	requireStatus(t, wrong, http.StatusUnauthorized)
	if decode(t, wrong)["detail"] != "Unauthorized" {
		t.Fatalf("unexpected detail %v", decode(t, wrong))
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/environments", nil)
	req.Header.Set("Authorization", "Basic abc")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusUnauthorized)
}

func TestAdminAPIListAndManageClips(t *testing.T) {
	app := newTestApp(t, func(s *config.Settings) {
		s.AdminAPIKey = "admin-1234567890"
	})
	idA := createTextClipForEnv(t, app, "env-a", "aaa")
	idB := createTextClipForEnv(t, app, "env-b", "bbb")

	envs := doAdmin(t, app, http.MethodGet, "/api/admin/environments", "admin-1234567890")
	requireStatus(t, envs, http.StatusOK)
	var envPayload struct {
		Items []struct {
			EnvironmentID string `json:"environmentId"`
			ClipCount     int64  `json:"clipCount"`
		} `json:"items"`
	}
	if err := json.Unmarshal(envs.Body.Bytes(), &envPayload); err != nil {
		t.Fatalf("unable to decode env payload: %v", err)
	}
	if len(envPayload.Items) < 2 {
		t.Fatalf("expected at least 2 environments, got %d", len(envPayload.Items))
	}

	list := doAdmin(t, app, http.MethodGet, "/api/admin/clips?limit=10&offset=0", "admin-1234567890")
	requireStatus(t, list, http.StatusOK)
	payload := decode(t, list)
	items, ok := payload["items"].([]interface{})
	if !ok || len(items) < 2 {
		t.Fatalf("unexpected items %v", payload["items"])
	}
	foundA, foundB := false, false
	for _, raw := range items {
		item := raw.(map[string]interface{})
		if item["id"] == idA {
			foundA = true
			if item["environmentId"] != "env-a" {
				t.Fatalf("idA environment mismatch: %v", item)
			}
		}
		if item["id"] == idB {
			foundB = true
			if item["environmentId"] != "env-b" {
				t.Fatalf("idB environment mismatch: %v", item)
			}
		}
	}
	if !foundA || !foundB {
		t.Fatalf("admin list missing clips: foundA=%v foundB=%v items=%v", foundA, foundB, items)
	}

	one := doAdmin(t, app, http.MethodGet, "/api/admin/clips/"+idA, "admin-1234567890")
	requireStatus(t, one, http.StatusOK)
	if decode(t, one)["environmentId"] != "env-a" {
		t.Fatalf("unexpected admin get payload %v", decode(t, one))
	}

	del := doAdmin(t, app, http.MethodDelete, "/api/admin/clips/"+idA, "admin-1234567890")
	requireStatus(t, del, http.StatusOK)
	if decode(t, del)["ok"] != true {
		t.Fatalf("unexpected delete payload %v", decode(t, del))
	}

	missing := doAdmin(t, app, http.MethodGet, "/api/admin/clips/"+idA, "admin-1234567890")
	requireStatus(t, missing, http.StatusNotFound)
	if !strings.Contains(missing.Body.String(), "片段未找到") {
		t.Fatalf("unexpected detail %s", missing.Body.String())
	}

	// sanity: idB should still be readable by admin
	rec := doAdmin(t, app, http.MethodGet, "/api/admin/clips/"+idB, "admin-1234567890")
	requireStatus(t, rec, http.StatusOK)
	if rec.Body == nil {
		t.Fatal("expected body")
	}
	_, _ = io.ReadAll(rec.Body)
}
