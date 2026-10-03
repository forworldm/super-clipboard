package schemas

import (
	"bytes"
	"strings"
)

// UploadInitRequest mirrors POST /api/uploads/init.
// Captcha is verified at init (BEFORE any chunk consumes disk), closing the
// storage-DoS where an attacker fills all chunks and only then trips captcha.
// RequestID is a client idempotency key for safe init retries.
type UploadInitRequest struct {
	Filename        string
	FileSize        int64
	MimeType        string
	EnvironmentID   string
	RequestID       string
	CaptchaToken    *string
	CaptchaProvider *string
}

// ParseUploadInitRequest validates the init body (422 on invalid).
// fileSize upper bound is enforced by the handler (400) because it needs Settings.
func ParseUploadInitRequest(body []byte) (*UploadInitRequest, *ValidationError) {
	fields, validationError := parseObject(body, []interface{}{"body"})
	if validationError != nil {
		return nil, validationError
	}
	filename, _ := fields.requiredString("filename", 1, 255)
	// fileSize: int >= 0 (upper bound checked in handler for a 400 + i18n message).
	geZero := int64(0)
	fileSize, _ := fields.requiredInt("fileSize", nil, &geZero)
	mimeOpt, _ := fields.optionalString("mimeType", 0, 255)
	envOpt, _ := fields.optionalString("environmentId", 0, 64)
	requestIDOpt, okReq := fields.optionalString("requestId", 4, 128)
	captchaToken, okCaptcha := fields.optionalString("captchaToken", 1, 4096)
	captchaProvider, _ := fields.optionalLiteral("captchaProvider", []string{"turnstile", "recaptcha"})
	if validationError := fields.result(); validationError != nil {
		return nil, validationError
	}
	// Extra guards mirroring @field_validator behaviour.
	trimmedName := strings.TrimSpace(filename)
	if trimmedName == "" {
		fields.fieldError("filename", "文件名不能为空", filename)
		return nil, fields.result()
	}
	requestID := ""
	if okReq && requestIDOpt != nil {
		candidate := strings.TrimSpace(*requestIDOpt)
		if candidate == "" {
			requestID = ""
		} else if !isRequestIDSafe(candidate) {
			fields.fieldError("requestId", "requestId 仅允许字母、数字、-、_、: ", *requestIDOpt)
			return nil, fields.result()
		} else {
			requestID = candidate
		}
	}
	mime := ""
	if mimeOpt != nil {
		mime = strings.TrimSpace(*mimeOpt)
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	env := ""
	if envOpt != nil {
		env = strings.TrimSpace(*envOpt)
	}
	out := &UploadInitRequest{
		Filename: trimmedName, FileSize: fileSize, MimeType: mime,
		EnvironmentID: env, RequestID: requestID, CaptchaProvider: captchaProvider,
	}
	if okCaptcha && captchaToken != nil {
		trimmed := strings.TrimSpace(*captchaToken)
		if trimmed != "" {
			out.CaptchaToken = &trimmed
		}
	}
	return out, nil
}

// isRequestIDSafe permits URL/JSON-safe idempotency keys (UUIDs et al.).
func isRequestIDSafe(value string) bool {
	if value == "" {
		return true
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == ':', r == '.':
		default:
			return false
		}
	}
	return true
}

// UploadInitResponse mirrors the init success body.
// ReceivedChunks is populated on idempotent replays so the client can resume
// without an extra GET (empty for fresh sessions).
type UploadInitResponse struct {
	UploadID       string `json:"uploadId"`
	ChunkSize      int    `json:"chunkSize"`
	TotalChunks    int    `json:"totalChunks"`
	FileSize       int64  `json:"fileSize"`
	Filename       string `json:"filename"`
	MimeType       string `json:"mimeType"`
	ExpiresAt      int64  `json:"expiresAt"`
	Status         string `json:"status"`
	ReceivedChunks []int  `json:"receivedChunks"`
}

// UploadInfoResponse mirrors GET /api/uploads/{id}.
type UploadInfoResponse struct {
	UploadID       string `json:"uploadId"`
	Filename       string `json:"filename"`
	FileSize       int64  `json:"fileSize"`
	MimeType       string `json:"mimeType"`
	ChunkSize      int    `json:"chunkSize"`
	TotalChunks    int    `json:"totalChunks"`
	ReceivedChunks []int  `json:"receivedChunks"`
	ReceivedCount  int    `json:"receivedCount"`
	MissingChunks  []int  `json:"missingChunks"`
	Status         string `json:"status"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
	ExpiresAt      int64  `json:"expiresAt"`
}

// UploadChunkResponse mirrors PUT .../chunks/{index}.
type UploadChunkResponse struct {
	UploadID       string `json:"uploadId"`
	Index          int    `json:"index"`
	ReceivedCount  int    `json:"receivedCount"`
	TotalChunks    int    `json:"totalChunks"`
	ReceivedChunks []int  `json:"receivedChunks"`
	Complete       bool   `json:"complete"`
}

// UploadCompleteClipParams carries optional clip-creation fields for
// POST /api/uploads/{id}/complete. Empty body == file-only assembly.
// Captcha is NOT part of complete anymore: it gates session creation in init
// so storage is never consumed by unverified clients.
type UploadCompleteClipParams struct {
	HasClipFields bool
	EnvironmentID string
	ExpiresAt     int64
	MaxDownloads  *int
	AccessCode    *string
	AccessToken   *string
}

// ParseUploadCompleteRequest parses the complete body.
//   - empty/whitespace/"{}" -> file-only (HasClipFields=false, no error)
//   - otherwise validates clip fields (422 on invalid), mirroring ClipCreateRequest.
//   - captchaToken/captchaProvider keys are accepted but ignored (back-compat
//     with older clients); captcha is verified at init instead.
func ParseUploadCompleteRequest(body []byte) (*UploadCompleteClipParams, *ValidationError) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || string(trimmed) == "{}" || string(trimmed) == "null" {
		return &UploadCompleteClipParams{HasClipFields: false}, nil
	}
	fields, validationError := parseObject(body, []interface{}{"body"})
	if validationError != nil {
		return nil, validationError
	}
	// Detect whether any clip field is present; if none, treat as file-only.
	// (ignored keys like captchaToken do not count as clip fields)
	hasAny := false
	for _, k := range []string{"environmentId", "expiresAt", "maxDownloads", "accessCode", "accessToken"} {
		if _, ok := fields.raw[k]; ok {
			hasAny = true
			break
		}
	}
	if !hasAny {
		return &UploadCompleteClipParams{HasClipFields: false}, nil
	}
	params := &UploadCompleteClipParams{HasClipFields: true}
	greaterThanZero := int64(0)
	expiresAt, _ := fields.requiredInt("expiresAt", &greaterThanZero, nil)
	params.ExpiresAt = expiresAt
	maxDownloads, _ := fields.optionalInt("maxDownloads", &greaterThanZero, nil)
	params.MaxDownloads = maxDownloads
	accessCode, okCode := fields.optionalString("accessCode", 5, 12)
	if okCode && accessCode != nil {
		t := strings.TrimSpace(*accessCode)
		switch {
		case t == "":
			params.AccessCode = nil
		case !isAlphanumeric(t):
			fields.fieldError("accessCode", "直链码需由字母或数字组成", *accessCode)
		default:
			params.AccessCode = &t
		}
	}
	accessToken, okToken := fields.optionalString("accessToken", 7, 0)
	if okToken && accessToken != nil {
		t := strings.TrimSpace(*accessToken)
		params.AccessToken = &t
	}
	envID, _ := fields.requiredString("environmentId", 1, 64)
	params.EnvironmentID = envID
	if validationError := fields.result(); validationError != nil {
		return nil, validationError
	}
	input := decodeInput(bytes.Clone(body))
	if params.AccessToken != nil && *params.AccessToken != "" && strings.TrimSpace(params.EnvironmentID) == "" {
		fields.modelError("持久 Token 校验失败，请重新保存", input)
	}
	if strings.TrimSpace(params.EnvironmentID) == "" {
		fields.modelError("environmentId 缺失", input)
	}
	if validationError := fields.result(); validationError != nil {
		return nil, validationError
	}
	return params, nil
}

// UploadCompleteFileResponse mirrors file-only complete success.
type UploadCompleteFileResponse struct {
	UploadID string `json:"uploadId"`
	Status   string `json:"status"`
	File     struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		Type string `json:"type"`
	} `json:"file"`
	ExpiresAt int64 `json:"expiresAt"`
}
