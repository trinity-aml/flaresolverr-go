package flaresolverr

import browserpkg "github.com/trinity-aml/flaresolverr-go/server/browser"

const (
	StatusOK    = "ok"
	StatusError = "error"
)

type Proxy = browserpkg.Proxy
type Cookie = browserpkg.Cookie

type V1Request struct {
	Cmd               string   `json:"cmd,omitempty"`
	Cookies           []Cookie `json:"cookies,omitempty"`
	MaxTimeout        int      `json:"maxTimeout,omitempty"`
	Proxy             *Proxy   `json:"proxy,omitempty"`
	Session           string   `json:"session,omitempty"`
	SessionTTLMinutes int      `json:"session_ttl_minutes,omitempty"`
	Headers           any      `json:"headers,omitempty"`
	UserAgent         string   `json:"userAgent,omitempty"`

	URL               string `json:"url,omitempty"`
	PostData          string `json:"postData,omitempty"`
	ReturnOnlyCookies bool   `json:"returnOnlyCookies,omitempty"`
	ReturnScreenshot  bool   `json:"returnScreenshot,omitempty"`
	Download          *bool  `json:"download,omitempty"`
	ReturnRawHTML     *bool  `json:"returnRawHtml,omitempty"`
	WaitInSeconds     int    `json:"waitInSeconds,omitempty"`
	DisableMedia      *bool  `json:"disableMedia,omitempty"`
	TabsTillVerify    *int   `json:"tabs_till_verify,omitempty"`
}

type ChallengeResolutionResult = browserpkg.ChallengeResolutionResult

type V1Response struct {
	Status         string                     `json:"status"`
	Message        string                     `json:"message"`
	Session        string                     `json:"session,omitempty"`
	Sessions       []string                   `json:"sessions,omitempty"`
	StartTimestamp int64                      `json:"startTimestamp,omitempty"`
	EndTimestamp   int64                      `json:"endTimestamp,omitempty"`
	Version        string                     `json:"version,omitempty"`
	Solution       *ChallengeResolutionResult `json:"solution,omitempty"`
}

type IndexResponse struct {
	Msg       string `json:"msg"`
	Version   string `json:"version"`
	UserAgent string `json:"userAgent"`
}

type HealthResponse struct {
	Status string `json:"status"`
}
