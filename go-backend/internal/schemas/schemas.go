package schemas

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/pixia1234/super-clipboard/backend/internal/models"
)

// StoredFileInput mirrors the StoredFileInput pydantic model.
type StoredFileInput struct {
	Name    string
	Size    int64
	Type    string
	DataURL string
}

// ClipPayloadInput mirrors ClipPayloadInput.
type ClipPayloadInput struct {
	Text *string
	File *StoredFileInput
}

// ClipCreateRequest mirrors ClipCreateRequest.
type ClipCreateRequest struct {
	Type            string
	ExpiresAt       int64
	MaxDownloads    *int
	AccessCode      *string
	AccessToken     *string
	EnvironmentID   string
	Payload         ClipPayloadInput
	CaptchaToken    *string
	CaptchaProvider *string
}

// HasAccessToken reports whether a (non-empty) persistent token was supplied,
// matching the truthiness check `if body.accessToken:` in main.py.
func (r *ClipCreateRequest) HasAccessToken() bool {
	return r.AccessToken != nil && *r.AccessToken != ""
}

// TextPayload mirrors `_resolve_text_payload`.
func (r *ClipCreateRequest) TextPayload() string {
	if r.Payload.Text == nil {
		return ""
	}
	return *r.Payload.Text
}

// TokenRegisterRequest mirrors TokenRegisterRequest.
type TokenRegisterRequest struct {
	Token         string
	EnvironmentID *string
}

// ParseClipCreateRequest validates the body of POST /api/clips.
func ParseClipCreateRequest(body []byte) (*ClipCreateRequest, *ValidationError) {
	fields, validationError := parseObject(body, []interface{}{"body"})
	if validationError != nil {
		return nil, validationError
	}
	input := decodeInput(body)
	request := &ClipCreateRequest{}

	clipType, _ := fields.literal("type", []string{models.ClipTypeText, models.ClipTypeFile}, true)
	request.Type = clipType

	greaterThanZero := int64(0)
	expiresAt, _ := fields.requiredInt("expiresAt", &greaterThanZero, nil)
	request.ExpiresAt = expiresAt

	maxDownloads, _ := fields.optionalInt("maxDownloads", &greaterThanZero, nil)
	request.MaxDownloads = maxDownloads

	// Every string below is explicitly bounded (see limits.go): accessCode is
	// 5..12 characters, accessToken 7..128 characters AND bytes, environmentId
	// 1..64, captchaToken 1..4096. The persistent token used to be unbounded
	// (max_length was skipped), which let a client persist an arbitrary sized
	// value as a lookup key.
	accessCode, okCode := fields.optionalBound("accessCode", accessCodeBound)
	if okCode && accessCode != nil {
		// @field_validator("accessCode") ensure_access_code
		trimmed := strings.TrimSpace(*accessCode)
		switch {
		case trimmed == "":
			request.AccessCode = nil
		case !isAlphanumeric(trimmed):
			fields.fieldError("accessCode", "直链码需由字母或数字组成", *accessCode)
			okCode = false
		default:
			request.AccessCode = &trimmed
		}
	}

	accessToken, okToken := fields.optionalBound("accessToken", accessTokenBound)
	if okToken && accessToken != nil {
		// @field_validator("accessToken") ensure_access_token
		trimmed := strings.TrimSpace(*accessToken)
		request.AccessToken = &trimmed
	}

	environmentID, _ := fields.requiredBound("environmentId", environmentIDBound)
	request.EnvironmentID = environmentID

	captchaToken, okCaptcha := fields.optionalBound("captchaToken", captchaTokenBound)
	if okCaptcha && captchaToken != nil {
		// @field_validator("captchaToken") trim_captcha_token
		trimmed := strings.TrimSpace(*captchaToken)
		if trimmed == "" {
			request.CaptchaToken = nil
		} else {
			request.CaptchaToken = &trimmed
		}
	}

	captchaProvider, _ := fields.optionalLiteral("captchaProvider", []string{"turnstile", "recaptcha"})
	request.CaptchaProvider = captchaProvider

	payloadFields, okPayload := fields.object("payload", true)
	if okPayload && payloadFields != nil {
		// payload.text is capped at 64 KiB (characters AND UTF-8 bytes). Text
		// used to be unbounded, so the only limit was readBody's 1 MiB: a
		// single clip could store ~1 MiB of inline text that is then echoed by
		// every list response. Oversized text must travel as a file clip
		// (POST /api/uploads/init + PUT .../chunks/{index}).
		text, _ := payloadFields.optionalBound("text", clipTextBound)
		request.Payload.Text = text
		storedFile, _ := parseStoredFileInput(payloadFields)
		request.Payload.File = storedFile
		fields.merge(payloadFields)
	}

	// Field level validation runs before @model_validator(mode="after"), and a
	// failing field prevents the model validator from running (pydantic v2).
	if validationError := fields.result(); validationError != nil {
		return nil, validationError
	}

	// @model_validator(mode="after") validate_payload
	if request.Type == models.ClipTypeText {
		if request.Payload.Text == nil || strings.TrimSpace(*request.Payload.Text) == "" {
			fields.modelError("文本片段需要 text 字段", input)
		}
		if request.Payload.File != nil {
			fields.modelError("文本片段不应包含文件数据", input)
		}
	} else if request.Type == models.ClipTypeFile {
		if request.Payload.File == nil {
			fields.modelError("文件片段需要 file 数据", input)
		}
		if request.Payload.Text != nil {
			fields.modelError("文件片段不应包含文本字段", input)
		}
	}
	if request.HasAccessToken() && strings.TrimSpace(request.EnvironmentID) == "" {
		fields.modelError("持久 Token 校验失败，请重新保存", input)
	}
	if strings.TrimSpace(request.EnvironmentID) == "" {
		fields.modelError("environmentId 缺失", input)
	}
	if validationError := fields.result(); validationError != nil {
		return nil, validationError
	}
	return request, nil
}

// parseStoredFileInput validates the nested `payload.file` object.
func parseStoredFileInput(payload *objectFields) (*StoredFileInput, bool) {
	fileFields, ok := payload.object("file", false)
	if !ok || fileFields == nil {
		return nil, ok
	}
	stored := &StoredFileInput{}

	name, okName := fileFields.requiredBound("name", storedFileNameBound)
	stored.Name = name

	greaterOrEqualZero := int64(0)
	size, okSize := fileFields.requiredInt("size", nil, &greaterOrEqualZero)
	stored.Size = size

	fileType, okType := fileFields.optionalBound("type", mimeTypeBound)
	if okType && fileType != nil {
		stored.Type = *fileType
	}

	// dataUrl is the inline base64 payload: bound it explicitly instead of
	// relying on the route's body cap, and keep the chunked upload endpoint as
	// the documented path for real files.
	dataURL, okData := fileFields.requiredBound("dataUrl", dataURLBound)
	if okData {
		// @field_validator("dataUrl") ensure_data_url
		if !strings.HasPrefix(dataURL, "data:") {
			fileFields.fieldError("dataUrl", "file dataUrl must be a base64 data URI", dataURL)
			okData = false
		} else {
			stored.DataURL = dataURL
		}
	}

	payload.merge(fileFields)
	if !okName || !okSize || !okType || !okData {
		return nil, false
	}
	return stored, true
}

// ParseTokenRegisterRequest validates the body of POST /api/tokens/register.
func ParseTokenRegisterRequest(body []byte) (*TokenRegisterRequest, *ValidationError) {
	fields, validationError := parseObject(body, []interface{}{"body"})
	if validationError != nil {
		return nil, validationError
	}
	// The registered token is the very same persistent token used by
	// POST /api/clips, so it shares its bound (7..128 characters/bytes). It was
	// previously unbounded, which made it a free-form row key.
	token, _ := fields.requiredBound("token", accessTokenBound)
	environmentID, okEnv := fields.optionalBound("environmentId", environmentIDBound)
	if okEnv && environmentID != nil {
		// @field_validator("environmentId") ensure_owner
		trimmed := strings.TrimSpace(*environmentID)
		if trimmed == "" {
			environmentID = nil
		} else {
			environmentID = &trimmed
		}
	}
	if validationError := fields.result(); validationError != nil {
		return nil, validationError
	}
	// @field_validator("token") ensure_token
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		fields.fieldError("token", "持久 Token 无效", token)
	} else {
		token = trimmed
	}
	if validationError := fields.result(); validationError != nil {
		return nil, validationError
	}
	return &TokenRegisterRequest{Token: token, EnvironmentID: environmentID}, nil
}

// StoredFileResponse mirrors StoredFileResponse.
type StoredFileResponse struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Type        string `json:"type"`
	DownloadURL string `json:"downloadUrl"`
}

// ClipPayloadResponse mirrors ClipPayloadResponse.
type ClipPayloadResponse struct {
	Text *string             `json:"text"`
	File *StoredFileResponse `json:"file"`
}

// ClipResponse mirrors ClipResponse.
type ClipResponse struct {
	ID            string              `json:"id"`
	Type          string              `json:"type"`
	CreatedAt     int64               `json:"createdAt"`
	ExpiresAt     int64               `json:"expiresAt"`
	MaxDownloads  int                 `json:"maxDownloads"`
	DownloadCount int                 `json:"downloadCount"`
	AccessCode    *string             `json:"accessCode"`
	AccessToken   *string             `json:"accessToken"`
	Payload       ClipPayloadResponse `json:"payload"`
	DirectURL     *string             `json:"directUrl"`
}

// ClipFromModel mirrors ClipResponse.from_clip.
func ClipFromModel(clip *models.Clip, baseURL string) *ClipResponse {
	response := &ClipResponse{
		ID:            normalizeUUID(clip.ID),
		Type:          clip.Type,
		CreatedAt:     clip.CreatedAt.UnixMilli(),
		ExpiresAt:     clip.ExpiresAt.UnixMilli(),
		MaxDownloads:  clip.MaxDownloads,
		DownloadCount: clip.DownloadCount,
		AccessCode:    clip.AccessCode,
		AccessToken:   clip.AccessToken,
		Payload:       ClipPayloadResponse{Text: clip.Text},
	}
	if clip.StoredFile != nil {
		response.Payload.File = &StoredFileResponse{
			Name: clip.StoredFile.Name,
			Size: clip.StoredFile.Size,
			Type: clip.StoredFile.Mime,
			DownloadURL: fmt.Sprintf("%s/api/clips/%s/file?environmentId=%s",
				baseURL, clip.ID, clip.EnvironmentID),
		}
	}
	if clip.AccessCode != nil && *clip.AccessCode != "" {
		directURL := fmt.Sprintf("%s/%s", baseURL, *clip.AccessCode)
		response.DirectURL = &directURL
	}
	return response
}

func normalizeUUID(value string) string {
	if parsed, err := uuid.Parse(value); err == nil {
		return parsed.String()
	}
	return value
}

// ClipListResponse mirrors ClipListResponse.
type ClipListResponse struct {
	Items []*ClipResponse `json:"items"`
}

// DeleteResponse mirrors DeleteResponse.
type DeleteResponse struct {
	OK bool `json:"ok"`
}

// IncrementResponse mirrors IncrementResponse.
type IncrementResponse struct {
	Clip    *ClipResponse `json:"clip"`
	Removed bool          `json:"removed"`
}

// AppConfigResponse mirrors AppConfigResponse.
// Upload knobs are server-generated; the frontend reads them for display and
// uses the per-session chunkSize from init (never its own guess).
type AppConfigResponse struct {
	CaptchaProvider         *string `json:"captchaProvider"`
	CaptchaSiteKey          *string `json:"captchaSiteKey"`
	MaxFileSizeBytes        int64   `json:"maxFileSizeBytes"`
	UploadChunkSizeBytes    int     `json:"uploadChunkSizeBytes"`
	UploadSessionTTLSeconds int     `json:"uploadSessionTTLSeconds"`
}

// TokenRegisterResponse mirrors TokenRegisterResponse.
type TokenRegisterResponse struct {
	Token         string `json:"token"`
	EnvironmentID string `json:"environmentId"`
	UpdatedAt     int64  `json:"updatedAt"`
	LastUsedAt    *int64 `json:"lastUsedAt"`
	ExpiresAt     int64  `json:"expiresAt"`
}

// IndexResponse mirrors the JSON body returned by GET / when no static bundle
// has been built yet.
type IndexResponse struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

// HealthResponse mirrors GET /healthz.
type HealthResponse struct {
	OK        bool  `json:"ok"`
	Timestamp int64 `json:"timestamp"`
}
