package browser

import (
	"context"
	"log/slog"
	"strings"
)

type Config struct {
	BrowserPath         string
	DriverPath          string
	Headless            bool
	StartupUserAgent    string
	LogHTML             bool
	DisableMedia        bool
	DriverCacheDir      string
	DriverAutoDownload  bool
	ChromeForTestingURL string
	Logger              *slog.Logger
}

type Proxy struct {
	URL      string `json:"url,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
	Size     int64   `json:"size,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	Session  bool    `json:"session,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
}

type ChallengeResolutionResult struct {
	URL            string            `json:"url,omitempty"`
	Status         int               `json:"status,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Response       string            `json:"response,omitempty"`
	Cookies        []Cookie          `json:"cookies,omitempty"`
	UserAgent      string            `json:"userAgent,omitempty"`
	Screenshot     string            `json:"screenshot,omitempty"`
	TurnstileToken string            `json:"turnstile_token,omitempty"`
}

type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type Request struct {
	URL               string   `json:"url,omitempty"`
	Method            string   `json:"method,omitempty"`
	PostData          string   `json:"postData,omitempty"`
	Cookies           []Cookie `json:"cookies,omitempty"`
	MaxTimeoutMS      int      `json:"maxTimeoutMs,omitempty"`
	ReturnOnlyCookies bool     `json:"returnOnlyCookies,omitempty"`
	ReturnScreenshot  bool     `json:"returnScreenshot,omitempty"`
	WaitInSeconds     int      `json:"waitInSeconds,omitempty"`
	DisableMedia      bool     `json:"disableMedia,omitempty"`
	TabsTillVerify    *int     `json:"tabsTillVerify,omitempty"`
	LogHTML           bool     `json:"logHtml,omitempty"`
}

type Result struct {
	Result  *ChallengeResolutionResult
	Message string
}

type Client interface {
	UserAgent(context.Context) (string, error)
	Resolve(context.Context, Request) (*Result, error)
	Close() error
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
