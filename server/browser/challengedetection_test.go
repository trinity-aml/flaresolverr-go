package browser

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The two documents below are the distinguishing markup of one real pair,
// captured 2026-11-15 from https://bitru.org/browse.php?tmp=movie&page=430 in
// this browser build: the Cloudflare interstitial it serves to a ru-RU browser
// (HTTP 403, 44 863 B) and the listing the same URL serves once the challenge
// clears (HTTP 200, 124 739 B). The challenge token in _cf_chl_opt is
// truncated — only the name of the global matters here.
const russianInterstitial = `<!DOCTYPE html><html lang="ru-RU"><head><title>Один момент…</title>
<script>window._cf_chl_opt = {cFPWv: 'b', cH: 'TRUNCATED'};</script></head>
<body class="no-js"><div class="main-wrapper" role="main"><div class="main-content">
<h2 class="h2"><span id="challenge-error-text">Выполнение проверки безопасности</span></h2>
<input type="hidden" name="cf-turnstile-response" id="cf-chl-widget-cybag_response">
<div id="cf-chl-widget-cybag_wrapper"></div></div>
<div class="footer" role="contentinfo"><div class="ray-id">Ray ID: <code>a5b04f91</code></div>
<script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1?ray=a5b04f91"></script>
</div></body></html>`

const clearedListing = `<html lang="ru" dir="ltr"><head><meta charset="utf-8">
<title>Торренты | Фильмы | Стр. 430 - bitru.org</title></head>
<body><div id="logo"></div><a href="details.php?id=123">раздача</a>
<script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js?ray=a5b05388"></script>
</body></html>`

// TestAChallengeIsRecognisedWithoutReadingItsTitle is the regression guard for
// the 2026-11-15 bitru outage: Cloudflare serves the interstitial in the
// browser's own language, so with a correct `--accept-lang=ru-RU` the title
// came back "Один момент…", no marker matched, the challenge was never solved
// and the 403 interstitial was returned to the caller as the answer. Measured
// on one box within one minute, same URL, same browser:
//
//	en-US  "Торренты | Фильмы | Стр. 430"  Challenge solved!        200
//	ru-RU  "Один момент…"                  Challenge not detected!  403
//
// So the title is removed from the inputs here deliberately. Detection has to
// stand up without it, or the next locale costs another outage.
func TestAChallengeIsRecognisedWithoutReadingItsTitle(t *testing.T) {
	if titleMatches(russianInterstitial) {
		t.Fatal("the fixture's title is in ChallengeTitles, so this test proves nothing — pick a locale that is not listed")
	}
	if !markupMatches(russianInterstitial) && !runtimeMarkerPresent(russianInterstitial) {
		t.Error("a Cloudflare interstitial in a non-English locale is not detected as a challenge")
	}
}

// TestNoMarkerSurvivesTheSolve pins the other half, which is easy to break by
// reaching for a marker that is merely *common* on Cloudflare pages.
// challengePresent is also the loop condition in solveChallenge, so a marker
// that is still there once the challenge has cleared turns that loop into one
// that cannot exit.
func TestNoMarkerSurvivesTheSolve(t *testing.T) {
	if titleMatches(clearedListing) || markupMatches(clearedListing) || runtimeMarkerPresent(clearedListing) {
		t.Error("the page served after the challenge cleared is detected as a challenge; solveChallenge would never finish")
	}
	// The cleared page carries /cdn-cgi/challenge-platform/scripts/jsd/main.js:
	// Cloudflare injects that on every proxied page. Only the challenge page
	// carries orchestrate/chl_page.
	for _, selector := range ChallengeSelectors {
		if strings.Contains(selector, "challenge-platform") {
			t.Errorf("ChallengeSelectors matches %q, which is present on pages with no challenge", selector)
		}
	}
}

// TestEveryBackendConsultsTheChallengeRuntimeMarker keeps the three backends
// together. Each has its own challengePresent and each aliases the shared
// lists, so a marker added to this package reaches all three — but the runtime
// probe is a call, and a call is the kind of thing one backend gets and the
// others do not.
func TestEveryBackendConsultsTheChallengeRuntimeMarker(t *testing.T) {
	for _, path := range []string{
		"webdriver/browser.go",
		"geckodriver/browser.go",
		"chromedp/browser.go",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := challengePresentBody(string(raw))
		if body == "" {
			t.Fatalf("%s: no challengePresent found", path)
		}
		if !strings.Contains(body, "ChallengeMarkerScript") {
			t.Errorf("%s: challengePresent does not consult ChallengeMarkerScript, so this backend still detects a challenge by its language", path)
		}
	}
}

// TestTheRuntimeMarkerIsTheChallengeGlobal states what the probe is allowed to
// rely on. `window._cf_chl_opt` is the challenge script's own config object:
// not translated, not obfuscated, and absent once the challenge clears.
func TestTheRuntimeMarkerIsTheChallengeGlobal(t *testing.T) {
	if !strings.Contains(ChallengeMarkerScript, "_cf_chl_opt") {
		t.Error("ChallengeMarkerScript no longer probes window._cf_chl_opt")
	}
}

// challengePresentBody returns the body of the backend's challengePresent with
// comment lines stripped — the explanation above the probe names the script,
// so a scan that kept comments would pass with the call deleted.
func challengePresentBody(src string) string {
	start := strings.Index(src, ") challengePresent(ctx context.Context) (bool, error) {")
	if start < 0 {
		return ""
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return ""
	}
	var kept []string
	for _, line := range strings.Split(rest[:end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func titleMatches(doc string) bool {
	m := regexp.MustCompile(`(?s)<title[^>]*>(.*?)</title>`).FindStringSubmatch(doc)
	if m == nil {
		return false
	}
	for _, title := range ChallengeTitles {
		if strings.EqualFold(strings.TrimSpace(m[1]), title) {
			return true
		}
	}
	return false
}

// markupMatches applies the selectors this test can evaluate against raw
// markup: an id, a class, and an attribute match on a single element. The
// compound selectors in the list target other vendors' pages and are skipped
// rather than approximated.
func markupMatches(doc string) bool {
	attr := regexp.MustCompile(`^([a-z]+)\[([a-z-]+)='([^']+)'\]$`)
	for _, selector := range ChallengeSelectors {
		switch {
		case attr.MatchString(selector):
			m := attr.FindStringSubmatch(selector)
			if regexp.MustCompile(`<` + m[1] + `[^>]*\b` + m[2] + `="` + regexp.QuoteMeta(m[3]) + `"`).MatchString(doc) {
				return true
			}
		case strings.HasPrefix(selector, "#") && !strings.ContainsAny(selector, " ."):
			if strings.Contains(doc, `id="`+selector[1:]+`"`) {
				return true
			}
		case strings.HasPrefix(selector, ".") && !strings.ContainsAny(selector, " #"):
			if regexp.MustCompile(`class="[^"]*\b` + regexp.QuoteMeta(selector[1:]) + `\b`).MatchString(doc) {
				return true
			}
		}
	}
	return false
}

// runtimeMarkerPresent reads the global out of ChallengeMarkerScript itself
// rather than restating it, so swapping the probe for something a translated
// page does not carry makes this fail instead of passing on a stale copy.
func runtimeMarkerPresent(doc string) bool {
	m := regexp.MustCompile(`window\.(_?[A-Za-z0-9_]+)`).FindStringSubmatch(ChallengeMarkerScript)
	if m == nil {
		return false
	}
	return strings.Contains(doc, m[1])
}
