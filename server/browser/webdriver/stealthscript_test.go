package webdriverbackend

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// stealthSource returns the stealth script as it is shipped — read out of the
// Go source so these guards cannot drift from the string that actually runs.
func stealthSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("browser.go")
	if err != nil {
		t.Fatalf("read browser.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "const stealthScript = `")
	if start < 0 {
		t.Fatal("stealthScript not found")
	}
	rest := src[start+len("const stealthScript = `"):]
	end := strings.Index(rest, "`")
	if end < 0 {
		t.Fatal("stealthScript is not terminated")
	}
	body := rest[:end]
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestAStealthGetterNeverReadsThePropertyItDefines is the regression guard for
// a defect that made the browser *more* detectable than an unpatched one.
//
// `safeDefine(navigator, 'languages', () => navigator.languages ...)` installs
// a getter whose body reads the property it is installing, so every read
// recursed until the stack blew. Measured on this build before the fix, a page
// reading navigator.languages got "Maximum call stack size exceeded" — and no
// real browser throws on that property, which an anti-bot reads early.
func TestAStealthGetterNeverReadsThePropertyItDefines(t *testing.T) {
	script := stealthSource(t)
	define := regexp.MustCompile(`safeDefine\(\s*([A-Za-z.]+)\s*,\s*'([A-Za-z]+)'\s*,\s*\(\)\s*=>\s*([^\n]*)`)
	for _, m := range define.FindAllStringSubmatch(script, -1) {
		obj, prop, body := m[1], m[2], m[3]
		if strings.Contains(body, obj+"."+prop) {
			t.Errorf("safeDefine(%s, %q, …) reads %s.%s inside its own getter — every read recurses until the stack overflows", obj, prop, obj, prop)
		}
	}
}

// TestTheLanguagesFallbackOnlyAppliesWhenTheBrowserHasNone pins the rule that
// came out of fixing the above: the first attempt replaced a throw with a lie,
// reporting ['en-US','en'] on a browser launched with --accept-lang=ru-RU and
// sending Accept-Language to match. Contradicting your own request headers is
// exactly what Cloudflare compares, so the override is now conditional.
func TestTheLanguagesFallbackOnlyAppliesWhenTheBrowserHasNone(t *testing.T) {
	script := stealthSource(t)
	at := strings.Index(script, "safeDefine(navigator, 'languages'")
	if at < 0 {
		return // the override is gone entirely, which satisfies this guard
	}
	before := script[max(0, at-320):at]
	if !strings.Contains(before, "if (!nativeLanguages)") {
		t.Error("navigator.languages is overridden unconditionally — a browser that reports a real language list would be made to contradict its own Accept-Language header")
	}
}

// TestClientHintBrandsFollowTheUserAgent keeps sec-ch-ua and the UA on the same
// version. They were written separately and drifted: measured on this build the
// UA said Chrome/146.0.0.0 while the brands said 147, which is the mismatch
// Cloudflare checks and the class of bug this file already records as "the
// entire difference between clearing in ~7s and looping forever".
func TestClientHintBrandsFollowTheUserAgent(t *testing.T) {
	script := stealthSource(t)
	at := strings.Index(script, "const brands = [")
	if at < 0 {
		t.Fatal("the userAgentData brands block is gone — update this guard")
	}
	block := script[at:min(len(script), at+400)]
	if hard := regexp.MustCompile(`brand: '(?:Google Chrome|Chromium)', version: '(\d+)'`).FindStringSubmatch(block); hard != nil {
		t.Errorf("the Chrome major is hardcoded as %q in sec-ch-ua; it has to be read back out of the User-Agent or it drifts on the next browser bump", hard[1])
	}
	if !strings.Contains(script[max(0, at-400):at], "navigator.userAgent") {
		t.Error("the brands version is not derived from navigator.userAgent")
	}
}
