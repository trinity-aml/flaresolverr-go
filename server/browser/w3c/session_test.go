package w3c

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	browserpkg "github.com/trinity-aml/flaresolverr-go/server/browser"
)

type recorded struct {
	Method string
	Path   string
	Body   map[string]any
}

// newTestSession spins up a fake WebDriver endpoint. handler returns the raw
// JSON to put in the {"value": ...} envelope.
func newTestSession(t *testing.T, handler func(r recorded) (status int, value string)) (*Session, *[]recorded) {
	t.Helper()

	var seen []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{Method: r.Method, Path: r.URL.Path}
		if r.Body != nil {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.Body = body
		}
		seen = append(seen, rec)

		status, value := handler(rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"value":` + value + `}`))
	}))
	t.Cleanup(srv.Close)

	return &Session{HTTP: srv.Client(), BaseURL: srv.URL, ID: "sess-1", ErrPrefix: "testdriver"}, &seen
}

func TestSessionPath(t *testing.T) {
	s := &Session{ID: "abc"}
	if got := s.Path("/url"); got != "/session/abc/url" {
		t.Errorf("Path = %q, want %q", got, "/session/abc/url")
	}
	if got := s.Path(""); got != "/session/abc" {
		t.Errorf("Path(\"\") = %q, want %q", got, "/session/abc")
	}
}

func TestWrapExpression(t *testing.T) {
	tests := []struct{ in, want string }{
		{"navigator.userAgent", "return (navigator.userAgent)"},
		{"  document.title  ", "return (document.title)"},
		{"foo();", "return (foo())"},
		{"", "return null"},
		{"   ", "return null"},
	}
	for _, tc := range tests {
		if got := WrapExpression(tc.in); got != tc.want {
			t.Errorf("WrapExpression(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDoUnwrapsEnvelope(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) {
		return http.StatusOK, `"hello"`
	})

	raw, _, err := sess.Do(context.Background(), http.MethodGet, "/x", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(raw) != `"hello"` {
		t.Errorf("value = %s, want %q", raw, `"hello"`)
	}
}

// A W3C error body carries the useful message; it must win over the bare status.
func TestDoSurfacesWebDriverErrorMessage(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) {
		return http.StatusBadRequest, `{"error":"invalid selector","message":"selector is not valid"}`
	})

	_, _, err := sess.Do(context.Background(), http.MethodGet, "/x", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != "selector is not valid" {
		t.Errorf("err = %q, want the driver's message", err)
	}
}

func TestDoFallsBackToStatusCode(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) {
		return http.StatusInternalServerError, `{}`
	})

	_, _, err := sess.Do(context.Background(), http.MethodGet, "/x", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	// ErrPrefix must name the driver so the message says which one failed.
	if !strings.Contains(err.Error(), "testdriver http 500") {
		t.Errorf("err = %q, want it to mention the prefix and status", err)
	}
}

func TestDoHonoursContextCancellation(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `null` })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := sess.Do(ctx, http.MethodGet, "/x", nil); err == nil {
		t.Error("expected a cancelled context to abort the request")
	}
}

func TestExecuteWrapsAndPostsScript(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) {
		return http.StatusOK, `"Example"`
	})

	got, err := sess.ExecuteString(context.Background(), "document.title")
	if err != nil {
		t.Fatalf("ExecuteString: %v", err)
	}
	if got != "Example" {
		t.Errorf("value = %q, want %q", got, "Example")
	}

	rec := (*seen)[0]
	if rec.Path != "/session/sess-1/execute/sync" {
		t.Errorf("path = %q", rec.Path)
	}
	if rec.Body["script"] != "return (document.title)" {
		t.Errorf("script = %v, want it wrapped", rec.Body["script"])
	}
	// args must always be present, never null — some drivers reject a missing key.
	if _, ok := rec.Body["args"]; !ok {
		t.Error("args key missing from the payload")
	}
}

func TestExecuteBool(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `true` })

	got, err := sess.ExecuteBool(context.Background(), "1 === 1")
	if err != nil {
		t.Fatalf("ExecuteBool: %v", err)
	}
	if !got {
		t.Error("expected true")
	}
}

func TestSelectorExistsQuotesSelector(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `false` })

	if _, err := sess.SelectorExists(context.Background(), `div[data-x="1"]`); err != nil {
		t.Fatalf("SelectorExists: %v", err)
	}
	script, _ := (*seen)[0].Body["script"].(string)
	if !strings.Contains(script, `\"1\"`) {
		t.Errorf("selector not safely quoted into the script: %s", script)
	}
}

func TestCookiesMapsFields(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) {
		return http.StatusOK, `[{"name":"cf","value":"v","domain":".x.com","path":"/","httpOnly":true,"secure":true,"sameSite":"Lax","expiry":1893456000}]`
	})

	cookies, err := sess.Cookies(context.Background())
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != "cf" || c.Value != "v" || c.Domain != ".x.com" || c.Path != "/" ||
		!c.HTTPOnly || !c.Secure || c.SameSite != "Lax" || c.Expires != 1893456000 {
		t.Errorf("cookie mapped wrong: %+v", c)
	}
}

func TestSetCookiesPayload(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `null` })

	err := sess.SetCookies(context.Background(), "https://target.example/path", []browserpkg.Cookie{
		{Name: "a", Value: "1"},
		{Name: "", Value: "skipped"},
		{Name: "b", Value: "2", Path: "/sub", Domain: "other.example", SameSite: "Strict", Expires: 123},
	})
	if err != nil {
		t.Fatalf("SetCookies: %v", err)
	}

	if len(*seen) != 2 {
		t.Fatalf("expected 2 requests (the nameless cookie is skipped), got %d", len(*seen))
	}

	first, _ := (*seen)[0].Body["cookie"].(map[string]any)
	if first["path"] != "/" {
		t.Errorf("empty path must default to /, got %v", first["path"])
	}
	if first["domain"] != "target.example" {
		t.Errorf("empty domain must fall back to the URL host, got %v", first["domain"])
	}
	if first["secure"] != true {
		t.Errorf("an https URL must imply secure, got %v", first["secure"])
	}
	if _, ok := first["sameSite"]; ok {
		t.Error("sameSite must be omitted when unset")
	}
	if _, ok := first["expiry"]; ok {
		t.Error("expiry must be omitted when unset")
	}

	second, _ := (*seen)[1].Body["cookie"].(map[string]any)
	if second["path"] != "/sub" || second["domain"] != "other.example" {
		t.Errorf("explicit path/domain not preserved: %v", second)
	}
	if second["sameSite"] != "Strict" {
		t.Errorf("sameSite = %v", second["sameSite"])
	}
	if second["expiry"] != float64(123) {
		t.Errorf("expiry = %v", second["expiry"])
	}
}

func TestDeleteClearsSessionID(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `null` })

	sess.Delete(context.Background())
	if sess.ID != "" {
		t.Errorf("ID = %q, want it cleared", sess.ID)
	}
	if len(*seen) != 1 || (*seen)[0].Method != http.MethodDelete {
		t.Errorf("expected one DELETE, got %+v", *seen)
	}

	// A second Delete must not issue another request.
	sess.Delete(context.Background())
	if len(*seen) != 1 {
		t.Errorf("Delete on an empty session must be a no-op, got %d requests", len(*seen))
	}
}

func TestNavigatePostsURL(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `null` })

	if err := sess.Navigate(context.Background(), "https://example.com/x"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	rec := (*seen)[0]
	if rec.Method != http.MethodPost || rec.Path != "/session/sess-1/url" {
		t.Errorf("unexpected request %s %s", rec.Method, rec.Path)
	}
	if rec.Body["url"] != "https://example.com/x" {
		t.Errorf("url = %v", rec.Body["url"])
	}
}

func TestStatusUsesUnscopedPath(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `{"ready":true}` })

	if err := sess.Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	// /status is driver-scoped, not session-scoped.
	if (*seen)[0].Path != "/status" {
		t.Errorf("path = %q, want /status", (*seen)[0].Path)
	}
}

func TestFreeLocalPort(t *testing.T) {
	port, err := FreeLocalPort()
	if err != nil {
		t.Fatalf("FreeLocalPort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Errorf("port = %d, out of range", port)
	}
}
