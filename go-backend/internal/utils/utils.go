// Package utils is the Go port of backend/utils.py.
package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pixia1234/super-clipboard/backend/internal/apperr"
)

const textClipHTMLTemplate = `<!doctype html>
<html lang="zh-CN">
  <head>
    <meta charset="utf-8" />
    <title>Super Clipboard 直链 %s</title>
    <meta name="viewport" content="width=device-width,initial-scale=1" />
    <style>
      body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; margin: 32px; color: #0f172a; background: #f8fafc; }
      pre { white-space: pre-wrap; word-break: break-word; padding: 24px; background: #fff; border-radius: 16px; box-shadow: 0 12px 32px rgba(15, 23, 42, 0.12); }
      footer { margin-top: 24px; font-size: 0.875rem; color: #64748b; }
    </style>
  </head>
  <body>
    <h1>直链文本</h1>
    <pre>%s</pre>
    <footer>创建于 %s, 下载次数 %d</footer>
  </body>
</html>`

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// BuildTextClipHTML mirrors build_text_clip_html. Only &, < and > are escaped,
// matching the Python implementation byte for byte.
func BuildTextClipHTML(content string, createdAt time.Time, downloadCount int, code *string) string {
	escaped := htmlEscaper.Replace(content)
	created := createdAt.Local().Format("2006-01-02 15:04:05")
	titleCode := ""
	if code != nil {
		titleCode = *code
	}
	return fmt.Sprintf(textClipHTMLTemplate, titleCode, escaped, created, downloadCount)
}

// BuildBaseURL mirrors build_base_url: `str(request.base_url).rstrip("/")`,
// i.e. "<scheme>://<host header>" without a trailing slash.
//
// uvicorn enables proxy_headers by default, so X-Forwarded-Proto and
// X-Forwarded-Host take precedence when present (first comma-separated value).
func BuildBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := firstHeaderValue(r.Header, "X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	if forwardedHost := firstHeaderValue(r.Header, "X-Forwarded-Host"); forwardedHost != "" {
		host = forwardedHost
	}
	if host == "" {
		host = "localhost"
	}
	return strings.TrimRight(scheme+"://"+host, "/")
}

// firstHeaderValue returns the first comma-separated entry of a header.
func firstHeaderValue(header http.Header, key string) string {
	raw := header.Get(key)
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(raw, ",")[0])
}

// ExtractClientIP mirrors _extract_client_ip: first X-Forwarded-For entry, else
// the peer address of the connection.
func ExtractClientIP(r *http.Request) string {
	if forwarded := firstHeaderValue(r.Header, "X-Forwarded-For"); forwarded != "" {
		return forwarded
	}
	if r.RemoteAddr != "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			return host
		}
		return r.RemoteAddr
	}
	return ""
}

// IsPrivateAddress reproduces the `client_ip.startswith(("127.", "10.", "192.168.", "172."))`
// guard used before forwarding the IP to the captcha provider.
//
// The "172." prefix is intentionally broader than RFC 1918 (which is only
// 172.16/12): the Python backend uses startswith, and matching it keeps
// captcha remoteip behaviour identical.
func IsPrivateAddress(ip string) bool {
	if ip == "" {
		return false
	}
	for _, prefix := range []string{"127.", "10.", "192.168.", "172."} {
		if strings.HasPrefix(ip, prefix) {
			return true
		}
	}
	return false
}

// CaptchaOptions groups the parameters of verify_captcha_token.
type CaptchaOptions struct {
	Provider    string
	Secret      string
	RemoteIP    string
	Timeout     time.Duration
	BypassToken string
	Client      *http.Client
}

var captchaEndpoints = map[string]string{
	"turnstile": "https://challenges.cloudflare.com/turnstile/v0/siteverify",
	"recaptcha": "https://www.google.com/recaptcha/api/siteverify",
}

// VerifyCaptchaToken mirrors verify_captcha_token. It returns nil when captcha
// is disabled, an *apperr.HTTPError (400) for rejected tokens and a plain error
// for transport failures (which FastAPI surfaced as a 500).
func VerifyCaptchaToken(ctx context.Context, token *string, options CaptchaOptions) error {
	if options.Provider == "" || options.Secret == "" {
		return nil
	}
	if token == nil || *token == "" {
		return apperr.NewHTTPError(http.StatusBadRequest, "缺少验证码，请重新验证后再试")
	}
	if options.BypassToken != "" && *token == options.BypassToken {
		return nil
	}

	endpoint, ok := captchaEndpoints[options.Provider]
	if !ok {
		return apperr.NewHTTPError(http.StatusBadRequest, "验证码服务未配置")
	}

	payload := url.Values{}
	payload.Set("secret", options.Secret)
	payload.Set("response", *token)
	if options.RemoteIP != "" {
		payload.Set("remoteip", options.RemoteIP)
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(payload.Encode()))
	if err != nil {
		return fmt.Errorf("unable to build captcha request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := client.Do(request)
	if err != nil {
		// httpx transport errors propagate in Python and become a 500.
		return fmt.Errorf("captcha verification request failed: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("captcha verification response unreadable: %w", err)
	}

	var data struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return apperr.NewHTTPError(http.StatusBadRequest, "验证码校验失败")
	}
	if !data.Success {
		codes := strings.Join(data.ErrorCodes, ", ")
		detail := "验证码校验失败"
		if codes != "" {
			detail += "（" + codes + "）"
		}
		return apperr.NewHTTPError(http.StatusBadRequest, detail)
	}
	return nil
}
