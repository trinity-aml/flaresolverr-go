package browser

import (
	"os"
	"strings"
	"testing"
)

// A POSIX locale is not a language tag, and passing one through was producing a
// header no browser can send. Measured against this browser build through a
// local echo server, before the fix:
//
//	--accept-lang=ru_RU.UTF-8  ->  Accept-Language: ru_RU.UTF-8,ru_RU.UTF;q=0.9
//	--accept-lang=ru-RU        ->  Accept-Language: ru-RU,ru;q=0.9
//
// The production box's system locale is ru_RU.UTF-8, so every challenge it has
// ever been served carried the first form.
func TestAPosixLocaleBecomesALanguageTag(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ru_RU.UTF-8", "ru-RU"}, // the production value
		{"en_US.UTF-8", "en-US"},
		{"ru_RU", "ru-RU"},
		{"ru_ru", "ru-RU"}, // region is upper-cased, language lower-cased
		{"RU", "ru"},
		{"de_DE@euro", "de-DE"},      // locale modifier dropped
		{"zh_Hans_CN", "zh-Hans-CN"}, // a 4-letter script subtag is not a region
		{" ru_RU.UTF-8 ", "ru-RU"},
		// Already a header value: reduced to the primary tag, because Chrome
		// appends its own fallback and would emit ru-RU,ru;q=0.9,ru;q=0.9;q=0.8.
		{"ru-RU,ru;q=0.9", "ru-RU"},
		// Nothing usable: the callers have their own fallback, and inventing a
		// language for a host that declined to state one is worse.
		{"", ""},
		{"C", ""},
		{"POSIX", ""},
		{"c", ""},
	} {
		if got := AcceptLanguageFromLocale(tc.in); got != tc.want {
			t.Errorf("AcceptLanguageFromLocale(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The invariant that actually protects the header, stated separately from the
// table because it also closes an injection route: $LANG reaches a Chrome
// command line, so a value carrying a space or a dash could add a flag.
func TestTheTagNeverCarriesLocaleOrHeaderSyntax(t *testing.T) {
	for _, in := range []string{
		"ru_RU.UTF-8", "en_US.iso88591", "de_DE@euro", "ru-RU,ru;q=0.9",
		"ru_RU.UTF-8 --disable-web-security", "../../etc/passwd",
		"--headless", "ru;q=0.9", "'; rm -rf /", "ru_RU.UTF-8\nen_US",
	} {
		got := AcceptLanguageFromLocale(in)
		if got == "" {
			continue // rejected outright, which is the safe answer
		}
		if strings.ContainsAny(got, "_.@,; \t\n\"'/\\") {
			t.Errorf("AcceptLanguageFromLocale(%q) = %q, which still carries locale, header or shell syntax", in, got)
		}
		if strings.HasPrefix(got, "-") {
			t.Errorf("AcceptLanguageFromLocale(%q) = %q, which Chrome would read as another flag", in, got)
		}
	}
}

func TestTheFallbackIsUsedOnlyWhenTheLocaleSaysNothing(t *testing.T) {
	t.Setenv("LANG", "ru_RU.UTF-8")
	if got := BrowserAcceptLanguage("en-US"); got != "ru-RU" {
		t.Errorf("BrowserAcceptLanguage = %q, want ru-RU — the host's own locale must win", got)
	}
	t.Setenv("LANG", "C")
	if got := BrowserAcceptLanguage("en-US"); got != "en-US" {
		t.Errorf("BrowserAcceptLanguage = %q, want the en-US fallback", got)
	}
	os.Unsetenv("LANG")
	if got := BrowserAcceptLanguage("en-US"); got != "en-US" {
		t.Errorf("BrowserAcceptLanguage = %q with no LANG at all, want the en-US fallback", got)
	}
}

// Both backends, and both the command line and the CDP override, must go
// through the helper. The CDP one is the easier to forget and the more
// damaging: `acceptLanguage` on setUserAgentOverride is also what
// `navigator.language` and `navigator.languages` report to the page, so a raw
// locale there is readable by the challenge in one line of JavaScript.
func TestNoBackendPassesTheRawLocaleToTheBrowser(t *testing.T) {
	for _, path := range []string{
		"webdriver/browser.go",
		"chromedp/browser.go",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if !strings.Contains(line, `os.Getenv("LANG")`) {
				continue
			}
			if !strings.Contains(line, "AcceptLanguageFromLocale") &&
				!strings.Contains(line, "BrowserAcceptLanguage") {
				t.Errorf("%s hands the raw $LANG to the browser: %s", path, strings.TrimSpace(line))
			}
		}
	}
}
