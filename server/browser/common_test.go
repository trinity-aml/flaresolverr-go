package browser

import (
	"strings"
	"testing"
	"time"
)

// TestBuildPostFormHTMLDoesNotPercentEncode guards the regression that the
// chromedp backend carried for a long time in a private copy of this function:
// applying url.QueryEscape on top of html.EscapeString produced a double-encoded
// body, because the browser percent-encodes form values itself on submit.
func TestBuildPostFormHTMLDoesNotPercentEncode(t *testing.T) {
	html := BuildPostFormHTML("https://example.com/login", "username=bob&password=P@ss w/rd")

	for _, want := range []string{
		`value="P@ss w/rd"`,
		`name="password"`,
		`name="username"`,
		`value="bob"`,
		`action="https://example.com/login"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("form HTML missing %s\ngot: %s", want, html)
		}
	}

	for _, unwanted := range []string{"P%40ss", "%2540", "w%2Frd", "+"} {
		if strings.Contains(html, unwanted) {
			t.Errorf("form HTML is percent-encoded (%q present), values would arrive double-encoded\ngot: %s", unwanted, html)
		}
	}
}

func TestBuildPostFormHTMLEscapesHTMLMetacharacters(t *testing.T) {
	html := BuildPostFormHTML("https://example.com/", `q=<script>"x"&y`)

	if strings.Contains(html, `value="<script>`) {
		t.Errorf("HTML metacharacters not escaped, attribute would break out\ngot: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Errorf("expected < and > to be HTML-escaped\ngot: %s", html)
	}
	if !strings.Contains(html, "&#34;") {
		t.Errorf("expected the quote to be HTML-escaped\ngot: %s", html)
	}
}

// The "submit" field is dropped so the injected auto-submit script is what
// actually submits the form.
func TestBuildPostFormHTMLSkipsSubmitField(t *testing.T) {
	html := BuildPostFormHTML("https://example.com/", "submit=Go&keep=1")

	if strings.Contains(html, `name="submit"`) {
		t.Errorf("submit field should be dropped\ngot: %s", html)
	}
	if !strings.Contains(html, `name="keep"`) {
		t.Errorf("non-submit fields must survive\ngot: %s", html)
	}
}

func TestBuildPostFormHTMLTrimsLeadingQuestionMark(t *testing.T) {
	withMark := BuildPostFormHTML("https://example.com/", "?a=1")
	withoutMark := BuildPostFormHTML("https://example.com/", "a=1")

	if withMark != withoutMark {
		t.Errorf("a leading '?' must be ignored\nwith:    %s\nwithout: %s", withMark, withoutMark)
	}
}

func TestNormalizeBlockedPattern(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"already absolute", "https://cdn.example.com/*", "https://cdn.example.com/*"},
		{"star extension", "*.png", "*://*:*/*.png"},
		{"bare extension", ".css", "*://*:*/*.css"},
		{"empty stays empty", "", ""},
		{"whitespace only", "   ", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeBlockedPattern(tc.in); got != tc.want {
				t.Errorf("NormalizeBlockedPattern(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestChromeArgValue(t *testing.T) {
	tests := []struct {
		name, raw, flag, want string
	}{
		{"equals form", "--foo --window-size=800,600 --bar", "window-size", "800,600"},
		{"space form", "--window-size 800,600", "window-size", "800,600"},
		{"missing", "--foo --bar", "window-size", ""},
		{"empty input", "", "window-size", ""},
		{"flag without value at end", "--window-size", "window-size", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChromeArgValue(tc.raw, tc.flag); got != tc.want {
				t.Errorf("ChromeArgValue(%q, %q) = %q, want %q", tc.raw, tc.flag, got, tc.want)
			}
		})
	}
}

func TestScrubUserAgentRemovesHeadlessMarker(t *testing.T) {
	const ua = "Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/120.0.0.0 Safari/537.36"
	got := ScrubUserAgent(ua)

	if strings.Contains(strings.ToLower(got), "headless") {
		t.Errorf("headless marker survived: %q", got)
	}
	if !strings.Contains(got, "Chrome/120.0.0.0") {
		t.Errorf("scrubbing damaged the rest of the UA: %q", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "   ", "value", "other"); got != "value" {
		t.Errorf("FirstNonEmpty = %q, want %q", got, "value")
	}
	if got := FirstNonEmpty("", "  "); got != "" {
		t.Errorf("FirstNonEmpty with no candidates = %q, want empty", got)
	}
}

func TestFirstCookiePathDefaultsToRoot(t *testing.T) {
	if got := FirstCookiePath(""); got != "/" {
		t.Errorf("FirstCookiePath(\"\") = %q, want \"/\"", got)
	}
	if got := FirstCookiePath("/api"); got != "/api" {
		t.Errorf("FirstCookiePath(\"/api\") = %q, want \"/api\"", got)
	}
}

func TestURLsEquivalent(t *testing.T) {
	tests := []struct {
		name, lhs, rhs string
		want           bool
	}{
		{"identical", "https://a.com/x", "https://a.com/x", true},
		{"trailing slash", "https://a.com", "https://a.com/", true},
		{"different path", "https://a.com/x", "https://a.com/y", false},
		{"different host", "https://a.com/", "https://b.com/", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := URLsEquivalent(tc.lhs, tc.rhs); got != tc.want {
				t.Errorf("URLsEquivalent(%q, %q) = %v, want %v", tc.lhs, tc.rhs, got, tc.want)
			}
		})
	}
}

func TestIsVerifyButtonTarget(t *testing.T) {
	tests := []struct {
		name   string
		target ClickTarget
		want   bool
	}{
		{"exact phrase", ClickTarget{Visible: true, Text: "Verify you are human"}, true},
		{"split words", ClickTarget{Visible: true, AriaLabel: "please verify that you are a human"}, true},
		{"invisible is never a target", ClickTarget{Visible: false, Text: "Verify you are human"}, false},
		{"unrelated button", ClickTarget{Visible: true, Text: "Sign in"}, false},
		{"verify without human", ClickTarget{Visible: true, Text: "Verify email"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsVerifyButtonTarget(tc.target); got != tc.want {
				t.Errorf("IsVerifyButtonTarget(%+v) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

func TestIsChallengeIframeTarget(t *testing.T) {
	tests := []struct {
		name   string
		target ClickTarget
		want   bool
	}{
		{"turnstile src", ClickTarget{Kind: "iframe", Visible: true, Src: "https://challenges.cloudflare.com/turnstile/x"}, true},
		{"widget-sized iframe", ClickTarget{Kind: "iframe", Visible: true, Width: 300, Height: 65}, true},
		{"full-page iframe", ClickTarget{Kind: "iframe", Visible: true, Width: 1200, Height: 800}, false},
		{"button is not an iframe", ClickTarget{Kind: "button", Visible: true, Src: "cloudflare"}, false},
		{"invisible iframe", ClickTarget{Kind: "iframe", Visible: false, Src: "cloudflare"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsChallengeIframeTarget(tc.target); got != tc.want {
				t.Errorf("IsChallengeIframeTarget(%+v) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

// A passive challenge (nothing to click) waits longer than an interactive one.
func TestChallengeAutoWaitDuration(t *testing.T) {
	if got := ChallengeAutoWaitDuration(nil); got != 1500*time.Millisecond {
		t.Errorf("passive wait = %v, want 1.5s", got)
	}
	if got := ChallengeAutoWaitDuration([]ClickTarget{{Kind: "button"}}); got != time.Second {
		t.Errorf("interactive wait = %v, want 1s", got)
	}
}

func TestTabbableTargetsDedupes(t *testing.T) {
	target := ClickTarget{Kind: "button", Tag: "button", Role: "button", Visible: true, Width: 100, Height: 40, TabIndex: 0}

	// This target matches both the TabIndex>=0 branch and the Role=="button"
	// branch, so it is appended twice before dedupe.
	got := TabbableTargets([]ClickTarget{target})
	if len(got) != 1 {
		t.Errorf("expected duplicates to be collapsed, got %d targets", len(got))
	}
}

func TestTabbableTargetsSkipsHelperAndInvisible(t *testing.T) {
	targets := []ClickTarget{
		{Tag: "button", ID: "__flaresolverr-focus", Visible: true, Width: 10, Height: 10, TabIndex: 0},
		{Tag: "button", Visible: false, Width: 10, Height: 10, TabIndex: 0},
		{Tag: "button", Visible: true, Disabled: true, Width: 10, Height: 10, TabIndex: 0},
		{Tag: "button", Visible: true, Width: 0, Height: 10, TabIndex: 0},
	}

	if got := TabbableTargets(targets); len(got) != 0 {
		t.Errorf("expected all targets filtered out, got %d: %+v", len(got), got)
	}
}

func TestAppendWithEnvReplacesExistingKey(t *testing.T) {
	env := AppendWithEnv([]string{"PATH=/bin", "DISPLAY=:0", "HOME=/root"}, "DISPLAY", ":99")

	var seen int
	for _, item := range env {
		if strings.HasPrefix(item, "DISPLAY=") {
			seen++
			if item != "DISPLAY=:99" {
				t.Errorf("DISPLAY not replaced: %q", item)
			}
		}
	}
	if seen != 1 {
		t.Errorf("expected exactly one DISPLAY entry, got %d in %v", seen, env)
	}
	if len(env) != 3 {
		t.Errorf("expected the other vars preserved, got %v", env)
	}
}

func TestAppendWithEnvAddsMissingKey(t *testing.T) {
	env := AppendWithEnv([]string{"PATH=/bin"}, "DISPLAY", ":99")

	if len(env) != 2 || env[1] != "DISPLAY=:99" {
		t.Errorf("expected DISPLAY appended, got %v", env)
	}
}

func TestNormalizeResponseHeadersLowercasesKeys(t *testing.T) {
	got := NormalizeResponseHeaders(map[string]any{
		"Content-Type": "text/html",
		"X-Ray-Id":     "abc",
	})

	if got["content-type"] != "text/html" {
		t.Errorf("expected lowercased content-type, got %v", got)
	}
	if got["x-ray-id"] != "abc" {
		t.Errorf("expected lowercased x-ray-id, got %v", got)
	}
}
