package w3c

import (
	"context"
	"encoding/json"
	"net/http"
)

// The JSON keys the W3C protocol uses to carry element and shadow-root
// references. Both are constants fixed by the spec, not per-driver values.
const (
	ElementKey = "element-6066-11e4-a52e-4f735466cecf"
	ShadowKey  = "shadow-6066-11e4-a52e-4f735466cecf"
)

// W3C error codes the element lookups treat as "nothing matched" rather than a
// failure.
const (
	CodeNoSuchElement    = "no such element"
	CodeNoSuchShadowRoot = "no such shadow root"
)

// The calls below address the DOM by driver-side reference instead of running a
// selector in the page. That distinction matters for one reason: a *closed*
// shadow root is invisible to page JavaScript (el.shadowRoot is null) but the
// driver still hands it over, so anything reachable only through one — a
// Cloudflare Turnstile widget, for instance — can be found and clicked here and
// nowhere else.

// FindElements returns a reference for every element matching selector in the
// current browsing context. No match yields an empty slice, not an error.
func (s *Session) FindElements(ctx context.Context, selector string) ([]string, error) {
	return s.findElements(ctx, s.Path("/elements"), selector)
}

// FindElementsInShadow searches inside a shadow root previously returned by
// ShadowRoot.
func (s *Session) FindElementsInShadow(ctx context.Context, shadowID, selector string) ([]string, error) {
	return s.findElements(ctx, s.Path("/shadow/"+shadowID+"/elements"), selector)
}

func (s *Session) findElements(ctx context.Context, path, selector string) ([]string, error) {
	raw, _, err := s.Do(ctx, http.MethodPost, path, map[string]any{
		"using": "css selector",
		"value": selector,
	})
	if err != nil {
		// Find Elements is specified to answer zero matches with an empty
		// array, and geckodriver does; this keeps a driver that reports it as
		// an error from turning a normal miss into a failure.
		if HasCode(err, CodeNoSuchElement) {
			return nil, nil
		}
		return nil, err
	}

	var refs []map[string]string
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		if id := ref[ElementKey]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ShadowRoot returns the shadow root hosted by elementID, or "" when the element
// hosts none.
//
// Firefox answers this for closed shadow roots as well, which is what makes the
// Turnstile widget reachable on the geckodriver backend at all: Firefox has no
// equivalent of Chromium's --enable-blink-features=FakeShadowRoot, so this
// endpoint is the only door in.
func (s *Session) ShadowRoot(ctx context.Context, elementID string) (string, error) {
	raw, _, err := s.Do(ctx, http.MethodGet, s.Path("/element/"+elementID+"/shadow"), nil)
	if err != nil {
		if HasCode(err, CodeNoSuchShadowRoot) || HasCode(err, CodeNoSuchElement) {
			return "", nil
		}
		return "", err
	}

	var ref map[string]string
	if err := json.Unmarshal(raw, &ref); err != nil {
		return "", err
	}
	return ref[ShadowKey], nil
}

// ClickElement issues a native WebDriver click: the driver scrolls the element
// into view and synthesises a trusted pointer event, which reaches content that
// a script-dispatched event cannot.
func (s *Session) ClickElement(ctx context.Context, elementID string) error {
	_, _, err := s.Do(ctx, http.MethodPost, s.Path("/element/"+elementID+"/click"), map[string]any{})
	return err
}

// ElementSelected reports the checked state of a checkbox or radio input.
func (s *Session) ElementSelected(ctx context.Context, elementID string) (bool, error) {
	raw, _, err := s.Do(ctx, http.MethodGet, s.Path("/element/"+elementID+"/selected"), nil)
	if err != nil {
		return false, err
	}
	var selected bool
	if err := json.Unmarshal(raw, &selected); err != nil {
		return false, err
	}
	return selected, nil
}

// SwitchToFrame makes the browsing context owned by an iframe element the
// current one. Every subsequent command — Title, HTML, Execute, element lookups
// — addresses that frame, so callers must pair this with
// SwitchToDefaultContent.
func (s *Session) SwitchToFrame(ctx context.Context, elementID string) error {
	_, _, err := s.Do(ctx, http.MethodPost, s.Path("/frame"), map[string]any{
		"id": map[string]string{ElementKey: elementID},
	})
	return err
}

// SwitchToDefaultContent returns to the top-level browsing context.
func (s *Session) SwitchToDefaultContent(ctx context.Context) error {
	_, _, err := s.Do(ctx, http.MethodPost, s.Path("/frame"), map[string]any{"id": nil})
	return err
}
