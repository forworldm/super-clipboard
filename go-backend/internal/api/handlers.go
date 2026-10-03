package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
	"github.com/pixia1234/super-clipboard/backend/internal/models"
	"github.com/pixia1234/super-clipboard/backend/internal/repository"
	"github.com/pixia1234/super-clipboard/backend/internal/schemas"
	"github.com/pixia1234/super-clipboard/backend/internal/storage"
	"github.com/pixia1234/super-clipboard/backend/internal/utils"
)

// Files are streamed in chunks, this value only affects regular text clips.
var maxBodyBytes int64 = 1 << 20

// readBody consumes the request body like FastAPI does before validation.
// Bodies larger than maxBodyBytes yield 413 instead of a truncated JSON parse.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	limited := http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	data, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, newHTTPError(http.StatusRequestEntityTooLarge, "请求体过大")
		}
		return nil, err
	}
	return data, nil
}

// requiredQuery mirrors a mandatory FastAPI query parameter such as
// `environmentId: str`: a missing key yields a 422 validation payload.
func (a *App) requiredQuery(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	value, ok := queryValue(r, name)
	if ok {
		return value, true
	}
	writeError(w, &schemas.ValidationError{Details: []schemas.ErrorDetail{{
		Type:  "missing",
		Loc:   []interface{}{"query", name},
		Msg:   "Field required",
		Input: nil,
		URL:   "https://errors.pydantic.dev/2.12/v/missing",
	}}})
	return "", false
}

// ---------------------------------------------------------------------------
// healthz / index
// ---------------------------------------------------------------------------

func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, schemas.HealthResponse{
		OK:        true,
		Timestamp: time.Now().UTC().UnixMilli(),
	})
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	indexFile := a.Settings.StaticIndex()
	if info, err := os.Stat(indexFile); err == nil && !info.IsDir() {
		http.ServeFile(w, r, indexFile)
		return
	}
	writeJSON(w, http.StatusOK, schemas.IndexResponse{Name: "Super Clipboard API", OK: true})
}

// ---------------------------------------------------------------------------
// /api/clips
// ---------------------------------------------------------------------------

func (a *App) handleListClips(w http.ResponseWriter, r *http.Request) {
	environmentID, ok := a.requiredQuery(w, r, "environmentId")
	if !ok {
		return
	}
	if _, err := a.Repo.PurgeInactive(); err != nil {
		a.logger.Printf("ERROR:    purge before listing failed: %v", err)
	}
	baseURL := utils.BuildBaseURL(r)
	normalized := strings.TrimSpace(environmentID)
	if normalized == "" {
		writeError(w, newHTTPError(http.StatusBadRequest, "environmentId 缺失"))
		return
	}
	clips, err := a.Repo.ListClips(normalized)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]*schemas.ClipResponse, 0, len(clips))
	for _, clip := range clips {
		items = append(items, schemas.ClipFromModel(clip, baseURL))
	}
	writeJSON(w, http.StatusOK, schemas.ClipListResponse{Items: items})
}

func (a *App) handleGetClip(w http.ResponseWriter, r *http.Request) {
	clipID := routeParam(r, "clip_id")
	environmentID, ok := a.requiredQuery(w, r, "environmentId")
	if !ok {
		return
	}
	clip, err := a.Repo.GetClip(clipID)
	if err != nil {
		writeError(w, err)
		return
	}
	if clip == nil || clip.EnvironmentID != environmentID {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}
	if !clip.IsActive() {
		if _, err := a.Repo.DeleteClip(clipID, environmentID); err != nil {
			a.logger.Printf("ERROR:    unable to drop inactive clip %s: %v", clipID, err)
		}
		writeError(w, newHTTPError(http.StatusNotFound, "片段已过期或达到下载次数"))
		return
	}
	writeJSON(w, http.StatusOK, schemas.ClipFromModel(clip, utils.BuildBaseURL(r)))
}

func (a *App) handleGetClipByCode(w http.ResponseWriter, r *http.Request) {
	accessCode := routeParam(r, "access_code")
	clip, err := a.Repo.GetClipByCode(accessCode)
	if err != nil {
		writeError(w, err)
		return
	}
	if clip == nil {
		writeError(w, newHTTPError(http.StatusNotFound, "直链不存在或已过期"))
		return
	}
	if !clip.IsActive() {
		if _, err := a.Repo.DeleteClip(clip.ID, clip.EnvironmentID); err != nil {
			a.logger.Printf("ERROR:    unable to drop inactive clip %s: %v", clip.ID, err)
		}
		writeError(w, newHTTPError(http.StatusNotFound, "直链不存在或已过期"))
		return
	}
	writeJSON(w, http.StatusOK, schemas.ClipFromModel(clip, utils.BuildBaseURL(r)))
}

// resolveFile mirrors `_resolve_file` in main.py.
func (a *App) resolveFile(request *schemas.ClipCreateRequest) (*models.StoredFile, error) {
	if request.Payload.File == nil {
		return nil, newHTTPError(http.StatusBadRequest, "文件数据缺失")
	}
	storedFile, err := storage.StoreDataURL(a.Settings.FileStorageDir, request.Payload.File.Name, request.Payload.File.DataURL)
	if err != nil {
		return nil, err
	}
	if storedFile.Size > a.Settings.MaxFileSizeBytes {
		_ = os.Remove(storedFile.Path)
		return nil, newHTTPError(http.StatusBadRequest, "文件体积超过限制")
	}
	return storedFile, nil
}

func (a *App) ensureAccessTokenOwner(token string, environmentID string) error {
	if _, err := a.Repo.EnsureTokenOwner(token, environmentID); err != nil {
		if apperr.IsValueCode(err, apperr.CodeTokenNotRegistered) {
			if _, registerErr := a.Repo.RegisterToken(token, &environmentID); registerErr != nil {
				if apperr.IsValueCode(registerErr, apperr.CodeTokenOccupied) {
					var valueErr *apperr.ValueError
					if errors.As(registerErr, &valueErr) && valueErr != nil {
						return newHTTPError(http.StatusConflict, valueErr.Message)
					}
					return newHTTPError(http.StatusConflict, "持久 Token 已被其他设备占用，请稍后重试")
				}
				return registerErr
			}
			return nil
		}
		var valueErr *apperr.ValueError
		if errors.As(err, &valueErr) {
			return newHTTPError(http.StatusConflict, valueErr.Message)
		}
		return err
	}
	return nil
}

func (a *App) handleCreateClip(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	request, validationError := schemas.ParseClipCreateRequest(body)
	if validationError != nil {
		writeError(w, validationError)
		return
	}

	environmentID := strings.TrimSpace(request.EnvironmentID)
	if environmentID == "" {
		writeError(w, newHTTPError(http.StatusBadRequest, "缺少 environmentId"))
		return
	}

	if a.Settings.CaptchaEnabled() {
		if !a.verifyCaptcha(w, r, request.CaptchaToken, request.CaptchaProvider) {
			return
		}
	}

	if request.HasAccessToken() {
		if err := a.ensureAccessTokenOwner(*request.AccessToken, environmentID); err != nil {
			writeError(w, err)
			return
		}
	}

	var storedFile *models.StoredFile
	clip, err := func() (*models.Clip, error) {
		if request.Type == models.ClipTypeFile {
			resolved, resolveErr := a.resolveFile(request)
			if resolveErr != nil {
				return nil, resolveErr
			}
			storedFile = resolved
		}
		var text *string
		if request.Type == models.ClipTypeText {
			value := request.TextPayload()
			text = &value
		}
		return a.Repo.CreateClip(repository.CreateClipParams{
			ClipType:      request.Type,
			ExpiresAtMs:   request.ExpiresAt,
			MaxDownloads:  request.MaxDownloads,
			AccessCode:    request.AccessCode,
			AccessToken:   request.AccessToken,
			EnvironmentID: environmentID,
			Text:          text,
			StoredFile:    storedFile,
		})
	}()
	if err != nil {
		if storedFile != nil {
			_ = os.Remove(storedFile.Path)
		}
		var valueError *apperr.ValueError
		if errors.As(err, &valueError) {
			status := http.StatusBadRequest
			if valueError.Code == apperr.CodeAccessCodeConflict || valueError.Code == apperr.CodeTokenOccupied {
				status = http.StatusConflict
			}
			writeError(w, newHTTPError(status, valueError.Message))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, schemas.ClipFromModel(clip, utils.BuildBaseURL(r)))
}

func (a *App) handleDeleteClip(w http.ResponseWriter, r *http.Request) {
	clipID := routeParam(r, "clip_id")
	environmentID, ok := a.requiredQuery(w, r, "environmentId")
	if !ok {
		return
	}
	removed, err := a.Repo.DeleteClip(clipID, strings.TrimSpace(environmentID))
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

func (a *App) handleTrackDownload(w http.ResponseWriter, r *http.Request) {
	clipID := routeParam(r, "clip_id")
	environmentID, ok := a.requiredQuery(w, r, "environmentId")
	if !ok {
		return
	}
	normalized := strings.TrimSpace(environmentID)
	clip, reached, err := a.Repo.IncrementDownloads(clipID, normalized)
	if err != nil {
		writeError(w, err)
		return
	}
	if clip == nil {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}
	if !clip.IsActive() && reached {
		if _, err := a.Repo.DeleteClip(clipID, normalized); err != nil {
			a.logger.Printf("ERROR:    unable to drop exhausted clip %s: %v", clipID, err)
		}
		writeError(w, newHTTPError(http.StatusGone, "片段已过期或销毁"))
		return
	}
	writeJSON(w, http.StatusOK, schemas.IncrementResponse{
		Clip:    schemas.ClipFromModel(clip, utils.BuildBaseURL(r)),
		Removed: reached,
	})
}

func (a *App) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	clipID := routeParam(r, "clip_id")
	environmentID, ok := a.requiredQuery(w, r, "environmentId")
	if !ok {
		return
	}
	normalized := strings.TrimSpace(environmentID)
	clip, err := a.Repo.GetClip(clipID)
	if err != nil {
		writeError(w, err)
		return
	}
	if clip == nil {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}
	if clip.EnvironmentID != normalized {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}
	if clip.StoredFile == nil {
		if _, err := a.Repo.DeleteClip(clipID, normalized); err != nil {
			a.logger.Printf("ERROR:    unable to drop clip %s: %v", clipID, err)
		}
		writeError(w, newHTTPError(http.StatusGone, "文件已丢失"))
		return
	}
	if !clip.IsActive() {
		if _, err := a.Repo.DeleteClip(clipID, normalized); err != nil {
			a.logger.Printf("ERROR:    unable to drop clip %s: %v", clipID, err)
		}
		writeError(w, newHTTPError(http.StatusGone, "文件已过期或销毁"))
		return
	}
	filePath := clip.StoredFile.Path
	if !fileExists(filePath) {
		if _, err := a.Repo.DeleteClip(clipID, normalized); err != nil {
			a.logger.Printf("ERROR:    unable to drop clip %s: %v", clipID, err)
		}
		writeError(w, newHTTPError(http.StatusGone, "文件已丢失"))
		return
	}

	updated, reached, err := a.Repo.IncrementDownloads(clipID, normalized)
	if err != nil {
		writeError(w, err)
		return
	}
	if updated == nil {
		writeError(w, newHTTPError(http.StatusNotFound, "片段未找到"))
		return
	}

	mediaType := "application/octet-stream"
	fileName := "clip"
	if updated.StoredFile != nil {
		mediaType = updated.StoredFile.Mime
		fileName = updated.StoredFile.Name
	}
	if err := writeFileResponse(w, r, filePath, mediaType, fileName); err != nil {
		a.logger.Printf("ERROR:    unable to stream %s: %v", filePath, err)
		writeError(w, newHTTPError(http.StatusGone, "文件已丢失"))
		return
	}

	if reached {
		// background.add_task(repository.delete_clip, ...)
		go func() {
			if _, err := a.Repo.DeleteClip(clipID, normalized); err != nil {
				a.logger.Printf("ERROR:    background delete failed for %s: %v", clipID, err)
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// /api/tokens/register and /api/config
// ---------------------------------------------------------------------------

func (a *App) handleRegisterToken(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	request, validationError := schemas.ParseTokenRegisterRequest(body)
	if validationError != nil {
		writeError(w, validationError)
		return
	}
	record, err := a.Repo.RegisterToken(request.Token, request.EnvironmentID)
	if err != nil {
		var valueError *apperr.ValueError
		if errors.As(err, &valueError) {
			writeError(w, newHTTPError(http.StatusConflict, valueError.Message))
			return
		}
		writeError(w, err)
		return
	}
	var lastUsedAt *int64
	if record.LastUsedAt != nil && *record.LastUsedAt != 0 {
		value := *record.LastUsedAt * 1000
		lastUsedAt = &value
	}
	writeJSON(w, http.StatusOK, schemas.TokenRegisterResponse{
		Token:         record.Token,
		EnvironmentID: record.EnvironmentID,
		UpdatedAt:     record.UpdatedAt * 1000,
		LastUsedAt:    lastUsedAt,
		ExpiresAt:     record.ExpiresAt * 1000,
	})
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	response := schemas.AppConfigResponse{
		MaxFileSizeBytes:        a.Settings.MaxFileSizeBytes,
		UploadChunkSizeBytes:    a.Settings.EffectiveChunkSize(),
		UploadSessionTTLSeconds: a.Settings.EffectiveUploadTTL(),
	}
	if a.Settings.CaptchaProvider != "" {
		provider := a.Settings.CaptchaProvider
		response.CaptchaProvider = &provider
	}
	if a.Settings.CaptchaSiteKey != "" {
		siteKey := a.Settings.CaptchaSiteKey
		response.CaptchaSiteKey = &siteKey
	}
	writeJSON(w, http.StatusOK, response)
}

// ---------------------------------------------------------------------------
// /{access_code} and /{access_code}/raw
// ---------------------------------------------------------------------------

func (a *App) handleResolveRaw(w http.ResponseWriter, r *http.Request) {
	a.resolveDirectLink(w, r, routeParam(r, "access_code"), true)
}

func (a *App) handleResolve(w http.ResponseWriter, r *http.Request) {
	a.resolveDirectLink(w, r, routeParam(r, "access_code"), false)
}

func (a *App) resolveDirectLink(w http.ResponseWriter, r *http.Request, identifier string, raw bool) {
	clip, err := a.getActiveClip(identifier)
	if err != nil {
		writeError(w, err)
		return
	}
	updated, reached, err := a.incrementClipDownloads(clip)
	if err != nil {
		writeError(w, err)
		return
	}
	a.dispatchClipResponse(w, r, updated, reached, raw)
}

// getActiveClip mirrors `_get_active_clip`.
func (a *App) getActiveClip(identifier string) (*models.Clip, error) {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return nil, newHTTPError(http.StatusNotFound, "直链不存在或已过期")
	}
	clip, err := a.Repo.GetClipByCode(trimmed)
	if err != nil {
		return nil, err
	}
	ownerHint := ""
	remainder := trimmed
	if clip == nil {
		ownerHint, remainder = parseIdentifier(trimmed)
		if ownerHint != "" && remainder != "" {
			if isDigitString(remainder) && utf8.RuneCountInString(remainder) == 5 {
				if clip, err = a.Repo.GetClipByCodeAndOwner(remainder, ownerHint); err != nil {
					return nil, err
				}
			}
			if clip == nil {
				if clip, err = a.Repo.GetClipByToken(remainder, ownerHint); err != nil {
					return nil, err
				}
			}
		}
	}
	if clip == nil {
		if clip, err = a.Repo.GetClipByToken(trimmed, ""); err != nil {
			return nil, err
		}
	}
	if clip == nil {
		return nil, newHTTPError(http.StatusNotFound, "直链不存在或已过期")
	}
	if !clip.IsActive() {
		if _, err := a.Repo.DeleteClip(clip.ID, clip.EnvironmentID); err != nil {
			a.logger.Printf("ERROR:    unable to drop inactive clip %s: %v", clip.ID, err)
		}
		return nil, newHTTPError(http.StatusNotFound, "直链不存在或已过期")
	}
	return clip, nil
}

// incrementClipDownloads mirrors `_increment_clip_downloads`.
func (a *App) incrementClipDownloads(clip *models.Clip) (*models.Clip, bool, error) {
	updated, reached, err := a.Repo.IncrementDownloads(clip.ID, clip.EnvironmentID)
	if err != nil {
		return nil, false, err
	}
	if updated == nil {
		return nil, false, newHTTPError(http.StatusNotFound, "直链不存在或已过期")
	}
	return updated, reached, nil
}

// dispatchClipResponse mirrors `_dispatch_clip_response`.
func (a *App) dispatchClipResponse(w http.ResponseWriter, r *http.Request, clip *models.Clip, reached bool, raw bool) {
	switch {
	case clip.Type == models.ClipTypeText:
		if raw {
			writePlainText(w, http.StatusOK, clip.TextValue())
		} else {
			writeHTML(w, http.StatusOK, utils.BuildTextClipHTML(clip.TextValue(), clip.CreatedAt, clip.DownloadCount, clip.AccessCode))
		}
		if reached {
			a.scheduleDelete(clip.ID, clip.EnvironmentID)
		}
	case clip.StoredFile != nil:
		filePath := clip.StoredFile.Path
		if !fileExists(filePath) {
			if _, err := a.Repo.DeleteClip(clip.ID, clip.EnvironmentID); err != nil {
				a.logger.Printf("ERROR:    unable to drop clip %s: %v", clip.ID, err)
			}
			writeError(w, newHTTPError(http.StatusGone, "文件已丢失"))
			return
		}
		if err := writeFileResponse(w, r, filePath, clip.StoredFile.Mime, clip.StoredFile.Name); err != nil {
			a.logger.Printf("ERROR:    unable to stream %s: %v", filePath, err)
			writeError(w, newHTTPError(http.StatusGone, "文件已丢失"))
			return
		}

		if reached {
			a.scheduleDelete(clip.ID, clip.EnvironmentID)
		}
	default:
		if _, err := a.Repo.DeleteClip(clip.ID, clip.EnvironmentID); err != nil {
			a.logger.Printf("ERROR:    unable to drop clip %s: %v", clip.ID, err)
		}
		writeError(w, newHTTPError(http.StatusGone, "文件数据缺失"))
	}
}

// scheduleDelete mirrors `background.add_task(repository.delete_clip, ...)`.
func (a *App) scheduleDelete(clipID string, environmentID string) {
	go func() {
		if _, err := a.Repo.DeleteClip(clipID, environmentID); err != nil {
			a.logger.Printf("ERROR:    background delete failed for %s: %v", clipID, err)
		}
	}()
}

// parseIdentifier mirrors `_parse_identifier`.
func parseIdentifier(identifier string) (string, string) {
	if index := strings.Index(identifier, "."); index >= 0 {
		return strings.TrimSpace(identifier[:index]), strings.TrimSpace(identifier[index+1:])
	}
	return "", strings.TrimSpace(identifier)
}

// isDigitString mirrors Python's `str.isdigit()`.
func isDigitString(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
