package w3c

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The context a command carries must be the deadline that governs it, and the
// HTTP client's own timeout must only ever be a last resort.
//
// Both backends used to hardcode a 30-second client timeout, which is shorter
// than any maxTimeout a caller sets, so it fired first and made the caller's
// budget unreachable. Measured on a production jacred box: 665 "the browser
// stopped responding" failures in 24 hours, with a spike at exactly 30.0
// seconds — the signature of this timeout firing on a solve's first command.
func TestCommandBackstopStaysAboveAnyRealBudget(t *testing.T) {
	// The largest maxTimeout jacred sets today is 90s. The backstop has to sit
	// comfortably clear of it, or it starts pre-empting real budgets again.
	const largestKnownMaxTimeout = 90 * time.Second
	if CommandBackstop < 3*largestKnownMaxTimeout {
		t.Errorf("CommandBackstop = %s, too close to the %s a client already asks for: "+
			"it would pre-empt the deadline the caller configured",
			CommandBackstop, largestKnownMaxTimeout)
	}
	if NewHTTPClient().Timeout != CommandBackstop {
		t.Errorf("NewHTTPClient().Timeout = %s, want the backstop %s", NewHTTPClient().Timeout, CommandBackstop)
	}
}

// Loosening the client timeout is only safe because every command carries the
// caller's context. If that stops being true, a command with no deadline could
// hang for the whole backstop.
func TestEveryCommandCarriesTheCallersContext(t *testing.T) {
	src, err := os.ReadFile("session.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "http.NewRequestWithContext(ctx,") {
		t.Error("Session.Do no longer builds its request with the caller's context, " +
			"so the backstop would become the only deadline a command has")
	}
}

// Neither backend may go back to constructing its own short-lived client.
func TestBackendsUseTheSharedClient(t *testing.T) {
	for _, path := range []string{
		"../webdriver/browser.go",
		"../geckodriver/browser.go",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(src)
		if strings.Contains(body, "http.Client{Timeout:") {
			t.Errorf("%s builds its own timed HTTP client again instead of w3c.NewHTTPClient(); "+
				"that is how a 30-second limit shadowed a 90-second maxTimeout", path)
		}
		if !strings.Contains(body, "w3c.NewHTTPClient()") {
			t.Errorf("%s no longer uses the shared client constructor", path)
		}
	}
}
