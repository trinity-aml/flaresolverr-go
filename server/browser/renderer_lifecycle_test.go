package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Chrome's zygote reaps renderer processes when their page goes away. With
// --no-zygote each renderer is forked straight off the browser process and
// nothing retires it.
//
// Measured on the production box 2026-11-14 inside one 57-minute session: 77
// live --type=renderer processes at ~230 MB each, zero --type=zygote, all
// parented to the browser main process, a new one every ~23 s and no exits.
// The service cgroup's anonymous memory went 589 MB -> 6723 MB in an hour
// while the Go runtime's own sys stayed flat — all of it in child processes.
//
// Both backends carried the flag since the initial commit, so this guards both
// and names the consequence, because "restore --no-zygote" looks harmless.
func TestNeitherBackendDisablesTheZygote(t *testing.T) {
	for _, f := range []string{"webdriver/browser.go", "chromedp/browser.go"} {
		src, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		body := string(src)
		// The explanation mentions the flag, so look for it being *set* rather
		// than merely named: a code line, not a comment.
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(trimmed, "no-zygote") {
				t.Errorf("%s sets --no-zygote again (%q) — renderers will stop being reaped and the session will accumulate one per site instance for its whole life", f, trimmed)
			}
		}
	}
}

// The ceiling is the second half and stands on its own: it bounds the damage
// if a future Chrome changes how renderers are retired.
func TestBothBackendsCapTheRendererCount(t *testing.T) {
	for _, f := range []string{"webdriver/browser.go", "chromedp/browser.go"} {
		src, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(src), "renderer-process-limit") {
			t.Errorf("%s no longer caps renderer processes", f)
		}
	}
}
