package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HTTPError is a structured error for HTTP responses.
type HTTPError struct {
	Status int
	Detail string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%d: %s", e.Status, e.Detail)
}

// JSON writes a JSON response.
func JSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// ErrorResponse is the standard error envelope.
type ErrorResponse struct {
	Detail string `json:"detail"`
}

func writeError(w http.ResponseWriter, status int, detail string) {
	JSON(w, status, ErrorResponse{Detail: detail})
}

// App is the server application, holding dependencies.
type App struct {
	cfg        *Config
	repository *ClipRepository
}

func NewApp(cfg *Config) (*App, error) {
	repo, err := NewClipRepository(cfg)
	if err != nil {
		return nil, err
	}
	return &App{cfg: cfg, repository: repo}, nil
}

// ----- Request/Response types -----

type storedFileInput struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Type    string `json:"type"`
	DataURL string `json:"dataUrl"`
}

type clipPayloadInput struct {
	Text *string          `json:"text"`
	File *storedFileInput `json:"file"`
}

type clipCreateRequest struct {
	Type            string           `json:"type"`
	ExpiresAt       int64            `json:"expiresAt"`
	MaxDownloads    *int             `json:"maxDownloads"`
	AccessCode      *string          `json:"accessCode"`
	AccessToken     *string          `json:"accessToken"`
	EnvironmentID   string           `json:"environmentId"`
	Payload         clipPayloadInput `json:"payload"`
	CaptchaToken    *string          `json:"captchaToken"`
	CaptchaProvider *string          `json:"captchaProvider"`
}

type storedFileResponse struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Type        string `json:"type"`
	DownloadURL string `json:"downloadUrl"`
}

type clipPayloadResponse struct {
	Text *string             `json:"text"`
	File *storedFileResponse `json:"file"`
}

type clipResponse struct {
	ID            string              `json:"id"`
	Type          string              `json:"type"`
	CreatedAt     int64               `json:"createdAt"`
	ExpiresAt     int64               `json:"expiresAt"`
	MaxDownloads  int                 `json:"maxDownloads"`
	DownloadCount int                 `json:"downloadCount"`
	AccessCode    *string             `json:"accessCode"`
	AccessToken   *string             `json:"accessToken"`
	Payload       clipPayloadResponse `json:"payload"`
	DirectURL     *string             `json:"directUrl"`
}

type clipListResponse struct {
	Items []clipResponse `json:"items"`
}

type deleteResponse struct {
	Ok bool `json:"ok"`
}

type incrementResponse struct {
	Clip    clipResponse `json:"clip"`
	Removed bool         `json:"removed"`
}

type appConfigResponse struct {
	CaptchaProvider *string `json:"captchaProvider"`
	CaptchaSiteKey  *string `json:"captchaSiteKey"`
}

type tokenRegisterRequest struct {
	Token         string  `json:"token"`
	EnvironmentID *string `json:"environmentId"`
}

type tokenRegisterResponse struct {
	Token         string  `json:"token"`
	EnvironmentID string  `json:"environmentId"`
	UpdatedAt     int64   `json:"updatedAt"`
	LastUsedAt    *int64  `json:"lastUsedAt"`
	ExpiresAt     int64   `json:"expiresAt"`
}

func (a *App) clipToResponse(clip *Clip, baseURL string) clipResponse {
	var accessCode *string
	if clip.AccessCode != "" {
		code := clip.AccessCode
		accessCode = &code
	}
	var accessToken *string
	if clip.AccessToken != "" {
		tok := clip.AccessToken
		accessToken = &tok
	}
	var text *string
	if clip.Type == "text" {
		t := clip.Text
		text = &t
	}
	var fileResp *storedFileResponse
	if clip.StoredFile != nil {
		fileResp = &storedFileResponse{
			Name:        clip.StoredFile.Name,
			Size:        clip.StoredFile.Size,
			Type:        clip.StoredFile.Mime,
			DownloadURL: fmt.Sprintf("%s/api/clips/%s/file?environmentId=%s", baseURL, clip.ID, clip.EnvironmentID),
		}
	}
	var directURL *string
	if clip.AccessCode != "" {
		u := fmt.Sprintf("%s/%s", baseURL, clip.AccessCode)
		directURL = &u
	}
	return clipResponse{
		ID:            clip.ID,
		Type:          clip.Type,
		CreatedAt:     clip.CreatedAt.Unix() * 1000,
		ExpiresAt:     clip.ExpiresAt.Unix() * 1000,
		MaxDownloads:  clip.MaxDownloads,
		DownloadCount: clip.DownloadCount,
		AccessCode:    accessCode,
		AccessToken:   accessToken,
		Payload: clipPayloadResponse{
			Text: text,
			File: fileResp,
		},
		DirectURL: directURL,
	}
}

// ----- Middleware -----

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// ----- Handlers -----

func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	JSON(w, http.StatusOK, map[string]interface{}{
		"ok":        true,
		"timestamp": time.Now().UTC().UnixMilli(),
	})
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// fall through to mux; but if this handler is registered for "/", we need to distinguish
	}
	indexFile := filepath.Join(a.cfg.StaticRoot, "index.html")
	if fi, err := os.Stat(indexFile); err == nil && !fi.IsDir() {
		http.ServeFile(w, r, indexFile)
		return
	}
	JSON(w, http.StatusOK, map[string]interface{}{
		"name": "Super Clipboard API",
		"ok":   true,
	})
}

func (a *App) handleListClips(w http.ResponseWriter, r *http.Request) {
	environmentID := r.URL.Query().Get("environmentId")
	environmentID = strings.TrimSpace(environmentID)
	if environmentID == "" {
		writeError(w, http.StatusBadRequest, "environmentId 缺失")
		return
	}
	if _, err := a.repository.PurgeInactive(); err != nil {
		log.Printf("purge error: %v", err)
	}
	clips, err := a.repository.ListClips(environmentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	baseURL := buildBaseURL(r)
	items := make([]clipResponse, 0, len(clips))
	for _, c := range clips {
		items = append(items, a.clipToResponse(c, baseURL))
	}
	JSON(w, http.StatusOK, clipListResponse{Items: items})
}

func (a *App) handleRegisterToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "持久 Token 无效")
		return
	}
	var envID string
	if req.EnvironmentID != nil {
		envID = strings.TrimSpace(*req.EnvironmentID)
	}
	record, err := a.repository.RegisterToken(req.Token, envID)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	resp := tokenRegisterResponse{
		Token:         record.Token,
		EnvironmentID: record.EnvironmentID,
		UpdatedAt:     record.UpdatedAt * 1000,
		LastUsedAt:    nil,
		ExpiresAt:     record.ExpiresAt * 1000,
	}
	if record.LastUsedAt != nil {
		v := *record.LastUsedAt * 1000
		resp.LastUsedAt = &v
	}
	JSON(w, http.StatusOK, resp)
}

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	resp := appConfigResponse{}
	if a.cfg.CaptchaProvider != "" {
		p := a.cfg.CaptchaProvider
		resp.CaptchaProvider = &p
	}
	if a.cfg.CaptchaSiteKey != "" {
		k := a.cfg.CaptchaSiteKey
		resp.CaptchaSiteKey = &k
	}
	JSON(w, http.StatusOK, resp)
}

func (a *App) handleGetClipWithID(w http.ResponseWriter, r *http.Request, clipID string) {
	environmentID := strings.TrimSpace(r.URL.Query().Get("environmentId"))
	if environmentID == "" {
		writeError(w, http.StatusBadRequest, "environmentId 缺失")
		return
	}
	clip, err := a.repository.GetClip(clipID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if clip == nil || clip.EnvironmentID != environmentID {
		writeError(w, http.StatusNotFound, "片段未找到")
		return
	}
	if !clip.IsActive() {
		a.repository.DeleteClip(clipID, environmentID)
		writeError(w, http.StatusNotFound, "片段已过期或达到下载次数")
		return
	}
	baseURL := buildBaseURL(r)
	JSON(w, http.StatusOK, a.clipToResponse(clip, baseURL))
}

func (a *App) validateCreateRequest(req *clipCreateRequest) error {
	if req.Type != "text" && req.Type != "file" {
		return errors.New("type 必须是 text 或 file")
	}
	req.EnvironmentID = strings.TrimSpace(req.EnvironmentID)
	if req.EnvironmentID == "" {
		return errors.New("environmentId 缺失")
	}
	if req.ExpiresAt <= 0 {
		return errors.New("expiresAt 无效")
	}
	// Access code validation
	if req.AccessCode != nil {
		code := strings.TrimSpace(*req.AccessCode)
		req.AccessCode = &code
		if code != "" {
			if len(code) < 5 || len(code) > 12 {
				return errors.New("直链码长度需在 5-12 之间")
			}
			for _, ch := range code {
				if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')) {
					return errors.New("直链码需由字母或数字组成")
				}
			}
		} else {
			req.AccessCode = nil
		}
	}
	// Access token
	if req.AccessToken != nil {
		tok := strings.TrimSpace(*req.AccessToken)
		req.AccessToken = &tok
		if len(tok) < 7 {
			return errors.New("accessToken 长度不足")
		}
	}
	if req.Type == "text" {
		if req.Payload.Text == nil || strings.TrimSpace(*req.Payload.Text) == "" {
			return errors.New("文本片段需要 text 字段")
		}
		if req.Payload.File != nil {
			return errors.New("文本片段不应包含文件数据")
		}
	} else if req.Type == "file" {
		if req.Payload.File == nil {
			return errors.New("文件片段需要 file 数据")
		}
		if req.Payload.Text != nil {
			return errors.New("文件片段不应包含文本字段")
		}
		if !strings.HasPrefix(req.Payload.File.DataURL, "data:") {
			return errors.New("file dataUrl must be a base64 data URI")
		}
	}
	if req.CaptchaToken != nil {
		t := strings.TrimSpace(*req.CaptchaToken)
		if t == "" {
			req.CaptchaToken = nil
		} else {
			req.CaptchaToken = &t
		}
	}
	return nil
}

func (a *App) handleCreateClip(w http.ResponseWriter, r *http.Request) {
	var req clipCreateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 100*1024*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := a.validateCreateRequest(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Captcha verification
	if a.cfg.CaptchaProvider != "" {
		if a.cfg.CaptchaSecret == "" {
			writeError(w, http.StatusInternalServerError, "验证码服务未正确配置")
			return
		}
		clientIP := extractClientIP(r)
		if isPrivateIP(clientIP) {
			clientIP = ""
		}
		var providedToken string
		if req.CaptchaToken != nil {
			providedToken = *req.CaptchaToken
		}
		if err := verifyCaptchaToken(
			providedToken,
			a.cfg.CaptchaProvider,
			a.cfg.CaptchaSecret,
			clientIP,
			a.cfg.CaptchaTimeoutSeconds,
			a.cfg.CaptchaBypassToken,
		); err != nil {
			var httpErr *HTTPError
			if errors.As(err, &httpErr) {
				writeError(w, httpErr.Status, httpErr.Detail)
			} else {
				writeError(w, http.StatusBadRequest, err.Error())
			}
			return
		}
	}

	accessTokenVal := ""
	if req.AccessToken != nil {
		accessTokenVal = *req.AccessToken
	}
	accessCodeVal := ""
	if req.AccessCode != nil {
		accessCodeVal = *req.AccessCode
	}

	if accessTokenVal != "" {
		_, err := a.repository.EnsureTokenOwner(accessTokenVal, req.EnvironmentID)
		if err != nil {
			msg := err.Error()
			if strings.Contains(msg, "未注册") || strings.Contains(msg, "未找到") {
				_, regErr := a.repository.RegisterToken(accessTokenVal, req.EnvironmentID)
				if regErr != nil {
					status := http.StatusConflict
					if !strings.Contains(regErr.Error(), "已被") {
						status = http.StatusBadRequest
					}
					writeError(w, status, regErr.Error())
					return
				}
			} else {
				status := http.StatusConflict
				if !strings.Contains(msg, "Token") && !strings.Contains(msg, "已被") {
					status = http.StatusBadRequest
				}
				writeError(w, status, msg)
				return
			}
		}
	}

	var storedFile *StoredFile
	var textContent string
	if req.Type == "file" {
		var err error
		storedFile, err = StoreDataURL(a.cfg, req.Payload.File.Name, req.Payload.File.DataURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if storedFile.Size > a.cfg.MaxFileSizeBytes {
			os.Remove(storedFile.Path)
			writeError(w, http.StatusBadRequest, "文件体积超过限制")
			return
		}
	} else {
		textContent = strings.TrimSpace(*req.Payload.Text)
	}

	input := &CreateClipInput{
		Type:          req.Type,
		ExpiresAtMS:   req.ExpiresAt,
		MaxDownloads:  req.MaxDownloads,
		AccessCode:    accessCodeVal,
		AccessToken:   accessTokenVal,
		EnvironmentID: req.EnvironmentID,
		Text:          textContent,
		StoredFile:    storedFile,
	}

	clip, err := a.repository.CreateClip(input)
	if err != nil {
		if storedFile != nil {
			os.Remove(storedFile.Path)
		}
		msg := err.Error()
		status := http.StatusBadRequest
		if strings.Contains(msg, "已存在") || strings.Contains(msg, "Token") {
			status = http.StatusConflict
		}
		writeError(w, status, msg)
		return
	}

	baseURL := buildBaseURL(r)
	JSON(w, http.StatusCreated, a.clipToResponse(clip, baseURL))
}

func (a *App) handleDeleteClipWithID(w http.ResponseWriter, r *http.Request, clipID string) {
	environmentID := strings.TrimSpace(r.URL.Query().Get("environmentId"))
	if environmentID == "" {
		writeError(w, http.StatusBadRequest, "environmentId 缺失")
		return
	}
	removed, err := a.repository.DeleteClip(clipID, environmentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "片段未找到")
		return
	}
	JSON(w, http.StatusOK, deleteResponse{Ok: true})
}

// parseIdentifier splits "owner.remainder" into (owner, remainder). If no dot, owner is empty.
func parseIdentifier(identifier string) (string, string) {
	identifier = strings.TrimSpace(identifier)
	if i := strings.Index(identifier, "."); i >= 0 {
		owner := strings.TrimSpace(identifier[:i])
		remainder := strings.TrimSpace(identifier[i+1:])
		return owner, remainder
	}
	return "", identifier
}

func (a *App) getActiveClip(identifier string) (*Clip, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, &HTTPError{Status: http.StatusNotFound, Detail: "直链不存在或已过期"}
	}
	clip, _ := a.repository.GetClipByCode(identifier)
	ownerHint := ""
	remainder := identifier
	if clip == nil {
		ownerHint, remainder = parseIdentifier(identifier)
		if ownerHint != "" && remainder != "" {
			// If remainder is 5 digits, treat as access code
			if len(remainder) == 5 && isAllDigits(remainder) {
				c, err := a.repository.GetClipByCodeAndOwner(remainder, ownerHint)
				if err == nil && c != nil {
					clip = c
				}
			}
			if clip == nil {
				c, err := a.repository.GetClipByToken(remainder, ownerHint)
				if err == nil {
					clip = c
				}
			}
		}
	}
	if clip == nil {
		c, err := a.repository.GetClipByToken(identifier, "")
		if err == nil {
			clip = c
		}
	}
	if clip == nil {
		return nil, &HTTPError{Status: http.StatusNotFound, Detail: "直链不存在或已过期"}
	}
	if !clip.IsActive() {
		a.repository.DeleteClip(clip.ID, clip.EnvironmentID)
		return nil, &HTTPError{Status: http.StatusNotFound, Detail: "直链不存在或已过期"}
	}
	return clip, nil
}

func isAllDigits(s string) bool {
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func (a *App) dispatchClipResponse(w http.ResponseWriter, r *http.Request, clip *Clip, reached, raw bool) {
	if clip.Type == "text" {
		if reached {
			go a.repository.DeleteClip(clip.ID, clip.EnvironmentID)
		}
		if raw {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, clip.Text)
			return
		}
		html := buildTextClipHTML(clip.Text, clip.CreatedAt, clip.DownloadCount, clip.AccessCode)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, html)
		return
	}
	if clip.StoredFile != nil {
		if reached {
			go a.repository.DeleteClip(clip.ID, clip.EnvironmentID)
		}
		fi, err := os.Stat(clip.StoredFile.Path)
		if err != nil || fi.IsDir() {
			a.repository.DeleteClip(clip.ID, clip.EnvironmentID)
			writeError(w, http.StatusGone, "文件已丢失")
			return
		}
		w.Header().Set("Content-Type", clip.StoredFile.Mime)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", clip.StoredFile.Name))
		http.ServeFile(w, r, clip.StoredFile.Path)
		return
	}
	a.repository.DeleteClip(clip.ID, clip.EnvironmentID)
	writeError(w, http.StatusGone, "文件数据缺失")
}

// background cleanup worker
func (a *App) runCleanupWorker(ctx context.Context) {
	interval := time.Duration(a.cfg.CleanupIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := a.repository.PurgeInactive()
			if err != nil {
				log.Printf("purge error: %v", err)
			} else if n > 0 {
				log.Printf("purged %d inactive clips", n)
			}
		}
	}
}

// recovers from panics in handlers
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleClipSubroutes(w http.ResponseWriter, r *http.Request) {
	// r.URL.Path is like /api/clips/xxx or /api/clips/xxx/yyy
	rest := strings.TrimPrefix(r.URL.Path, "/api/clips/")
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")

	// Route 1: /api/clips/code/{access_code} -> GET
	if len(parts) == 2 && parts[0] == "code" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		accessCode := parts[1]
		clip, err := a.repository.GetClipByCode(accessCode)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if clip == nil {
			writeError(w, http.StatusNotFound, "直链不存在或已过期")
			return
		}
		if !clip.IsActive() {
			a.repository.DeleteClip(clip.ID, clip.EnvironmentID)
			writeError(w, http.StatusNotFound, "直链不存在或已过期")
			return
		}
		baseURL := buildBaseURL(r)
		JSON(w, http.StatusOK, a.clipToResponse(clip, baseURL))
		return
	}

	// Route: /api/clips/{clip_id} -> GET, DELETE
	if len(parts) == 1 {
		clipID := parts[0]
		switch r.Method {
		case http.MethodGet:
			a.handleGetClipWithID(w, r, clipID)
		case http.MethodDelete:
			a.handleDeleteClipWithID(w, r, clipID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	// Route: /api/clips/{clip_id}/download -> POST
	if len(parts) == 2 && parts[1] == "download" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		// Inject the clip_id into path values; but handleTrackDownload uses r.PathValue, so let's re-implement here with a modified request
		// Actually easier: call handleTrackDownload with clip_id parsed here
		clipID := parts[0]
		environmentID := strings.TrimSpace(r.URL.Query().Get("environmentId"))
		if environmentID == "" {
			writeError(w, http.StatusBadRequest, "environmentId 缺失")
			return
		}
		clip, reached, err := a.repository.IncrementDownloads(clipID, environmentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if clip == nil {
			writeError(w, http.StatusNotFound, "片段未找到")
			return
		}
		if !clip.IsActive() && reached {
			a.repository.DeleteClip(clipID, environmentID)
			writeError(w, http.StatusGone, "片段已过期或销毁")
			return
		}
		baseURL := buildBaseURL(r)
		JSON(w, http.StatusOK, incrementResponse{
			Clip:    a.clipToResponse(clip, baseURL),
			Removed: reached,
		})
		return
	}

	// Route: /api/clips/{clip_id}/file -> GET
	if len(parts) == 2 && parts[1] == "file" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		clipID := parts[0]
		environmentID := strings.TrimSpace(r.URL.Query().Get("environmentId"))
		if environmentID == "" {
			writeError(w, http.StatusBadRequest, "environmentId 缺失")
			return
		}
		clip, err := a.repository.GetClip(clipID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if clip == nil {
			writeError(w, http.StatusNotFound, "片段未找到")
			return
		}
		if clip.EnvironmentID != environmentID {
			writeError(w, http.StatusNotFound, "片段未找到")
			return
		}
		if clip.StoredFile == nil {
			a.repository.DeleteClip(clipID, environmentID)
			writeError(w, http.StatusGone, "文件已丢失")
			return
		}
		if !clip.IsActive() {
			a.repository.DeleteClip(clipID, environmentID)
			writeError(w, http.StatusGone, "文件已过期或销毁")
			return
		}
		fi, err := os.Stat(clip.StoredFile.Path)
		if err != nil || fi.IsDir() {
			a.repository.DeleteClip(clipID, environmentID)
			writeError(w, http.StatusGone, "文件已丢失")
			return
		}

		clip, reached, err := a.repository.IncrementDownloads(clipID, environmentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if clip == nil {
			writeError(w, http.StatusNotFound, "片段未找到")
			return
		}

		sf := clip.StoredFile
		if sf == nil {
			writeError(w, http.StatusGone, "文件已丢失")
			return
		}

		w.Header().Set("Content-Type", sf.Mime)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sf.Name))
		// Serve the file
		http.ServeFile(w, r, sf.Path)
		// Delete after serving if limit reached
		if reached {
			go a.repository.DeleteClip(clipID, environmentID)
		}
		return
	}

	writeError(w, http.StatusNotFound, "not found")
}

func (a *App) setupRoutes() http.Handler {
	mux := http.NewServeMux()

	// Serve static assets if present
	assetsDir := filepath.Join(a.cfg.StaticRoot, "assets")
	if fi, err := os.Stat(assetsDir); err == nil && fi.IsDir() {
		mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(assetsDir))))
	}
	staticDir := a.cfg.StaticRoot
	if fi, err := os.Stat(staticDir); err == nil && fi.IsDir() {
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))
	}

	// Root handler dispatches all requests manually to avoid pattern conflicts.
	rootHandler := func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Healthcheck
		if path == "/healthz" && r.Method == http.MethodGet {
			a.handleHealthz(w, r)
			return
		}

		// Index
		if path == "/" && r.Method == http.MethodGet {
			a.handleIndex(w, r)
			return
		}

		// API routes
		if path == "/api/clips" {
			switch r.Method {
			case http.MethodGet:
				a.handleListClips(w, r)
			case http.MethodPost:
				a.handleCreateClip(w, r)
			default:
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
			return
		}
		if strings.HasPrefix(path, "/api/clips/") {
			a.handleClipSubroutes(w, r)
			return
		}
		if path == "/api/tokens/register" && r.Method == http.MethodPost {
			a.handleRegisterToken(w, r)
			return
		}
		if path == "/api/config" && r.Method == http.MethodGet {
			a.handleGetConfig(w, r)
			return
		}

		// Direct access routes: /{access_code}/raw (GET) or /{access_code} (GET)
		// Must be registered AFTER all other known paths.
		if r.Method == http.MethodGet {
			trimmed := strings.TrimPrefix(path, "/")
			trimmed = strings.TrimSuffix(trimmed, "/")
			if strings.HasSuffix(trimmed, "/raw") {
				code := strings.TrimSuffix(trimmed, "/raw")
				if code != "" && !strings.Contains(code, "/") {
					// Need to set access_code in a way resolveCode can read; but resolveCode uses PathValue, so we'll replicate here
					// Call a helper that takes the code directly.
					a.resolveCodeWithCode(w, r, code, true)
					return
				}
			}
			if trimmed != "" && !strings.Contains(trimmed, "/") {
				a.resolveCodeWithCode(w, r, trimmed, false)
				return
			}
		}

		writeError(w, http.StatusNotFound, "not found")
	}

	mux.HandleFunc("/", rootHandler)

	var handler http.Handler = mux
	handler = corsMiddleware(handler)
	handler = recoveryMiddleware(handler)
	handler = loggingMiddleware(handler)
	return handler
}

// resolveCodeWithCode resolves a clip by access code/token and dispatches response.
func (a *App) resolveCodeWithCode(w http.ResponseWriter, r *http.Request, code string, raw bool) {
	clip, err := a.getActiveClip(code)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			writeError(w, httpErr.Status, httpErr.Detail)
		} else {
			writeError(w, http.StatusNotFound, "直链不存在或已过期")
		}
		return
	}
	clip2, reached, err := a.repository.IncrementDownloads(clip.ID, clip.EnvironmentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if clip2 == nil {
		writeError(w, http.StatusNotFound, "直链不存在或已过期")
		return
	}
	a.dispatchClipResponse(w, r, clip2, reached, raw)
}

func main() {
	cfg := LoadConfig()

	app, err := NewApp(cfg)
	if err != nil {
		log.Fatalf("failed to initialize app: %v", err)
	}
	defer app.repository.Close()

	// Run initial purge
	if _, err := app.repository.PurgeInactive(); err != nil {
		log.Printf("initial purge error: %v", err)
	}

	// Start background cleanup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.runCleanupWorker(ctx)

	addr := fmt.Sprintf("%s:%d", cfg.AppHost, cfg.AppPort)
	log.Printf("Super Clipboard Go backend listening on %s", addr)

	srv := &http.Server{
		Addr:    addr,
		Handler: app.setupRoutes(),
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}
