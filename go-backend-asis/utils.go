package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

// buildTextClipHTML returns an HTML page for displaying a text clip.
func buildTextClipHTML(content string, createdAt time.Time, downloadCount int, code string) string {
	escaped := html.EscapeString(content)
	created := createdAt.In(time.Local).Format("2006-01-02 15:04:05")
	titleCode := code
	return fmt.Sprintf(`<!doctype html>
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
</html>`, titleCode, escaped, created, downloadCount)
}

// buildBaseURL returns the base URL (scheme + host) with no trailing slash.
func buildBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwdProto := r.Header.Get("x-forwarded-proto"); fwdProto != "" {
		scheme = strings.Split(fwdProto, ",")[0]
		scheme = strings.TrimSpace(scheme)
	}
	host := r.Host
	if fwdHost := r.Header.Get("x-forwarded-host"); fwdHost != "" {
		host = strings.Split(fwdHost, ",")[0]
		host = strings.TrimSpace(host)
	}
	base := fmt.Sprintf("%s://%s", scheme, host)
	return strings.TrimRight(base, "/")
}

type captchaResponse struct {
	Success   bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

// verifyCaptchaToken verifies a captcha token with the provider. If provider/secret are empty, it's a no-op.
func verifyCaptchaToken(token, provider, secret, remoteIP string, timeoutSeconds float64, bypassToken string) error {
	if provider == "" || secret == "" {
		return nil
	}
	if token == "" {
		return &HTTPError{Status: http.StatusBadRequest, Detail: "缺少验证码，请重新验证后再试"}
	}
	if bypassToken != "" && token == bypassToken {
		return nil
	}

	var endpoint string
	switch provider {
	case "turnstile":
		endpoint = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	case "recaptcha":
		endpoint = "https://www.google.com/recaptcha/api/siteverify"
	default:
		return &HTTPError{Status: http.StatusBadRequest, Detail: "验证码服务未配置"}
	}

	form := url.Values{}
	form.Set("secret", secret)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	client := &http.Client{Timeout: time.Duration(timeoutSeconds * float64(time.Second))}
	resp, err := client.Post(endpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return &HTTPError{Status: http.StatusBadRequest, Detail: "验证码校验失败"}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &HTTPError{Status: http.StatusBadRequest, Detail: "验证码校验失败"}
	}
	var data captchaResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return &HTTPError{Status: http.StatusBadRequest, Detail: "验证码校验失败"}
	}
	if !data.Success {
		detail := "验证码校验失败"
		if len(data.ErrorCodes) > 0 {
			detail = detail + "（" + strings.Join(data.ErrorCodes, ", ") + "）"
		}
		return &HTTPError{Status: http.StatusBadRequest, Detail: detail}
	}
	return nil
}

// extractClientIP returns the client IP, respecting X-Forwarded-For.
func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("x-forwarded-for"); xff != "" {
		parts := strings.Split(xff, ",")
		candidate := strings.TrimSpace(parts[0])
		if candidate != "" {
			return candidate
		}
	}
	if r.RemoteAddr != "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	}
	return ""
}

// isPrivateIP returns true if the IP looks like a private/internal address.
func isPrivateIP(ip string) bool {
	if ip == "" {
		return false
	}
	if strings.HasPrefix(ip, "127.") || strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "192.168.") {
		return true
	}
	// 172.16.0.0/12
	if strings.HasPrefix(ip, "172.") {
		// check second octet
		parts := strings.Split(ip, ".")
		if len(parts) == 4 {
			var second int
			fmt.Sscanf(parts[1], "%d", &second)
			if second >= 16 && second <= 31 {
				return true
			}
		}
	}
	return false
}
