package w3c

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestFindElementsUnwrapsReferences(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) {
		return http.StatusOK, `[{"` + ElementKey + `":"a"},{"` + ElementKey + `":"b"}]`
	})

	ids, err := sess.FindElements(context.Background(), "iframe")
	if err != nil {
		t.Fatalf("FindElements: %v", err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("ids = %v, want [a b]", ids)
	}

	rec := (*seen)[0]
	if rec.Method != http.MethodPost || rec.Path != "/session/sess-1/elements" {
		t.Errorf("unexpected request %s %s", rec.Method, rec.Path)
	}
	if rec.Body["using"] != "css selector" || rec.Body["value"] != "iframe" {
		t.Errorf("payload = %v", rec.Body)
	}
}

func TestFindElementsEmptyIsNotAnError(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `[]` })

	ids, err := sess.FindElements(context.Background(), "iframe")
	if err != nil {
		t.Fatalf("an empty match must not be an error, got %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty", ids)
	}
}

// Some drivers report a zero-match Find Elements as an error instead of [].
func TestFindElementsTreatsNoSuchElementAsEmpty(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) {
		return http.StatusNotFound, `{"error":"no such element","message":"nothing matched"}`
	})

	ids, err := sess.FindElements(context.Background(), "iframe")
	if err != nil {
		t.Fatalf("no such element must read as empty, got %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty", ids)
	}
}

func TestFindElementsInShadowUsesShadowPath(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `[]` })

	if _, err := sess.FindElementsInShadow(context.Background(), "sh-9", "input"); err != nil {
		t.Fatalf("FindElementsInShadow: %v", err)
	}
	if got := (*seen)[0].Path; got != "/session/sess-1/shadow/sh-9/elements" {
		t.Errorf("path = %q", got)
	}
}

func TestShadowRootReturnsReference(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) {
		return http.StatusOK, `{"` + ShadowKey + `":"sh-1"}`
	})

	got, err := sess.ShadowRoot(context.Background(), "el-1")
	if err != nil {
		t.Fatalf("ShadowRoot: %v", err)
	}
	if got != "sh-1" {
		t.Errorf("shadow = %q, want sh-1", got)
	}
	rec := (*seen)[0]
	if rec.Method != http.MethodGet || rec.Path != "/session/sess-1/element/el-1/shadow" {
		t.Errorf("unexpected request %s %s", rec.Method, rec.Path)
	}
}

// Firefox answers this one with an error code and no message at all — the walk
// over candidate hosts depends on it reading as "no shadow root here".
func TestShadowRootAbsentIsNotAnError(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) {
		return http.StatusNotFound, `{"error":"no such shadow root","message":""}`
	})

	got, err := sess.ShadowRoot(context.Background(), "el-1")
	if err != nil {
		t.Fatalf("a missing shadow root must not be an error, got %v", err)
	}
	if got != "" {
		t.Errorf("shadow = %q, want empty", got)
	}
}

// A transport-level failure must still surface: silently reading it as "no
// shadow root" would make the walk quietly give up.
func TestShadowRootPropagatesRealErrors(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) {
		return http.StatusInternalServerError, `{"error":"unknown error","message":"driver died"}`
	})

	if _, err := sess.ShadowRoot(context.Background(), "el-1"); err == nil {
		t.Fatal("expected the driver error to propagate")
	}
}

func TestDriverErrorCarriesCodeAndMessage(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) {
		return http.StatusNotFound, `{"error":"no such element","message":"gone"}`
	})

	_, _, err := sess.Do(context.Background(), http.MethodGet, "/x", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != "gone" {
		t.Errorf("Error() = %q, want the driver's message", err)
	}
	if !HasCode(err, CodeNoSuchElement) {
		t.Errorf("HasCode(%v, %q) = false", err, CodeNoSuchElement)
	}
	if HasCode(err, CodeNoSuchShadowRoot) {
		t.Error("HasCode matched the wrong code")
	}
}

// With no message the code has to carry the reply, and the text must still name
// the driver and the status.
func TestDriverErrorWithEmptyMessage(t *testing.T) {
	sess, _ := newTestSession(t, func(recorded) (int, string) {
		return http.StatusNotFound, `{"error":"no such shadow root","message":""}`
	})

	_, _, err := sess.Do(context.Background(), http.MethodGet, "/x", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !HasCode(err, CodeNoSuchShadowRoot) {
		t.Errorf("HasCode = false for %v", err)
	}
	if !strings.Contains(err.Error(), "testdriver http 404") ||
		!strings.Contains(err.Error(), "no such shadow root") {
		t.Errorf("err = %q, want it to name the driver, status and code", err)
	}
}

func TestElementSelected(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `true` })

	got, err := sess.ElementSelected(context.Background(), "el-1")
	if err != nil {
		t.Fatalf("ElementSelected: %v", err)
	}
	if !got {
		t.Error("expected true")
	}
	if got := (*seen)[0].Path; got != "/session/sess-1/element/el-1/selected" {
		t.Errorf("path = %q", got)
	}
}

func TestClickElementPostsEmptyBody(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `null` })

	if err := sess.ClickElement(context.Background(), "el-1"); err != nil {
		t.Fatalf("ClickElement: %v", err)
	}
	rec := (*seen)[0]
	if rec.Method != http.MethodPost || rec.Path != "/session/sess-1/element/el-1/click" {
		t.Errorf("unexpected request %s %s", rec.Method, rec.Path)
	}
	// The endpoint rejects a missing body on some drivers.
	if rec.Body == nil {
		t.Error("expected a JSON object body, got none")
	}
}

func TestSwitchToFrameSendsElementReference(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `null` })

	if err := sess.SwitchToFrame(context.Background(), "el-7"); err != nil {
		t.Fatalf("SwitchToFrame: %v", err)
	}
	rec := (*seen)[0]
	if rec.Path != "/session/sess-1/frame" {
		t.Errorf("path = %q", rec.Path)
	}
	id, ok := rec.Body["id"].(map[string]any)
	if !ok {
		t.Fatalf("id = %v, want an element reference object", rec.Body["id"])
	}
	if id[ElementKey] != "el-7" {
		t.Errorf("id = %v, want %s=el-7", id, ElementKey)
	}
}

func TestSwitchToDefaultContentSendsNullID(t *testing.T) {
	sess, seen := newTestSession(t, func(recorded) (int, string) { return http.StatusOK, `null` })

	if err := sess.SwitchToDefaultContent(context.Background()); err != nil {
		t.Fatalf("SwitchToDefaultContent: %v", err)
	}
	rec := (*seen)[0]
	if rec.Path != "/session/sess-1/frame" {
		t.Errorf("path = %q", rec.Path)
	}
	value, present := rec.Body["id"]
	if !present {
		t.Fatal("the id key must be present and null")
	}
	if value != nil {
		t.Errorf("id = %v, want null", value)
	}
}
