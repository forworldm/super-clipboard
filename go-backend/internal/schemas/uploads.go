package schemas

import (
	"strings"
)

// UploadInitRequest mirrors POST /api/uploads/init.
//
// There is no such thing as a "regular file upload": every chunked upload
// becomes a file clip, so the clip parameters arrive with the init request and
// are validated here -- BEFORE a session row or a single chunk byte exists.
// They are then frozen on the session row and reused by /complete, which never
// re-accepts them (a client cannot fix a rejected parameter afterwards).
//
// Captcha is verified at init too (BEFORE any chunk consumes disk), closing the
// storage-DoS where an attacker fills all chunks and only then trips captcha.
// RequestID is a client idempotency key for safe init retries.
type UploadInitRequest struct {
	Filename string
	FileSize int64
	MimeType string
	// EnvironmentID is mandatory. Without an accessCode/accessToken the clip is
	// a nameless clip: only its environment owner can ever list/open it, so a
	// clip without an owner would be unreachable garbage.
	EnvironmentID   string
	RequestID       string
	CaptchaToken    *string
	CaptchaProvider *string
	// Clip fields, validated like ClipCreateRequest. ExpiresAt is mandatory for
	// the same reason: clips.expires_at is NOT NULL and must be in the future.
	ExpiresAt    int64
	MaxDownloads *int
	AccessCode   *string
	AccessToken  *string
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
	// environmentId is mandatory for every upload (see UploadInitRequest).
	env, _ := fields.requiredString("environmentId", 1, 64)
	requestIDOpt, okReq := fields.optionalString("requestId", 4, 128)
	captchaToken, okCaptcha := fields.optionalString("captchaToken", 1, 4096)
	captchaProvider, _ := fields.optionalLiteral("captchaProvider", []string{"turnstile", "recaptcha"})

	// Clip params: expiresAt is required (a clip without a future expiry can
	// never be created), maxDownloads/accessCode/accessToken stay optional.
	greaterThanZero := int64(0)
	expiresAt, _ := fields.requiredInt("expiresAt", &greaterThanZero, nil)
	maxDownloads, _ := fields.optionalInt("maxDownloads", &greaterThanZero, nil)
	var accessCode *string
	code, okCode := fields.optionalString("accessCode", 5, 12)
	if okCode && code != nil {
		t := strings.TrimSpace(*code)
		switch {
		case t == "":
			accessCode = nil
		case !isAlphanumeric(t):
			fields.fieldError("accessCode", "直链码需由字母或数字组成", *code)
		default:
			accessCode = &t
		}
	}
	var accessToken *string
	token, okToken := fields.optionalString("accessToken", 7, 0)
	if okToken && token != nil {
		t := strings.TrimSpace(*token)
		accessToken = &t
	}
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
	out := &UploadInitRequest{
		Filename: trimmedName, FileSize: fileSize, MimeType: mime,
		EnvironmentID: strings.TrimSpace(env), RequestID: requestID,
		CaptchaProvider: captchaProvider,
		ExpiresAt:       expiresAt, MaxDownloads: maxDownloads,
		AccessCode: accessCode, AccessToken: accessToken,
	}
	if okCaptcha && captchaToken != nil {
		trimmed := strings.TrimSpace(*captchaToken)
		if trimmed != "" {
			out.CaptchaToken = &trimmed
		}
	}
	// @model_validator(mode="after") mirrors ClipCreateRequest: a blank
	// environmentId is as invalid as a missing one.
	input := decodeInput(body)
	if out.AccessToken != nil && *out.AccessToken != "" && strings.TrimSpace(out.EnvironmentID) == "" {
		fields.modelError("持久 Token 校验失败，请重新保存", input)
	}
	if strings.TrimSpace(out.EnvironmentID) == "" {
		fields.modelError("environmentId 缺失", input)
	}
	if validationError := fields.result(); validationError != nil {
		return nil, validationError
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

// NOTE: POST /api/uploads/{id}/complete has no request model on purpose. Every
// upload ends as a file clip, and the clip parameters were captured (and
// validated) by UploadInitRequest at init, so complete only replays the values
// stored on the session row. Its success body is a ClipResponse.
