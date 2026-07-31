// Package w3c implements the raw W3C WebDriver protocol over HTTP.
//
// Both driver-based backends (chromedriver and geckodriver) speak the same
// protocol, and used to carry byte-identical copies of every call in this file.
// They differed only in the receiver type, one error-message prefix and a couple
// of local variable names — which is how the two copies drifted into having
// different timeout policies and a missing backoff clamp on one side.
//
// No Selenium client library is used; this is the whole client.
package w3c

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	browserpkg "github.com/trinity-aml/flaresolverr-go/server/browser"
)

// Session is a live WebDriver session on one driver process.
//
// It is not safe for concurrent use; both backends already serialize their
// calls behind the browser-level mutex.
type Session struct {
	HTTP    *http.Client
	BaseURL string
	ID      string

	// ErrPrefix labels protocol-level errors so a message names the driver that
	// produced it ("webdriver" / "geckodriver").
	ErrPrefix string
}

// Do issues a WebDriver command and unwraps the {"value": ...} envelope.
func (s *Session) Do(ctx context.Context, method, path string, payload any) (json.RawMessage, string, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, "", err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+path, body)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	var envelope struct {
		Value     json.RawMessage `json:"value"`
		SessionID string          `json:"sessionId"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &envelope); err != nil {
			if resp.StatusCode >= 400 {
				return nil, "", fmt.Errorf("%s http %d: %s", s.ErrPrefix, resp.StatusCode, strings.TrimSpace(string(data)))
			}
			return nil, "", err
		}
	}

	if resp.StatusCode >= 400 {
		var wdErr struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if len(envelope.Value) > 0 && json.Unmarshal(envelope.Value, &wdErr) == nil && strings.TrimSpace(wdErr.Message) != "" {
			return nil, envelope.SessionID, fmt.Errorf("%s", wdErr.Message)
		}
		return nil, envelope.SessionID, fmt.Errorf("%s http %d", s.ErrPrefix, resp.StatusCode)
	}

	return envelope.Value, envelope.SessionID, nil
}

// Path builds a session-scoped endpoint path.
func (s *Session) Path(path string) string {
	return "/session/" + s.ID + path
}

// Delete asks the driver to quit the browser. Callers should pass a bounded
// context: a wedged driver would otherwise stall teardown for the full client
// timeout, once per session.
func (s *Session) Delete(ctx context.Context) {
	if strings.TrimSpace(s.ID) == "" || strings.TrimSpace(s.BaseURL) == "" {
		return
	}
	_, _, _ = s.Do(ctx, http.MethodDelete, s.Path(""), nil)
	s.ID = ""
}

// Status probes the driver's readiness endpoint (no session required).
func (s *Session) Status(ctx context.Context) error {
	_, _, err := s.Do(ctx, http.MethodGet, "/status", nil)
	return err
}

// WrapExpression turns a JS expression into the function body /execute/sync
// expects.
func WrapExpression(script string) string {
	script = strings.TrimSpace(script)
	script = strings.TrimSuffix(script, ";")
	if script == "" {
		return "return null"
	}
	return "return (" + script + ")"
}

func (s *Session) Execute(ctx context.Context, script string, args ...any) (json.RawMessage, error) {
	if args == nil {
		args = []any{}
	}
	raw, _, err := s.Do(ctx, http.MethodPost, s.Path("/execute/sync"), map[string]any{
		"script": WrapExpression(script),
		"args":   args,
	})
	return raw, err
}

func (s *Session) ExecuteString(ctx context.Context, script string) (string, error) {
	raw, err := s.Execute(ctx, script)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func (s *Session) ExecuteBool(ctx context.Context, script string) (bool, error) {
	raw, err := s.Execute(ctx, script)
	if err != nil {
		return false, err
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, err
	}
	return value, nil
}

func (s *Session) Title(ctx context.Context) (string, error) {
	return s.getString(ctx, "/title")
}

func (s *Session) URL(ctx context.Context) (string, error) {
	return s.getString(ctx, "/url")
}

// Screenshot returns a base64-encoded PNG of the viewport.
func (s *Session) Screenshot(ctx context.Context) (string, error) {
	return s.getString(ctx, "/screenshot")
}

func (s *Session) getString(ctx context.Context, path string) (string, error) {
	raw, _, err := s.Do(ctx, http.MethodGet, s.Path(path), nil)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func (s *Session) HTML(ctx context.Context) (string, error) {
	return s.ExecuteString(ctx, `document.documentElement ? document.documentElement.outerHTML : ''`)
}

func (s *Session) SelectorExists(ctx context.Context, selector string) (bool, error) {
	return s.ExecuteBool(ctx, fmt.Sprintf(`document.querySelector(%q) !== null`, selector))
}

// Navigate loads a URL. The caller is responsible for building data: URLs for
// POST replay.
func (s *Session) Navigate(ctx context.Context, targetURL string) error {
	_, _, err := s.Do(ctx, http.MethodPost, s.Path("/url"), map[string]any{"url": targetURL})
	return err
}

type wdCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"`
	Expiry   float64 `json:"expiry"`
}

// Cookies returns the jar for the current origin.
//
// The endpoint is origin-scoped: called while the browser still sits on a
// data: URL it returns an empty jar, which is why resolve() must navigate and
// settle before reading cookies.
func (s *Session) Cookies(ctx context.Context) ([]browserpkg.Cookie, error) {
	raw, _, err := s.Do(ctx, http.MethodGet, s.Path("/cookie"), nil)
	if err != nil {
		return nil, err
	}

	var entries []wdCookie
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}

	result := make([]browserpkg.Cookie, 0, len(entries))
	for _, entry := range entries {
		result = append(result, browserpkg.Cookie{
			Name:     entry.Name,
			Value:    entry.Value,
			Domain:   entry.Domain,
			Path:     entry.Path,
			HTTPOnly: entry.HTTPOnly,
			Secure:   entry.Secure,
			SameSite: entry.SameSite,
			Expires:  entry.Expiry,
		})
	}
	return result, nil
}

// SetCookies installs cookies for the origin of rawURL, one call per cookie as
// the protocol requires.
func (s *Session) SetCookies(ctx context.Context, rawURL string, cookies []browserpkg.Cookie) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	domain := parsed.Hostname()
	secure := strings.EqualFold(parsed.Scheme, "https")

	for _, cookie := range cookies {
		if cookie.Name == "" {
			continue
		}
		payload := map[string]any{
			"name":     cookie.Name,
			"value":    cookie.Value,
			"path":     browserpkg.FirstCookiePath(cookie.Path),
			"domain":   browserpkg.FirstNonEmpty(cookie.Domain, domain),
			"secure":   cookie.Secure || secure,
			"httpOnly": cookie.HTTPOnly,
		}
		if strings.TrimSpace(cookie.SameSite) != "" {
			payload["sameSite"] = cookie.SameSite
		}
		if cookie.Expires > 0 {
			payload["expiry"] = int64(cookie.Expires)
		}
		if _, _, err := s.Do(ctx, http.MethodPost, s.Path("/cookie"), map[string]any{"cookie": payload}); err != nil {
			return err
		}
	}
	return nil
}

// FreeLocalPort reserves an ephemeral port for a driver process to bind.
func FreeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
