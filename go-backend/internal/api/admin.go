package api

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"

	"github.com/pixia1234/super-clipboard/backend/internal/schemas"
	"github.com/pixia1234/super-clipboard/backend/internal/utils"
)

func (a *App) requireAdminAuth(w http.ResponseWriter, r *http.Request) bool {
	secret := strings.TrimSpace(a.Settings.AdminAPIKey)
	if secret == "" {
		writeError(w, newHTTPError(http.StatusNotFound, "Not Found"))
		return false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
		writeError(w, newHTTPError(http.StatusUnauthorized, "Unauthorized"))
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
		writeError(w, newHTTPError(http.StatusUnauthorized, "Unauthorized"))
		return false
	}
	if len(token) != len(secret) || subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
		writeError(w, newHTTPError(http.StatusUnauthorized, "Unauthorized"))
		return false
	}
	return true
}

func parsePositiveInt(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return fallback
	}
	return v
}

func (a *App) handleAdminListEnvironments(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdminAuth(w, r) {
		return
	}
	items, err := a.Repo.ListEnvironmentsAdmin()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

func (a *App) handleAdminListClips(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdminAuth(w, r) {
		return
	}
	environmentID := strings.TrimSpace(r.URL.Query().Get("environmentId"))
	limit := parsePositiveInt(r.URL.Query().Get("limit"), 50)
	offset := parsePositiveInt(r.URL.Query().Get("offset"), 0)
	clips, total, err := a.Repo.ListClipsAdmin(environmentID, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	baseURL := utils.BuildBaseURL(r)
	items := make([]map[string]interface{}, 0, len(clips))
	for _, clip := range clips {
		resp := schemas.ClipFromModel(clip, baseURL)
		items = append(items, map[string]interface{}{
			"id":            resp.ID,
			"type":          resp.Type,
			"createdAt":     resp.CreatedAt,
			"expiresAt":     resp.ExpiresAt,
			"maxDownloads":  resp.MaxDownloads,
			"downloadCount": resp.DownloadCount,
			"accessCode":    resp.AccessCode,
			"accessToken":   resp.AccessToken,
			"payload":       resp.Payload,
			"directUrl":     resp.DirectURL,
			"environmentId": clip.EnvironmentID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":   items,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"hasMore": int64(offset+len(items)) < total,
	})
}

func (a *App) handleAdminGetClip(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdminAuth(w, r) {
		return
	}
	clipID := strings.TrimSpace(routeParam(r, "clip_id"))
	if clipID == "" {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}
	clip, err := a.Repo.GetClip(clipID)
	if err != nil {
		writeError(w, err)
		return
	}
	if clip == nil {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}
	resp := schemas.ClipFromModel(clip, utils.BuildBaseURL(r))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":            resp.ID,
		"type":          resp.Type,
		"createdAt":     resp.CreatedAt,
		"expiresAt":     resp.ExpiresAt,
		"maxDownloads":  resp.MaxDownloads,
		"downloadCount": resp.DownloadCount,
		"accessCode":    resp.AccessCode,
		"accessToken":   resp.AccessToken,
		"payload":       resp.Payload,
		"directUrl":     resp.DirectURL,
		"environmentId": clip.EnvironmentID,
	})
}

func (a *App) handleAdminDeleteClip(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdminAuth(w, r) {
		return
	}
	clipID := strings.TrimSpace(routeParam(r, "clip_id"))
	if clipID == "" {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}
	clip, err := a.Repo.GetClip(clipID)
	if err != nil {
		writeError(w, err)
		return
	}
	if clip == nil {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}
	removed, err := a.Repo.DeleteClip(clipID, clip.EnvironmentID)
	if err != nil {
		writeError(w, err)
		return
	}
	if !removed {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}
	writeJSON(w, http.StatusOK, schemas.DeleteResponse{OK: true})
}
