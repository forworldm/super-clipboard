package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
	"github.com/pixia1234/super-clipboard/backend/internal/schemas"
)

// newHTTPError is a shortcut for apperr.NewHTTPError (FastAPI's HTTPException).
func newHTTPError(status int, detail string) *apperr.HTTPError {
	return apperr.NewHTTPError(status, detail)
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	// FastAPI/Starlette use Python json.dumps, which does not escape &, <, >.
	// encoding/json defaults to \u003c-style escapes; disable that so the wire
	// format stays compatible with the original API.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// json.Encoder terminates every value with a newline; FastAPI does not.
	body := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeJSONResponse mirrors fastapi.responses.JSONResponse.
func writeJSONResponse(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]interface{}{"detail": detail})
}

// writePlainText mirrors fastapi.responses.PlainTextResponse.
func writePlainText(w http.ResponseWriter, status int, content string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(content))
}

// writeHTML mirrors fastapi.responses.HTMLResponse.
func writeHTML(w http.ResponseWriter, status int, content string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(content))
}

// writeError renders an error exactly like FastAPI's exception handlers:
//   - pydantic ValidationError      -> 422 {"detail": [...]}
//   - HTTPException                 -> status {"detail": "..."}
//   - ValueError escaping a handler -> 400 {"detail": "..."}
//   - anything else                 -> 500 "Internal Server Error" (text/plain)
func writeError(w http.ResponseWriter, err error) {
	var validationError *schemas.ValidationError
	if errors.As(err, &validationError) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{"detail": validationError.Details})
		return
	}
	var httpError *apperr.HTTPError
	if errors.As(err, &httpError) {
		writeJSONResponse(w, httpError.Status, httpError.Detail)
		return
	}
	var valueError *apperr.ValueError
	if errors.As(err, &valueError) {
		writeJSONResponse(w, http.StatusBadRequest, valueError.Message)
		return
	}
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

// contentDispositionFilename mirrors starlette.responses.FileResponse: ASCII
// filenames use filename="...", anything else falls back to RFC 5987.
func contentDisposition(filename string) string {
	quoted := pythonQuote(filename, "/")
	if quoted != filename {
		return "attachment; filename*=utf-8''" + quoted
	}
	return fmt.Sprintf("attachment; filename=%q", filename)
}

// pythonQuote reproduces urllib.parse.quote(value, safe=safe).
func pythonQuote(value string, safe string) string {
	const unreserved = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-~"
	var builder strings.Builder
	builder.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if strings.IndexByte(unreserved, c) >= 0 || strings.IndexByte(safe, c) >= 0 {
			builder.WriteByte(c)
			continue
		}
		builder.WriteString(fmt.Sprintf("%%%02X", c))
	}
	return builder.String()
}

// writeFileResponse mirrors fastapi.responses.FileResponse: the media type and
// the attachment filename are honoured and range requests keep working.
// It does not write to w on error, so the caller can still emit a JSON body.
func writeFileResponse(w http.ResponseWriter, r *http.Request, path string, mediaType string, filename string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mediaType)
	if filename != "" {
		w.Header().Set("Content-Disposition", contentDisposition(filename))
	}
	w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	return nil
}
