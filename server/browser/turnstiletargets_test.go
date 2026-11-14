package browser

import (
	"strings"
	"testing"
)

// The Cloudflare branding link must never be a click candidate.
//
// A bare interstitial carries exactly one anchor — the "Cloudflare" link in
// its footer, which opens www.cloudflare.com in a new tab — and the old filter
// admitted it twice: through a `case "a"` that only asked whether Src was
// empty (Src is populated for iframes alone, so every anchor passed), and
// through the TabIndex branch above it, because the DOM reports tabIndex 0 for
// any focusable anchor. Fixing one without the other changes nothing, which is
// why this test states both shapes.
//
// Measured on the production box 2026-11-14: every failed solve logged
// `closed stray browser tabs the page opened closed=3` — three cloudflare.com
// tabs per 90-second attempt — while the widget was never clicked.
func TestTheBrandingLinkIsNeverClickable(t *testing.T) {
	branding := ClickTarget{
		Kind: "anchor", Tag: "a",
		Text:    "Cloudflare",
		Visible: true, Width: 80, Height: 16,
		// Src is empty for every anchor: the collector fills it for iframes only.
		Src: "",
		// What the DOM reports for a focusable link.
		TabIndex: 0,
	}

	if got := TabbableTargets([]ClickTarget{branding}); len(got) != 0 {
		t.Errorf("the Cloudflare branding link is a tabbable click candidate again: %+v", got)
	}
	if got := FallbackChallengeTargets([]ClickTarget{branding}); len(got) != 0 {
		t.Errorf("the Cloudflare branding link reached the fallback candidates: %+v", got)
	}
	if got := RelevantChallengeTargets([]ClickTarget{branding}); len(got) != 0 {
		t.Errorf("the Cloudflare branding link counted as a challenge target: %+v", got)
	}
}

// Excluding anchors must not cost a real control. An anchor that declares
// itself a button still qualifies.
func TestAnAnchorActingAsAButtonStaysClickable(t *testing.T) {
	control := ClickTarget{
		Kind: "role_button", Tag: "a", Role: "button",
		Visible: true, Width: 120, Height: 40, TabIndex: 0,
	}
	if got := TabbableTargets([]ClickTarget{control}); len(got) != 1 {
		t.Errorf("an anchor with role=button was dropped: %+v", got)
	}
}

// The widget's own checkbox must be a target, and must be tried first.
//
// At ~24x24 with no text it matched nothing before: not IsVerifyButtonTarget
// (which wants the words "verify" and "human"), not IsChallengeIframeTarget,
// and not the fallback's 40px minimum width. So the one element the challenge
// wants clicked was the one element no filter selected.
func TestTheTurnstileCheckboxIsTheFirstTarget(t *testing.T) {
	checkbox := ClickTarget{
		Kind: "input", Tag: "input", Type: "checkbox",
		Visible: true, Width: 24, Height: 24,
	}
	iframe := ClickTarget{
		Kind: "iframe", Tag: "iframe", Src: "https://challenges.cloudflare.com/cdn-cgi/challenge-platform/...",
		Visible: true, Width: 300, Height: 65,
	}

	if !IsTurnstileCheckboxTarget(checkbox) {
		t.Fatal("a 24x24 input[type=checkbox] is not recognised as the Turnstile checkbox")
	}

	// Deliberately listed after the iframe: document order must not decide
	// which target is clicked first.
	relevant := RelevantChallengeTargets([]ClickTarget{iframe, checkbox})
	if len(relevant) != 2 {
		t.Fatalf("expected both the iframe and the checkbox, got %+v", relevant)
	}
	if !IsTurnstileCheckboxTarget(relevant[0]) {
		t.Errorf("the checkbox is not tried first; the caller stops at the first target that clears the challenge, so order is the contract. got %+v", relevant[0])
	}

	if got := FallbackChallengeTargets([]ClickTarget{checkbox}); len(got) != 1 {
		t.Errorf("the checkbox was filtered out of the fallback candidates by the size window: %+v", got)
	}
}

// A checkbox the widget has already ticked must not be clicked again.
func TestACheckedCheckboxIsNotClickedAgain(t *testing.T) {
	checked := ClickTarget{
		Kind: "input", Tag: "input", Type: "checkbox", AriaChecked: "true",
		Visible: true, Width: 24, Height: 24,
	}
	if !TargetAlreadyChecked(checked) {
		t.Error("aria-checked=true was not read as already clicked")
	}
	unchecked := checked
	unchecked.AriaChecked = "false"
	if TargetAlreadyChecked(unchecked) {
		t.Error("aria-checked=false was read as already clicked")
	}
	blank := checked
	blank.AriaChecked = ""
	if TargetAlreadyChecked(blank) {
		t.Error("a checkbox with no aria-checked at all was read as already clicked")
	}
}

// A disabled checkbox is not a click candidate.
func TestADisabledCheckboxIsNotATarget(t *testing.T) {
	disabled := ClickTarget{
		Kind: "input", Tag: "input", Type: "checkbox", Disabled: true,
		Visible: true, Width: 24, Height: 24,
	}
	if IsTurnstileCheckboxTarget(disabled) {
		t.Error("a disabled checkbox is still a target")
	}
}

// The collector has to supply aria-checked, or TargetAlreadyChecked is always
// false and the idempotence check above is decoration.
func TestTheCollectorReportsAriaChecked(t *testing.T) {
	if !strings.Contains(ClickTargetsScript, "ariaChecked") {
		t.Error("ClickTargetsScript no longer collects aria-checked, so TargetAlreadyChecked can never fire")
	}
}
