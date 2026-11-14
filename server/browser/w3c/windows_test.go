package w3c

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The window API exists for one reason: a live tab keeps its renderer process
// alive, and nothing in this library used to close the tabs a challenge page
// opens. Measured on production 2026-11-14 — 7 of 8 page targets were the
// Cloudflare branding link from the interstitial, ~230 MB of renderer each.
//
// These pin the wire calls, because getting the method or the body shape wrong
// fails at runtime inside a sweep whose errors are deliberately swallowed.
func TestWindowCallsUseTheRightWireShape(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/session/S/window/handles":
			_, _ = w.Write([]byte(`{"value":["a","b","c"]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/session/S/window":
			_, _ = w.Write([]byte(`{"value":"a"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/session/S/window":
			if _, ok := body["handle"]; !ok {
				t.Errorf("switch sent %v, want a handle field", body)
			}
			_, _ = w.Write([]byte(`{"value":null}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/session/S/window":
			_, _ = w.Write([]byte(`{"value":["a"]}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	s := &Session{ID: "S", BaseURL: srv.URL, HTTP: NewHTTPClient()}
	ctx := context.Background()

	handles, err := s.WindowHandles(ctx)
	if err != nil || len(handles) != 3 {
		t.Fatalf("WindowHandles = %v, %v; want 3 handles", handles, err)
	}
	cur, err := s.WindowHandle(ctx)
	if err != nil || cur != "a" {
		t.Fatalf("WindowHandle = %q, %v; want \"a\"", cur, err)
	}
	if err := s.SwitchToWindow(ctx, "b"); err != nil {
		t.Fatalf("SwitchToWindow: %v", err)
	}
	if err := s.CloseWindow(ctx); err != nil {
		t.Fatalf("CloseWindow: %v", err)
	}
	// DELETE, not POST: closing is a different verb on the same path, and
	// getting that pair wrong is the kind of mistake a sweep hides.
	want := []string{
		"GET /session/S/window/handles",
		"GET /session/S/window",
		"POST /session/S/window",
		"DELETE /session/S/window",
	}
	for i, w := range want {
		if i >= len(seen) || seen[i] != w {
			t.Errorf("call %d was %q, want %q", i, func() string {
				if i < len(seen) {
					return seen[i]
				}
				return "(missing)"
			}(), w)
		}
	}
}
