package schemas

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/models"
)

func parse(t *testing.T, body string) (*ClipCreateRequest, *ValidationError) {
	t.Helper()
	return ParseClipCreateRequest([]byte(body))
}

func mustParse(t *testing.T, body string) *ClipCreateRequest {
	t.Helper()
	request, validationError := parse(t, body)
	if validationError != nil {
		t.Fatalf("unexpected validation error: %s", validationError.Error())
	}
	return request
}

// TestParseTextClip covers a valid text payload.
func TestParseTextClip(t *testing.T) {
	request := mustParse(t, `{
		"type": "text",
		"expiresAt": 1893456000000,
		"maxDownloads": 3,
		"accessCode": " 54321 ",
		"environmentId": "env-1",
		"payload": {"text": "hello"}
	}`)

	if request.Type != "text" || request.ExpiresAt != 1893456000000 {
		t.Fatalf("unexpected request %+v", request)
	}
	if request.MaxDownloads == nil || *request.MaxDownloads != 3 {
		t.Fatalf("unexpected maxDownloads %v", request.MaxDownloads)
	}
	if request.AccessCode == nil || *request.AccessCode != "54321" {
		t.Fatalf("accessCode should be trimmed, got %v", request.AccessCode)
	}
	if request.Payload.Text == nil || *request.Payload.Text != "hello" {
		t.Fatalf("unexpected text payload %v", request.Payload.Text)
	}
	if request.Payload.File != nil {
		t.Fatalf("unexpected file payload %v", request.Payload.File)
	}
	if request.AccessToken != nil || request.CaptchaToken != nil || request.CaptchaProvider != nil {
		t.Fatalf("optional fields should stay nil: %+v", request)
	}
}

// TestParseFileClip covers a valid file payload.
func TestParseFileClip(t *testing.T) {
	request := mustParse(t, `{
		"type": "file",
		"expiresAt": 1893456000000,
		"accessToken": "  persist-token  ",
		"environmentId": "env-1",
		"captchaToken": "  captcha  ",
		"captchaProvider": "recaptcha",
		"payload": {"file": {"name": "a.txt", "size": 3, "type": "text/plain", "dataUrl": "data:text/plain;base64,YWJj"}}
	}`)

	if request.Type != "file" {
		t.Fatalf("unexpected type %q", request.Type)
	}
	if request.AccessToken == nil || *request.AccessToken != "persist-token" {
		t.Fatalf("accessToken should be trimmed, got %v", request.AccessToken)
	}
	if !request.HasAccessToken() {
		t.Fatalf("HasAccessToken should be true")
	}
	if request.CaptchaToken == nil || *request.CaptchaToken != "captcha" {
		t.Fatalf("captchaToken should be trimmed, got %v", request.CaptchaToken)
	}
	if request.CaptchaProvider == nil || *request.CaptchaProvider != "recaptcha" {
		t.Fatalf("unexpected captchaProvider %v", request.CaptchaProvider)
	}
	if request.MaxDownloads != nil {
		t.Fatalf("maxDownloads should default to nil, got %v", *request.MaxDownloads)
	}
	file := request.Payload.File
	if file == nil {
		t.Fatalf("expected a file payload")
	}
	if file.Name != "a.txt" || file.Size != 3 || file.Type != "text/plain" {
		t.Fatalf("unexpected file metadata %+v", file)
	}
	if file.DataURL != "data:text/plain;base64,YWJj" {
		t.Fatalf("unexpected data url %q", file.DataURL)
	}
}

// TestParseFileClipDefaults covers optional members of StoredFileInput.
func TestParseFileClipDefaults(t *testing.T) {
	request := mustParse(t, `{
		"type": "file",
		"expiresAt": 1893456000000,
		"environmentId": "env-1",
		"payload": {"file": {"name": "a.bin", "size": 0, "dataUrl": "data:application/octet-stream;base64,"}}
	}`)
	if request.Payload.File.Type != "" {
		t.Fatalf("type should default to an empty string, got %q", request.Payload.File.Type)
	}
}

// TestParseValidationErrors covers the pydantic compatible error payloads.
func TestParseValidationErrors(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantMsg    string
		wantLoc    string
		wantStatus string
	}{
		{
			name:    "unknown literal",
			body:    `{"type":"image","expiresAt":1,"environmentId":"e","payload":{"text":"x"}}`,
			wantMsg: "Input should be 'text' or 'file'",
			wantLoc: `["body","type"]`,
		},
		{
			name:    "missing expiresAt",
			body:    `{"type":"text","environmentId":"e","payload":{"text":"x"}}`,
			wantMsg: "Field required",
			wantLoc: `["body","expiresAt"]`,
		},
		{
			name:    "expiresAt must be positive",
			body:    `{"type":"text","expiresAt":0,"environmentId":"e","payload":{"text":"x"}}`,
			wantMsg: "Input should be greater than 0",
			wantLoc: `["body","expiresAt"]`,
		},
		{
			name:    "short access code",
			body:    `{"type":"text","expiresAt":1,"accessCode":"12","environmentId":"e","payload":{"text":"x"}}`,
			wantMsg: "String should have at least 5 characters",
			wantLoc: `["body","accessCode"]`,
		},
		{
			name:    "long access code",
			body:    `{"type":"text","expiresAt":1,"accessCode":"1234567890123","environmentId":"e","payload":{"text":"x"}}`,
			wantMsg: "String should have at most 12 characters",
			wantLoc: `["body","accessCode"]`,
		},
		{
			name:    "symbolic access code",
			body:    `{"type":"text","expiresAt":1,"accessCode":"ab!cd","environmentId":"e","payload":{"text":"x"}}`,
			wantMsg: "Value error, 直链码需由字母或数字组成",
			wantLoc: `["body","accessCode"]`,
		},
		{
			name:    "short token",
			body:    `{"type":"text","expiresAt":1,"accessToken":"abc","environmentId":"e","payload":{"text":"x"}}`,
			wantMsg: "String should have at least 7 characters",
			wantLoc: `["body","accessToken"]`,
		},
		{
			name:    "missing environmentId",
			body:    `{"type":"text","expiresAt":1,"payload":{"text":"x"}}`,
			wantMsg: "Field required",
			wantLoc: `["body","environmentId"]`,
		},
		{
			name:    "blank text payload",
			body:    `{"type":"text","expiresAt":1,"environmentId":"e","payload":{"text":"   "}}`,
			wantMsg: "Value error, 文本片段需要 text 字段",
			wantLoc: `["body"]`,
		},
		{
			name:    "file payload on a text clip",
			body:    `{"type":"text","expiresAt":1,"environmentId":"e","payload":{"text":"x","file":{"name":"a","size":1,"dataUrl":"data:x;base64,"}}}`,
			wantMsg: "Value error, 文本片段不应包含文件数据",
			wantLoc: `["body"]`,
		},
		{
			name:    "text payload on a file clip",
			body:    `{"type":"file","expiresAt":1,"environmentId":"e","payload":{"text":"x","file":{"name":"a","size":1,"dataUrl":"data:x;base64,"}}}`,
			wantMsg: "Value error, 文件片段不应包含文本字段",
			wantLoc: `["body"]`,
		},
		{
			name:    "missing file payload",
			body:    `{"type":"file","expiresAt":1,"environmentId":"e","payload":{}}`,
			wantMsg: "Value error, 文件片段需要 file 数据",
			wantLoc: `["body"]`,
		},
		{
			name:    "invalid data url",
			body:    `{"type":"file","expiresAt":1,"environmentId":"e","payload":{"file":{"name":"a","size":1,"dataUrl":"http://x/y"}}}`,
			wantMsg: "Value error, file dataUrl must be a base64 data URI",
			wantLoc: `["body","payload","file","dataUrl"]`,
		},
		{
			name:    "negative size",
			body:    `{"type":"file","expiresAt":1,"environmentId":"e","payload":{"file":{"name":"a","size":-1,"dataUrl":"data:x;base64,"}}}`,
			wantMsg: "Input should be greater than or equal to 0",
			wantLoc: `["body","payload","file","size"]`,
		},
		{
			name:    "unknown captcha provider",
			body:    `{"type":"text","expiresAt":1,"environmentId":"e","captchaProvider":"hcaptcha","payload":{"text":"x"}}`,
			wantMsg: "Input should be 'turnstile' or 'recaptcha'",
			wantLoc: `["body","captchaProvider"]`,
		},
		{
			name:    "wrong json type",
			body:    `{"type":"text","expiresAt":"abc","environmentId":"e","payload":{"text":"x"}}`,
			wantMsg: "Input should be a valid integer, unable to parse string as an integer",
			wantLoc: `["body","expiresAt"]`,
		},
		{
			name:    "fractional integer",
			body:    `{"type":"text","expiresAt":1.5,"environmentId":"e","payload":{"text":"x"}}`,
			wantMsg: "Input should be a valid integer, got a number with a fractional part",
			wantLoc: `["body","expiresAt"]`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, validationError := parse(t, testCase.body)
			if validationError == nil {
				t.Fatalf("expected a validation error for %s", testCase.body)
			}
			encoded, err := json.Marshal(validationError.Details)
			if err != nil {
				t.Fatalf("unable to encode details: %v", err)
			}
			if !strings.Contains(string(encoded), testCase.wantMsg) {
				t.Fatalf("expected message %q in %s", testCase.wantMsg, encoded)
			}
			if !strings.Contains(string(encoded), testCase.wantLoc) {
				t.Fatalf("expected loc %s in %s", testCase.wantLoc, encoded)
			}
		})
	}
}

// TestParseMalformedBodies covers the json_invalid branch.
func TestParseMalformedBodies(t *testing.T) {
	for _, body := range []string{"", "   ", "{not json"} {
		_, validationError := parse(t, body)
		if validationError == nil {
			t.Fatalf("expected a validation error for %q", body)
		}
		if validationError.Details[0].Type != "json_invalid" {
			t.Fatalf("expected json_invalid, got %q", validationError.Details[0].Type)
		}
	}
}

// TestParseUnicodeAccessCode keeps Python's unicode aware isalnum behaviour.
func TestParseUnicodeAccessCode(t *testing.T) {
	request := mustParse(t, `{"type":"text","expiresAt":1,"accessCode":"剪贴板12","environmentId":"e","payload":{"text":"x"}}`)
	if request.AccessCode == nil || *request.AccessCode != "剪贴板12" {
		t.Fatalf("unicode alphanumerics should be accepted, got %v", request.AccessCode)
	}
}

// TestParseBlankOptionalValues mirrors the trim validators returning None.
func TestParseBlankOptionalValues(t *testing.T) {
	request := mustParse(t, `{
		"type": "text",
		"expiresAt": 1893456000000,
		"accessCode": "     ",
		"captchaToken": "   ",
		"environmentId": "env-1",
		"payload": {"text": "x"}
	}`)
	if request.AccessCode != nil {
		t.Fatalf("a blank access code should become nil, got %q", *request.AccessCode)
	}
	if request.CaptchaToken != nil {
		t.Fatalf("a blank captcha token should become nil, got %q", *request.CaptchaToken)
	}
}

// TestParseTokenRegisterRequest covers TokenRegisterRequest.
func TestParseTokenRegisterRequest(t *testing.T) {
	request, validationError := ParseTokenRegisterRequest([]byte(`{"token":"  pixia1234  "}`))
	if validationError != nil {
		t.Fatalf("unexpected validation error: %s", validationError.Error())
	}
	if request.Token != "pixia1234" {
		t.Fatalf("token should be trimmed, got %q", request.Token)
	}
	if request.EnvironmentID != nil {
		t.Fatalf("environmentId should be nil, got %q", *request.EnvironmentID)
	}

	request, validationError = ParseTokenRegisterRequest([]byte(`{"token":"pixia1234","environmentId":"  env-9  "}`))
	if validationError != nil {
		t.Fatalf("unexpected validation error: %s", validationError.Error())
	}
	if request.EnvironmentID == nil || *request.EnvironmentID != "env-9" {
		t.Fatalf("environmentId should be trimmed, got %v", request.EnvironmentID)
	}

	request, validationError = ParseTokenRegisterRequest([]byte(`{"token":"pixia1234","environmentId":"   "}`))
	if validationError != nil {
		t.Fatalf("unexpected validation error: %s", validationError.Error())
	}
	if request.EnvironmentID != nil {
		t.Fatalf("a blank environmentId should become nil, got %q", *request.EnvironmentID)
	}

	if _, validationError = ParseTokenRegisterRequest([]byte(`{"token":"short"}`)); validationError == nil {
		t.Fatalf("expected a length validation error")
	}
	if _, validationError = ParseTokenRegisterRequest([]byte(`{}`)); validationError == nil {
		t.Fatalf("expected a missing field error")
	}
}

// TestClipFromModel covers ClipResponse.from_clip.
func TestClipFromModel(t *testing.T) {
	code := "54321"
	token := "persist-token"
	text := "content"
	createdAt := time.Unix(1700000000, 0).UTC()
	expiresAt := time.Unix(1700003600, 0).UTC()

	clip := &models.Clip{
		ID:            "6f9d0e6a-1b2c-4d3e-8f90-1234567890ab",
		Type:          models.ClipTypeText,
		CreatedAt:     createdAt,
		ExpiresAt:     expiresAt,
		MaxDownloads:  5,
		DownloadCount: 2,
		AccessCode:    &code,
		AccessToken:   &token,
		EnvironmentID: "env-1",
		Text:          &text,
	}

	response := ClipFromModel(clip, "http://example.com")
	if response.ID != clip.ID {
		t.Fatalf("unexpected id %q", response.ID)
	}
	if response.CreatedAt != 1700000000000 || response.ExpiresAt != 1700003600000 {
		t.Fatalf("timestamps should be milliseconds: %+v", response)
	}
	if response.MaxDownloads != 5 || response.DownloadCount != 2 {
		t.Fatalf("unexpected counters %+v", response)
	}
	if response.DirectURL == nil || *response.DirectURL != "http://example.com/54321" {
		t.Fatalf("unexpected directUrl %v", response.DirectURL)
	}
	if response.Payload.File != nil {
		t.Fatalf("text clips should not expose a file payload")
	}

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("unable to encode response: %v", err)
	}
	for _, expected := range []string{
		`"createdAt":1700000000000`,
		`"accessCode":"54321"`,
		`"accessToken":"persist-token"`,
		`"directUrl":"http://example.com/54321"`,
		`"payload":{"text":"content","file":null}`,
	} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("expected %s in %s", expected, encoded)
		}
	}

	// File clips expose the download URL and no direct URL without a code.
	fileClip := &models.Clip{
		ID:            "6f9d0e6a-1b2c-4d3e-8f90-1234567890ab",
		Type:          models.ClipTypeFile,
		CreatedAt:     createdAt,
		ExpiresAt:     expiresAt,
		MaxDownloads:  1,
		EnvironmentID: "env-2",
		StoredFile:    &models.StoredFile{Name: "report.pdf", Size: 2048, Mime: "application/pdf", Path: "/tmp/report.pdf"},
	}
	fileResponse := ClipFromModel(fileClip, "https://clip.example.org")
	if fileResponse.DirectURL != nil {
		t.Fatalf("directUrl should be null without an access code, got %v", *fileResponse.DirectURL)
	}
	if fileResponse.AccessCode != nil || fileResponse.AccessToken != nil {
		t.Fatalf("credentials should stay null")
	}
	if fileResponse.Payload.Text != nil {
		t.Fatalf("file clips should not expose text")
	}
	expectedURL := "https://clip.example.org/api/clips/6f9d0e6a-1b2c-4d3e-8f90-1234567890ab/file?environmentId=env-2"
	if fileResponse.Payload.File == nil || fileResponse.Payload.File.DownloadURL != expectedURL {
		t.Fatalf("unexpected download url %+v", fileResponse.Payload.File)
	}
	if fileResponse.Payload.File.Name != "report.pdf" || fileResponse.Payload.File.Size != 2048 || fileResponse.Payload.File.Type != "application/pdf" {
		t.Fatalf("unexpected file payload %+v", fileResponse.Payload.File)
	}
}

// TestClipModelHelpers covers the small model accessors used by the handlers.
func TestClipModelHelpers(t *testing.T) {
	clip := &models.Clip{
		ExpiresAt:     time.Now().UTC().Add(time.Hour),
		MaxDownloads:  2,
		DownloadCount: 0,
	}
	if !clip.IsActive() || clip.IsExpired() || clip.ReachedDownloadLimit() {
		t.Fatalf("clip should be active: %+v", clip)
	}
	if clip.TextValue() != "" || clip.AccessCodeValue() != "" {
		t.Fatalf("accessors should return empty strings for nil pointers")
	}

	clip.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	if !clip.IsExpired() || clip.IsActive() {
		t.Fatalf("clip should be expired: %+v", clip)
	}

	clip.ExpiresAt = time.Now().UTC().Add(time.Hour)
	clip.DownloadCount = 2
	if !clip.ReachedDownloadLimit() || clip.IsActive() {
		t.Fatalf("clip should have reached its limit: %+v", clip)
	}
}
