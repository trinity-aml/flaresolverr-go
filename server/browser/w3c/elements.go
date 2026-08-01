package w3c

import (
	"context"
	"encoding/json"
	"fmt"
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
// Both drivers answer this for *closed* shadow roots, which is what makes the
// Turnstile widget reachable at all. Firefox never had an alternative. Chromium
// used to offer one — --enable-blink-features=FakeShadowRoot, which exposed
// el.fakeShadowRoot to page JS — but that feature is gone as of Chrome 151:
// attaching a closed root to a fresh element and reading .fakeShadowRoot yields
// undefined even with the flag passed. So this endpoint is now the only door in
// on both engines.
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

// Bounds on the shadow walk. Each candidate host costs one driver round-trip,
// so an unbounded walk on a large page is slow enough to eat a whole maxTimeout.
const (
	ShadowWalkMaxNodes = 400
	ShadowWalkMaxDepth = 6
)

// FindThroughShadowDOM finds the first element matching selector, descending
// into shadow roots when the document itself has no match. It returns the
// element reference and how many hosts were probed getting there; both are empty
// when nothing matched.
//
// Shared by every driver-based backend on purpose: the walk is expressed purely
// in terms of the calls above, and the engine-specific part — whether a closed
// root is handed over — happens inside ShadowRoot.
func (s *Session) FindThroughShadowDOM(ctx context.Context, selector string) (string, int, error) {
	ids, err := s.FindElements(ctx, selector)
	if err != nil {
		return "", 0, err
	}
	if len(ids) > 0 {
		return ids[0], 0, nil
	}

	walked := 0
	roots := []string{""} // "" is the document itself
	for depth := 0; depth < ShadowWalkMaxDepth && len(roots) > 0; depth++ {
		var deeper []string
		for _, root := range roots {
			hosts, err := s.findAllIn(ctx, root, "*")
			if err != nil {
				continue
			}
			for _, host := range hosts {
				if walked >= ShadowWalkMaxNodes {
					return "", walked, fmt.Errorf("shadow walk hit its %d-node cap", ShadowWalkMaxNodes)
				}
				walked++

				shadow, err := s.ShadowRoot(ctx, host)
				if err != nil || shadow == "" {
					continue
				}
				if found, err := s.FindElementsInShadow(ctx, shadow, selector); err == nil && len(found) > 0 {
					return found[0], walked, nil
				}
				deeper = append(deeper, shadow)
			}
		}
		roots = deeper
	}
	return "", walked, nil
}

// findAllIn searches a shadow root, or the document when root is "".
func (s *Session) findAllIn(ctx context.Context, root, selector string) ([]string, error) {
	if root == "" {
		return s.FindElements(ctx, selector)
	}
	return s.FindElementsInShadow(ctx, root, selector)
}

// ElementRect returns the element's position and size in viewport coordinates.
func (s *Session) ElementRect(ctx context.Context, elementID string) (x, y, width, height float64, err error) {
	raw, _, err := s.Do(ctx, http.MethodGet, s.Path("/element/"+elementID+"/rect"), nil)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var rect struct {
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	}
	if err := json.Unmarshal(raw, &rect); err != nil {
		return 0, 0, 0, 0, err
	}
	return rect.X, rect.Y, rect.Width, rect.Height, nil
}

// ElementAttribute returns an element attribute, or "" when it is absent.
func (s *Session) ElementAttribute(ctx context.Context, elementID, name string) (string, error) {
	raw, _, err := s.Do(ctx, http.MethodGet, s.Path("/element/"+elementID+"/attribute/"+name), nil)
	if err != nil {
		if HasCode(err, CodeNoSuchElement) {
			return "", nil
		}
		return "", err
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return "", nil
	}
	return *value, nil
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
