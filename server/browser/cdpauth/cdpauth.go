// Package cdpauth answers proxy authentication challenges over CDP.
//
// Both Chromium backends need a byte-identical handler for this, and CDP is the
// only mechanism that still does the job: W3C WebDriver has no notion of proxy
// credentials, and the MV3 extension the chromedriver backend used to rely on no
// longer loads at all — Chrome removed --load-extension, and then the
// DisableLoadExtensionCommandLineSwitch escape hatch that worked around its
// removal.
package cdpauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	browserpkg "github.com/trinity-aml/flaresolverr-go/server/browser"
)

// replyTimeout bounds each reply to a paused request. With interception on, a
// request nobody answers stalls the navigation until it times out, so a reply
// that cannot be delivered promptly is better abandoned than waited on.
const replyTimeout = 5 * time.Second

// Install registers the auth handler on an already-allocated chromedp context
// and turns on request interception.
//
// The listener goes on before Fetch.enable so no challenge can arrive
// unhandled, and every paused request that is *not* an auth challenge is
// continued untouched — interception pauses everything, so anything left
// unanswered hangs the page.
func Install(ctx context.Context, username, password string, logger browserpkg.Logger) error {
	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *fetch.EventRequestPaused:
			go func() {
				replyCtx, cancel := context.WithTimeout(ctx, replyTimeout)
				defer cancel()
				_ = chromedp.Run(replyCtx, fetch.ContinueRequest(e.RequestID))
			}()
		case *fetch.EventAuthRequired:
			go func() {
				replyCtx, cancel := context.WithTimeout(ctx, replyTimeout)
				defer cancel()
				reply := &fetch.AuthChallengeResponse{Response: fetch.AuthChallengeResponseResponseDefault}
				if e.AuthChallenge != nil && e.AuthChallenge.Source == fetch.AuthChallengeSourceProxy {
					reply = &fetch.AuthChallengeResponse{
						Response: fetch.AuthChallengeResponseResponseProvideCredentials,
						Username: username,
						Password: password,
					}
				}
				// Logged because "the handler never fired" and "the credentials
				// were wrong" look identical from outside: both end as a 407.
				logger.Debug("answering proxy auth challenge", "url", e.Request.URL, "provided", reply.Response)
				_ = chromedp.Run(replyCtx, fetch.ContinueWithAuth(e.RequestID, reply))
			}()
		}
	})

	if err := chromedp.Run(ctx, fetch.Enable().
		WithHandleAuthRequests(true).
		WithPatterns([]*fetch.RequestPattern{{URLPattern: "*"}})); err != nil {
		return fmt.Errorf("enable cdp request interception: %w", err)
	}
	return nil
}

// AttachRemote opens a second CDP connection to a browser this process does not
// own and installs the auth handler on the page that browser already has open.
// It returns a func that detaches.
//
// debuggerAddress is the host:port chromedriver reports as
// goog:chromeOptions.debuggerAddress. Chrome multiplexes debugger sessions, so
// this coexists with chromedriver's own connection.
//
// Three things here are load-bearing:
//
//   - The target is named explicitly with WithTargetID. chromedp forces its
//     "first context" flag off for a RemoteAllocator, so a plain NewContext
//     means Target.createTarget — Fetch.enable would land on a fresh blank tab
//     that nothing ever navigates. That is not hypothetical: it is what the
//     first version of this code did, and the proxy logged three unanswered
//     407s to prove it.
//   - The contexts are rooted at Background, not at a caller's context, which
//     bounds browser *construction*; this listener lives as long as the browser.
//   - Teardown uses the plain cancel funcs and never chromedp.Cancel, which
//     would send Browser.close and kill a browser chromedriver owns. The
//     cancel path does still try to close the tab it attached to, so callers
//     must detach only once the browser is on its way out. (The chromedp
//     backend does the exact opposite for the mirror-image reason: there it
//     *does* own the browser and must wait for the process to exit.)
func AttachRemote(debuggerAddress, username, password string, logger browserpkg.Logger) (context.CancelFunc, error) {
	debuggerAddress = strings.TrimSpace(debuggerAddress)
	if debuggerAddress == "" {
		return nil, fmt.Errorf("no debugger address to attach to")
	}

	targetID, err := pageTargetID(debuggerAddress)
	if err != nil {
		return nil, err
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), "http://"+debuggerAddress)
	cdpCtx, cdpCancel := chromedp.NewContext(allocCtx, chromedp.WithTargetID(targetID))
	detach := func() {
		cdpCancel()
		allocCancel()
	}

	// This first Run performs the attach, and it must be passed cdpCtx itself.
	// The remote allocator registers its teardown goroutine on whatever context
	// it is allocated with, so handing it a context.WithTimeout child would let
	// that timeout later fire chromedp.Cancel, closing the browser out from
	// under chromedriver some seconds after a perfectly good attach.
	if err := chromedp.Run(cdpCtx); err != nil {
		detach()
		return nil, fmt.Errorf("attach cdp at %s: %w", debuggerAddress, err)
	}

	if err := Install(cdpCtx, username, password, logger); err != nil {
		detach()
		return nil, err
	}
	logger.Debug("proxy authentication attached over cdp", "address", debuggerAddress, "target", targetID)
	return detach, nil
}

// pageTargetID returns the browser's first page target.
func pageTargetID(debuggerAddress string) (target.ID, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + debuggerAddress + "/json/list")
	if err != nil {
		return "", fmt.Errorf("list cdp targets at %s: %w", debuggerAddress, err)
	}
	defer resp.Body.Close()

	var targets []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return "", fmt.Errorf("decode cdp target list: %w", err)
	}
	for _, t := range targets {
		if t.Type == "page" && t.ID != "" {
			return target.ID(t.ID), nil
		}
	}
	return "", fmt.Errorf("no page target at %s", debuggerAddress)
}
