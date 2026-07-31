package browser

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Page is the per-backend surface the shared challenge pipeline drives.
//
// The three backends used to carry three near-identical copies of ResolvePage's
// body, which is how geckodriver ended up silently ignoring req.LogHTML and
// per-request DisableMedia: nothing forced it to implement them. Adding a step
// here now forces every backend to answer for it.
//
// Note the name PageUserAgent: the backends already expose UserAgent as part of
// browser.Client, and that method takes the instance mutex. ResolvePage runs
// with that mutex already held, so it must not call it.
type Page interface {
	// SetMediaBlocked applies (or clears) the image/CSS/font block list. It is
	// called on *every* request, with blocked=false when media is allowed —
	// leaving a previous block list in place made disableMedia sticky for the
	// rest of a session.
	SetMediaBlocked(ctx context.Context, blocked bool) error

	Navigate(ctx context.Context, req Request) error
	SetPageCookies(ctx context.Context, rawURL string, cookies []Cookie) error
	Title(ctx context.Context) (string, error)
	SelectorExists(ctx context.Context, selector string) (bool, error)
	HTML(ctx context.Context) (string, error)
	CurrentURL(ctx context.Context) (string, error)
	PageUserAgent(ctx context.Context) (string, error)
	// PageCookies takes the settled URL because the CDP backend needs it to
	// scope the jar; the WebDriver backends ignore it (their /cookie endpoint
	// is already origin-scoped).
	PageCookies(ctx context.Context, currentURL string) ([]Cookie, error)
	Screenshot(ctx context.Context) (string, error)

	ChallengePresent(ctx context.Context) (bool, error)
	SolveChallenge(ctx context.Context) error

	// DocumentReady reports whether the page has finished parsing. It is a bare
	// predicate on purpose: the polling budget is pipeline policy, so the loop
	// lives in awaitDocumentReady and a backend only has to answer the question.
	DocumentReady(ctx context.Context) (bool, error)

	// DocumentResponse reports the real HTTP status and headers of the main
	// document. A backend with no way to obtain them returns a zero value and
	// the pipeline keeps the 200 default.
	DocumentResponse(ctx context.Context, currentURL string) (DocumentResponse, error)

	// ApplyTurnstileToken fills in result.TurnstileToken (and may refresh
	// result.Cookies) when the caller asked for it. Backends differ materially
	// here, so this stays a hook rather than shared code.
	ApplyTurnstileToken(ctx context.Context, req Request, result *ChallengeResolutionResult) error

	PageLogger() Logger
	// LogHTMLConfigured reports the backend's configured log_html, OR'd with
	// the per-request flag by the pipeline.
	LogHTMLConfigured() bool
}

// accessDeniedError is the message the Python original returns, kept verbatim
// for wire compatibility.
const accessDeniedError = "Cloudflare has blocked this request. Probably your IP is banned for this site, check in your web browser."

const (
	// documentReadyBudget caps the wait for a page to finish parsing. It is
	// generous because the alternative — snapshotting a half-parsed document —
	// is silent data loss, while overshooting only costs latency on a page that
	// was going to be slow anyway.
	documentReadyBudget = 10 * time.Second
	documentReadyPoll   = 200 * time.Millisecond
)

// awaitDocumentReady blocks until the page reports itself parsed, the budget
// runs out, or the backend stops answering.
//
// Every exit is silent by design: this only improves what the caller is about
// to read, so a backend that cannot answer is a reason to stop waiting, never a
// reason to fail a request that has otherwise succeeded.
func awaitDocumentReady(ctx context.Context, p Page, logger Logger) {
	deadline := time.Now().Add(documentReadyBudget)
	for time.Now().Before(deadline) {
		ready, err := p.DocumentReady(ctx)
		if err != nil {
			logger.Debug("document readiness probe failed", "err", err)
			return
		}
		if ready {
			return
		}
		if err := SleepContext(ctx, documentReadyPoll); err != nil {
			return
		}
	}
	logger.Debug("page still loading when the pipeline was ready to read it", "waited", documentReadyBudget)
}

// ResolvePage runs the challenge pipeline that all three backends share:
// block media → navigate → (cookies → renavigate) → access-denied check →
// challenge detect/solve → wait → snapshot → turnstile → HTML → screenshot.
//
// Callers hold the browser-level mutex; ResolvePage must not take it again.
func ResolvePage(ctx context.Context, p Page, req Request) (*ChallengeResolutionResult, string, error) {
	logger := p.PageLogger()

	if err := p.SetMediaBlocked(ctx, req.DisableMedia); err != nil {
		logger.Debug("apply media block list failed", "err", err)
	}

	if err := p.Navigate(ctx, req); err != nil {
		return nil, "", fmt.Errorf("navigate: %w", err)
	}

	if len(req.Cookies) > 0 {
		if err := p.SetPageCookies(ctx, req.URL, req.Cookies); err != nil {
			return nil, "", fmt.Errorf("set cookies: %w", err)
		}
		if err := p.Navigate(ctx, req); err != nil {
			return nil, "", fmt.Errorf("reload after cookies: %w", err)
		}
	}

	if req.LogHTML || p.LogHTMLConfigured() {
		if _, err := p.HTML(ctx); err != nil {
			logger.Debug("response html read failed", "err", err)
		}
	}

	title, err := p.Title(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read page title: %w", err)
	}
	for _, accessTitle := range AccessDeniedTitles {
		if strings.HasPrefix(title, accessTitle) {
			return nil, "", fmt.Errorf("%s", accessDeniedError)
		}
	}
	for _, selector := range AccessDeniedSelectors {
		exists, err := p.SelectorExists(ctx, selector)
		if err != nil {
			return nil, "", fmt.Errorf("check access denied selector %q: %w", selector, err)
		}
		if exists {
			return nil, "", fmt.Errorf("%s", accessDeniedError)
		}
	}

	message := "Challenge not detected!"
	challengeFound, err := p.ChallengePresent(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("detect challenge: %w", err)
	}
	if challengeFound {
		if err := p.SolveChallenge(ctx); err != nil {
			return nil, "", fmt.Errorf("solve challenge: %w", err)
		}
		message = "Challenge solved!"
	}

	// Clearing a challenge ends in a navigation to the real page, and the
	// challenge stops being detectable the moment that navigation *starts* — so
	// without this the snapshot below can catch a document that has only got as
	// far as </head>. Measured on a live interactive challenge: 1.4 KB with no
	// <body> against the full 12 KB page.
	//
	// This runs unconditionally rather than only after a solve. Our own Navigate
	// already blocks until the load event, so with no challenge the first probe
	// answers true and costs one round-trip; but a passive challenge that
	// redirects without ChallengePresent ever seeing it lands in exactly the
	// same race, and that path would otherwise stay uncovered.
	awaitDocumentReady(ctx, p, logger)

	// Wait BEFORE reading currentURL/cookies/HTML. For request.post the form
	// submission is driven from a data: URL — the browser is still navigating
	// to the real target when we get here, and the cookie jar is scoped to the
	// current page's origin (data:), so reading it now returns nothing. Letting
	// WaitInSeconds elapse first makes the snapshot reflect the post-navigation
	// state: real URL, populated jar, final HTML. Do not move this.
	if req.WaitInSeconds > 0 {
		if err := SleepContext(ctx, time.Duration(req.WaitInSeconds)*time.Second); err != nil {
			return nil, "", fmt.Errorf("wait after challenge: %w", err)
		}
	}

	currentURL, err := p.CurrentURL(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read current url: %w", err)
	}
	userAgent, err := p.PageUserAgent(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read user agent: %w", err)
	}
	cookies, err := p.PageCookies(ctx, currentURL)
	if err != nil {
		return nil, "", fmt.Errorf("read cookies: %w", err)
	}

	result := &ChallengeResolutionResult{
		URL:       currentURL,
		Status:    200,
		Cookies:   cookies,
		UserAgent: userAgent,
	}
	if docResp, err := p.DocumentResponse(ctx, currentURL); err != nil {
		logger.Debug("read document response headers failed", "err", err)
	} else if docResp.Status > 0 {
		result.Status = docResp.Status
		if len(docResp.Headers) > 0 {
			result.Headers = docResp.Headers
		}
	}

	if err := p.ApplyTurnstileToken(ctx, req, result); err != nil {
		return nil, "", fmt.Errorf("read turnstile token: %w", err)
	}

	if !req.ReturnOnlyCookies {
		htmlDoc, err := p.HTML(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("read response html: %w", err)
		}
		result.Response = htmlDoc
	}

	if req.ReturnScreenshot {
		screenshot, err := p.Screenshot(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("capture screenshot: %w", err)
		}
		result.Screenshot = screenshot
	}

	return result, message, nil
}
