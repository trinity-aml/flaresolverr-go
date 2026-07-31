package flaresolverr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()

	server, err := NewServer(Config{LogLevel: "error"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.service.Close() })
	return server
}

func doJSON(t *testing.T, handler http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("response is not JSON (%s %s -> %d): %s", method, path, rec.Code, rec.Body.String())
		}
	}
	return rec, decoded
}

func TestHealthEndpoint(t *testing.T) {
	rec, body := doJSON(t, newTestServer(t).Handler(), http.MethodGet, "/health", "")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body["status"] != StatusOK {
		t.Errorf("status field = %v, want %q", body["status"], StatusOK)
	}
}

func TestIndexEndpoint(t *testing.T) {
	rec, body := doJSON(t, newTestServer(t).Handler(), http.MethodGet, "/", "")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if _, ok := body["version"]; !ok {
		t.Errorf("index response must carry a version: %v", body)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	rec, _ := doJSON(t, newTestServer(t).Handler(), http.MethodGet, "/nope", "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestWrongMethodIs405(t *testing.T) {
	handler := newTestServer(t).Handler()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/health"},
		{http.MethodPost, "/"},
		{http.MethodGet, "/v1"},
		{http.MethodPost, "/settings"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec, _ := doJSON(t, handler, tc.method, tc.path, "")
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
		})
	}
}

// sessions.list must always emit the key, including as [] — Python does, and
// clients index into it.
func TestSessionsListAlwaysEmitsTheKey(t *testing.T) {
	rec, _ := doJSON(t, newTestServer(t).Handler(), http.MethodPost, "/v1", `{"cmd":"sessions.list"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	value, ok := raw["sessions"]
	if !ok {
		t.Fatalf(`the "sessions" key is missing entirely: %s`, rec.Body.String())
	}
	if string(value) != "[]" {
		t.Errorf(`sessions = %s, want []`, value)
	}
}

// Every other command must NOT carry the sessions key.
func TestNonListCommandsOmitTheSessionsKey(t *testing.T) {
	rec, _ := doJSON(t, newTestServer(t).Handler(), http.MethodPost, "/v1", `{"cmd":"sessions.destroy","session":"absent"}`)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["sessions"]; ok {
		t.Errorf(`unexpected "sessions" key: %s`, rec.Body.String())
	}
}

func TestV1RejectsMalformedJSON(t *testing.T) {
	rec, _ := doJSON(t, newTestServer(t).Handler(), http.MethodPost, "/v1", `{"cmd":`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// A browser is expensive; these must be rejected before one is ever launched.
func TestV1RejectsNonHTTPURLsBeforeLaunchingABrowser(t *testing.T) {
	handler := newTestServer(t).Handler()

	for _, raw := range []string{
		"file:///etc/passwd",
		"chrome://settings",
		"ftp://example.com/x",
		"javascript:alert(1)",
		"/relative/path",
		"example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"cmd": "request.get", "url": raw, "maxTimeout": 1000})
			rec, decoded := doJSON(t, handler, http.MethodPost, "/v1", string(body))

			if rec.Code == http.StatusOK {
				t.Fatalf("URL %q was accepted: %s", raw, rec.Body.String())
			}
			if msg, _ := decoded["message"].(string); !strings.Contains(msg, "http or https") {
				t.Errorf("message = %q, want it to mention the http/https requirement", msg)
			}
		})
	}
}

func TestV1RejectsUnknownCommand(t *testing.T) {
	rec, _ := doJSON(t, newTestServer(t).Handler(), http.MethodPost, "/v1", `{"cmd":"request.teleport"}`)

	if rec.Code == http.StatusOK {
		t.Errorf("an unknown command must not return 200: %s", rec.Body.String())
	}
}

func TestV1RequestPostRequiresPostData(t *testing.T) {
	rec, decoded := doJSON(t, newTestServer(t).Handler(), http.MethodPost, "/v1",
		`{"cmd":"request.post","url":"https://example.com","maxTimeout":1000}`)

	if rec.Code == http.StatusOK {
		t.Fatalf("request.post without postData must fail: %s", rec.Body.String())
	}
	if msg, _ := decoded["message"].(string); !strings.Contains(msg, "postData") {
		t.Errorf("message = %q, want it to mention postData", msg)
	}
}

func TestV1RequestGetRejectsPostData(t *testing.T) {
	rec, _ := doJSON(t, newTestServer(t).Handler(), http.MethodPost, "/v1",
		`{"cmd":"request.get","url":"https://example.com","postData":"a=1","maxTimeout":1000}`)

	if rec.Code == http.StatusOK {
		t.Errorf("request.get with postData must fail: %s", rec.Body.String())
	}
}

// The CSRF guard must reject the simple-request shape while leaving GET and
// ordinary JSON clients working.
func TestSettingsAPIGuard(t *testing.T) {
	handler := newTestServer(t).Handler()

	t.Run("GET is always allowed", func(t *testing.T) {
		rec, _ := doJSON(t, handler, http.MethodGet, "/api/settings", "")
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("POST with text/plain is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"host":"0.0.0.0","port":8191}`))
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("POST from a foreign origin is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"host":"0.0.0.0","port":8191}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://evil.example")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})
}

func TestSettingsPageIsServed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestServerHasReadTimeouts(t *testing.T) {
	server := newTestServer(t)

	if server.httpServer.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout must be set (slowloris)")
	}
	if server.httpServer.IdleTimeout == 0 {
		t.Error("IdleTimeout must be set")
	}
	// WriteTimeout must stay unset: a solve legitimately runs for maxTimeout.
	if server.httpServer.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 so long solves are not cut off", server.httpServer.WriteTimeout)
	}
}
