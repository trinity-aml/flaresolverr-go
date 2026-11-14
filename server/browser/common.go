package browser

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var headlessUserAgentRE = regexp.MustCompile(`(?i)HEADLESS`)

var transientDirRootMu sync.Mutex
var transientDirRoot string

var AccessDeniedTitles = []string{
	"Access denied",
	"Attention Required! | Cloudflare",
}

var AccessDeniedSelectors = []string{
	"div.cf-error-title span.cf-code-label span",
	"#cf-error-details div.cf-error-overview h1",
}

var ChallengeTitles = []string{
	"Just a moment...",
	"DDoS-Guard",
}

var ChallengeSelectors = []string{
	"#cf-challenge-running",
	".ray_id",
	".attack-box",
	"#cf-please-wait",
	"#challenge-spinner",
	"#trk_jschal_js",
	"#turnstile-wrapper",
	".lds-ring",
	"td.info #js_info",
	"div.vc div.text-box h2",
}

var TurnstileSelectors = []string{
	"input[name='cf-turnstile-response']",
}

var BlockedURLs = []string{
	"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp", "*.bmp", "*.svg", "*.ico",
	"*.PNG", "*.JPG", "*.JPEG", "*.GIF", "*.WEBP", "*.BMP", "*.SVG", "*.ICO",
	"*.tiff", "*.tif", "*.jpe", "*.apng", "*.avif", "*.heic", "*.heif",
	"*.TIFF", "*.TIF", "*.JPE", "*.APNG", "*.AVIF", "*.HEIC", "*.HEIF",
	"*.css", "*.CSS",
	"*.woff", "*.woff2", "*.ttf", "*.otf", "*.eot",
	"*.WOFF", "*.WOFF2", "*.TTF", "*.OTF", "*.EOT",
}

type ClickTarget struct {
	Kind      string `json:"kind"`
	Tag       string `json:"tag"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	AriaLabel string `json:"ariaLabel"`
	Role      string `json:"role"`
	// AriaChecked is the widget's own answer to "has this already been
	// clicked". Turnstile sets aria-checked="true" on its checkbox the moment
	// the click lands, well before the page navigates away, so re-clicking in
	// the meantime is at best wasted and at worst reads as a double-click.
	AriaChecked string  `json:"ariaChecked"`
	ClassName   string  `json:"className"`
	TabIndex    int64   `json:"tabIndex"`
	Disabled    bool    `json:"disabled"`
	Src         string  `json:"src"`
	Left        float64 `json:"left"`
	Top         float64 `json:"top"`
	Width       float64 `json:"width"`
	Height      float64 `json:"height"`
	Visible     bool    `json:"visible"`
}

type Point struct {
	X float64
	Y float64
}

type DocumentResponse struct {
	URL     string
	Status  int
	Headers map[string]string
}

// ProxySchemes are the proxy URL schemes the backends can actually honour.
var ProxySchemes = []string{"http", "https", "socks4", "socks5"}

// ParseProxyURL validates a proxy URL and splits it into scheme and host:port.
//
// A URL with no scheme is accepted as http: that is what Chrome's
// --proxy-server does with a bare host:port, and what people write in config
// files. geckodriver used to reject that form and fall back to a direct
// connection, so the two engines disagreed on the same setting.
func ParseProxyURL(raw string) (scheme, hostPort string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("proxy url is empty")
	}

	// url.Parse would read the host of a bare "host:port" as the scheme.
	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "http://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return "", "", fmt.Errorf("proxy url %q is not a valid URL: %w", raw, err)
	}

	scheme = strings.ToLower(parsed.Scheme)
	if !slices.Contains(ProxySchemes, scheme) {
		return "", "", fmt.Errorf("proxy url %q uses unsupported scheme %q, expected one of %s",
			raw, scheme, strings.Join(ProxySchemes, ", "))
	}

	host, port := parsed.Hostname(), parsed.Port()
	if host == "" || port == "" {
		return "", "", fmt.Errorf("proxy url %q must include both host and port", raw)
	}
	return scheme, host + ":" + port, nil
}

// ValidateProxy reports whether a proxy configuration is one the backends can
// honour. A nil or empty proxy is valid — it means "no proxy".
//
// Callers are expected to treat a failure as fatal. An unusable proxy used to
// be non-fatal everywhere: geckodriver logged "ignoring proxy", and the Chromium
// backends handed the malformed string to --proxy-server, which Chrome quietly
// drops. All three then sent the request over the real IP — the one outcome
// somebody who configured a proxy would least want, announced only by a log
// line they were never going to read. Failing is visible and recoverable; a
// leaked IP is neither.
func ValidateProxy(p *Proxy) error {
	if p == nil || strings.TrimSpace(p.URL) == "" {
		return nil
	}
	_, _, err := ParseProxyURL(p.URL)
	return err
}

func AppendWithEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	replaced := false
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			if !replaced {
				result = append(result, prefix+value)
				replaced = true
			}
			continue
		}
		result = append(result, item)
	}
	if !replaced {
		result = append(result, prefix+value)
	}
	return result
}

// BuildPostFormHTML wraps a form-encoded postData into an auto-submitting
// HTML form so a WebDriver-driven browser can replay it as a real POST.
//
// HTML form inputs hold plain text in name/value attributes — the browser
// percent-encodes them itself when serializing the submission. Applying
// url.QueryEscape here as well produced a double-encoded body (e.g. a
// password "P@ss" arrived at the server as "P%2540ss"), silently failing
// every login with a non-alphanumeric character. We only need to escape
// the HTML metacharacters (< > & " ') so the attributes parse correctly.
func BuildPostFormHTML(targetURL, postData string) string {
	queryString := strings.TrimPrefix(postData, "?")
	pairs, _ := url.ParseQuery(queryString)

	var builder strings.Builder
	builder.WriteString(`<!DOCTYPE html><html><body><form id="hackForm" action="`)
	builder.WriteString(html.EscapeString(targetURL))
	builder.WriteString(`" method="POST">`)
	for name, values := range pairs {
		if name == "submit" {
			continue
		}
		for _, value := range values {
			builder.WriteString(`<input type="text" name="`)
			builder.WriteString(html.EscapeString(name))
			builder.WriteString(`" value="`)
			builder.WriteString(html.EscapeString(value))
			builder.WriteString(`"><br>`)
		}
	}
	builder.WriteString(`</form><script>document.getElementById("hackForm").submit();</script></body></html>`)
	return builder.String()
}

func NormalizeBlockedPattern(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return pattern
	}
	if strings.Contains(pattern, "://") {
		return pattern
	}
	if strings.HasPrefix(pattern, "*") {
		return "*://*:*/*" + strings.TrimPrefix(pattern, "*")
	}
	return "*://*:*/*" + pattern
}

func IsVerifyButtonTarget(target ClickTarget) bool {
	if !target.Visible {
		return false
	}

	haystack := strings.ToLower(strings.Join([]string{
		target.Text,
		target.AriaLabel,
		target.Title,
		target.Name,
		target.ID,
	}, " "))

	if strings.Contains(haystack, "verify you are human") {
		return true
	}
	return strings.Contains(haystack, "verify") && strings.Contains(haystack, "human")
}

func IsChallengeIframeTarget(target ClickTarget) bool {
	if target.Kind != "iframe" || !target.Visible {
		return false
	}

	haystack := strings.ToLower(strings.Join([]string{
		target.Src,
		target.Title,
		target.Name,
		target.ID,
		target.AriaLabel,
	}, " "))

	for _, needle := range []string{"cloudflare", "challenge", "turnstile", "cf-chl", "captcha", "widget"} {
		if strings.Contains(haystack, needle) {
			return true
		}
	}

	return target.Width >= 240 && target.Width <= 420 && target.Height >= 40 && target.Height <= 120
}

// IsTurnstileCheckboxTarget identifies the one element a Turnstile challenge
// actually wants clicked: its <input type="checkbox">.
//
// Naming the element instead of describing it is the whole point. The old code
// had no checkbox case at all, so the widget was reached only by heuristics —
// "a button-ish thing between 40 and 500 pixels wide", "an element whose text
// says verify you are human" — and a 24x24 checkbox with no text matches
// neither. Meanwhile the interstitial's Cloudflare branding link did match the
// tabbable heuristic, so on the production box every failed solve opened three
// tabs on www.cloudflare.com and never touched the widget.
//
// Deliberately no size bound: the checkbox is small by construction, and the
// tag plus the type is already an exact identification.
func IsTurnstileCheckboxTarget(target ClickTarget) bool {
	if !target.Visible || target.Disabled {
		return false
	}
	if target.Width <= 0 || target.Height <= 0 {
		return false
	}
	return target.Tag == "input" && strings.EqualFold(strings.TrimSpace(target.Type), "checkbox")
}

// TargetAlreadyChecked reports that the widget says it has been clicked, so
// clicking it again would be pure noise.
func TargetAlreadyChecked(target ClickTarget) bool {
	return strings.EqualFold(strings.TrimSpace(target.AriaChecked), "true")
}

// RelevantChallengeTargets returns the targets worth clicking, most specific
// first: the checkbox itself, then an explicit verify button, then the
// challenge iframe. Order is the contract here — the caller clicks in sequence
// and stops as soon as the challenge clears, so a precise target that comes
// after a vague one is a target that mostly never gets tried.
func RelevantChallengeTargets(targets []ClickTarget) []ClickTarget {
	checkboxes := make([]ClickTarget, 0, 2)
	others := make([]ClickTarget, 0, len(targets))
	for _, target := range targets {
		switch {
		case IsTurnstileCheckboxTarget(target):
			checkboxes = append(checkboxes, target)
		case IsVerifyButtonTarget(target) || IsChallengeIframeTarget(target):
			others = append(others, target)
		}
	}
	return append(checkboxes, others...)
}

func FallbackChallengeTargets(targets []ClickTarget) []ClickTarget {
	candidates := make([]ClickTarget, 0, len(targets))
	for _, target := range targets {
		if !target.Visible || target.ID == "__flaresolverr-focus" {
			continue
		}
		switch target.Kind {
		case "iframe":
			if target.Width >= 20 && target.Width <= 900 && target.Height >= 20 && target.Height <= 400 {
				candidates = append(candidates, target)
			}
		case "button", "input", "role_button":
			// The checkbox is exempt from the size window: it is identified
			// exactly by tag+type, and at ~24x24 it fails the 40px floor that
			// the window uses to reject page furniture.
			if IsTurnstileCheckboxTarget(target) {
				candidates = append(candidates, target)
				continue
			}
			if target.Width >= 40 && target.Width <= 500 && target.Height >= 20 && target.Height <= 180 {
				candidates = append(candidates, target)
			}
		}
	}
	return candidates
}

// TabbableTargets returns the elements a Tab key could reach, for the
// last-resort step that clicks whichever one the challenge probably means.
//
// Anchors are excluded, and that exclusion is load-bearing rather than tidy.
// A bare Cloudflare interstitial contains exactly one link — the "Cloudflare"
// branding link, which opens www.cloudflare.com in a new tab — and this
// function used to admit it twice over: through a `case "a"` that asked only
// whether Src was empty (Src is populated for iframes only, so every anchor
// passed), and through the TabIndex check above it, because the DOM reports
// tabIndex 0 for any focusable anchor. Removing the first alone would have
// changed nothing.
//
// Measured on the production box 2026-11-14: every failed solve logged
// `closed stray browser tabs the page opened closed=3` — three tabs on
// www.cloudflare.com per 90-second attempt — while rutracker, kinozal and
// bitru reported zero successful solves for most of the day.
//
// An anchor that genuinely acts as a control still qualifies through the role
// check, so no real button is lost by this.
func TabbableTargets(targets []ClickTarget) []ClickTarget {
	candidates := make([]ClickTarget, 0, len(targets))
	for _, target := range targets {
		if !target.Visible || target.Disabled || target.ID == "__flaresolverr-focus" {
			continue
		}
		if target.Width <= 0 || target.Height <= 0 {
			continue
		}
		if target.Role == "button" {
			candidates = append(candidates, target)
			continue
		}
		// Checked before everything below, including TabIndex.
		if target.Tag == "a" || target.Kind == "anchor" {
			continue
		}
		if target.TabIndex >= 0 {
			candidates = append(candidates, target)
			continue
		}
		switch target.Tag {
		case "iframe", "button", "input", "textarea", "select":
			candidates = append(candidates, target)
		}
	}
	return dedupeTargets(candidates)
}

func dedupeTargets(targets []ClickTarget) []ClickTarget {
	if len(targets) < 2 {
		return targets
	}
	seen := make(map[string]struct{}, len(targets))
	unique := make([]ClickTarget, 0, len(targets))
	for _, target := range targets {
		key := fmt.Sprintf("%s|%s|%s|%s|%.1f|%.1f|%.1f|%.1f", target.Kind, target.Tag, target.ID, target.Name, target.Left, target.Top, target.Width, target.Height)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, target)
	}
	return unique
}

func SummarizeCandidateTargets(targets []ClickTarget) []string {
	if len(targets) == 0 {
		return nil
	}

	limit := min(len(targets), 6)
	summary := make([]string, 0, limit)
	for _, target := range targets[:limit] {
		summary = append(summary, SummarizeClickTarget(target))
	}
	return summary
}

// ChallengeAutoWaitDuration is how long to idle between click-ladder attempts.
// Passive challenges (no interactive controls found) get the longer wait — the
// page is expected to clear itself; an interactive one is retried sooner.
func ChallengeAutoWaitDuration(relevant []ClickTarget) time.Duration {
	if len(relevant) == 0 {
		return 1500 * time.Millisecond
	}
	return time.Second
}

func ClickPointsForTarget(target ClickTarget) []Point {
	left := target.Left
	top := target.Top
	width := target.Width
	height := target.Height
	center := Point{X: left + width/2, Y: top + height/2}

	if target.Kind != "iframe" {
		return []Point{
			center,
			{X: center.X + min(width*0.05, 8), Y: center.Y + min(height*0.05, 6)},
		}
	}

	leftBias := left + min(max(width*0.2, 18), 42)
	verticalCenter := top + height/2
	return []Point{
		{X: leftBias, Y: verticalCenter},
		center,
		{X: left + min(max(width*0.33, 28), max(width-10, 10)), Y: top + min(max(height*0.5, 18), max(height-6, 6))},
	}
}

func SummarizeClickTarget(target ClickTarget) string {
	return fmt.Sprintf("%s/%s text=%q role=%q title=%q src=%q tabIndex=%d rect=(%.1f,%.1f %.1fx%.1f)", target.Kind, target.Tag, target.Text, target.Role, target.Title, target.Src, target.TabIndex, target.Left, target.Top, target.Width, target.Height)
}

func ChromeArgValue(raw, name string) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return ""
	}

	flagPrefix := "--" + strings.TrimSpace(name)
	for i := 0; i < len(fields); i++ {
		field := strings.TrimSpace(fields[i])
		if field == "" {
			continue
		}
		if value, ok := strings.CutPrefix(field, flagPrefix+"="); ok {
			return strings.TrimSpace(value)
		}
		if field == flagPrefix && i+1 < len(fields) {
			return strings.TrimSpace(fields[i+1])
		}
	}
	return ""
}

func NormalizeResponseHeaders(headers map[string]any) map[string]string {
	if len(headers) == 0 {
		return nil
	}

	result := make(map[string]string, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if normalized := normalizeHeaderValue(value); normalized != "" {
			result[strings.ToLower(key)] = normalized
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func URLsEquivalent(lhs, rhs string) bool {
	left := canonicalizeURL(lhs)
	right := canonicalizeURL(rhs)
	if left == "" || right == "" {
		return strings.TrimSpace(lhs) == strings.TrimSpace(rhs)
	}
	return left == right
}

func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func FirstCookiePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "/"
	}
	return path
}

func ScrubUserAgent(value string) string {
	return strings.TrimSpace(headlessUserAgentRE.ReplaceAllString(value, ""))
}

func SleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// XvfbProcess owns a running Xvfb and the single goroutine waiting on it.
//
// Both start strategies below spawn a goroutine blocked in cmd.Wait() to detect
// an early exit, and that goroutine is still running when they hand the process
// back. Callers must therefore never call Wait on the process themselves — two
// concurrent waits race for the same wait4, one gets ECHILD, and the child can
// be left unreaped. Stop is the only supported teardown.
type XvfbProcess struct {
	cmd    *exec.Cmd
	exitCh <-chan error
}

// Stop kills Xvfb and reaps it through the goroutine that already owns Wait.
func (p *XvfbProcess) Stop() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Kill()
	if p.exitCh == nil {
		_, err := p.cmd.Process.Wait()
		return err
	}
	return <-p.exitCh
}

func StartXvfb(ctx context.Context, xvfbPath string) (*XvfbProcess, string, error) {
	if proc, display, err := startXvfbWithDisplayFD(ctx, xvfbPath); err == nil {
		return proc, display, nil
	}

	return startXvfbWithRange(ctx, xvfbPath)
}

func CreateTransientDir(prefix string) (string, error) {
	// Before making a new one, drop the ones whose owner died. This is the
	// only creation point for every transient directory the library makes, so
	// hanging the sweep here means no backend and no caller can forget it.
	// See transientsweep.go for why a teardown fix would not be enough.
	transientSweepOnce.Do(func() {
		if removed, freed := SweepStaleTransientDirs(); removed > 0 {
			transientSweepReport = fmt.Sprintf("removed %d stale transient dir(s), freed %.1f MB", removed, float64(freed)/(1<<20))
		}
	})

	if dir, err := createTransientDirInPreferredRoot(prefix); err == nil {
		writeTransientOwner(dir)
		return dir, nil
	}

	for _, root := range transientDirRoots() {
		if strings.TrimSpace(root) == "" {
			continue
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			continue
		}
		dir, err := os.MkdirTemp(root, prefix)
		if err == nil {
			transientDirRootMu.Lock()
			transientDirRoot = root
			transientDirRootMu.Unlock()
			writeTransientOwner(dir)
			return dir, nil
		}
	}
	dir, err := os.MkdirTemp("", prefix)
	if err == nil {
		writeTransientOwner(dir)
	}
	return dir, err
}

// transientSweepReport holds what the startup sweep did, so a caller can log
// it with its own logger rather than this package reaching for a global one.
var transientSweepReport string

// TransientSweepReport returns a one-line summary of the stale-directory
// sweep, or "" when it removed nothing. Read it after the first browser
// session is created.
func TransientSweepReport() string { return transientSweepReport }

func createTransientDirInPreferredRoot(prefix string) (string, error) {
	transientDirRootMu.Lock()
	root := transientDirRoot
	transientDirRootMu.Unlock()
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("preferred transient dir root is not set")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		transientDirRootMu.Lock()
		if transientDirRoot == root {
			transientDirRoot = ""
		}
		transientDirRootMu.Unlock()
		return "", err
	}
	dir, err := os.MkdirTemp(root, prefix)
	if err == nil {
		return dir, nil
	}
	transientDirRootMu.Lock()
	if transientDirRoot == root {
		transientDirRoot = ""
	}
	transientDirRootMu.Unlock()
	return "", err
}

func normalizeHeaderValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case []string:
		return strings.Join(typed, ", ")
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if normalized := normalizeHeaderValue(item); normalized != "" {
				parts = append(parts, normalized)
			}
		}
		return strings.Join(parts, ", ")
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func canonicalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	parsed.Fragment = ""
	parsed.Host = strings.ToLower(parsed.Host)
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	if parsed.Path != "/" {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		if parsed.Path == "" {
			parsed.Path = "/"
		}
	}
	return parsed.String()
}

func startXvfbWithDisplayFD(ctx context.Context, xvfbPath string) (*XvfbProcess, string, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, "", fmt.Errorf("create Xvfb pipe: %w", err)
	}
	defer reader.Close()

	var stderr bytes.Buffer
	cmd := exec.Command(xvfbPath, "-displayfd", "3", "-screen", "0", "1920x1080x24", "-nolisten", "tcp")
	cmd.Stdout = nil
	cmd.Stderr = &stderr
	cmd.ExtraFiles = []*os.File{writer}

	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		return nil, "", fmt.Errorf("start Xvfb with -displayfd: %w", err)
	}
	_ = writer.Close()

	exitCh := make(chan error, 1)
	go func() {
		exitCh <- cmd.Wait()
	}()

	displayCh := make(chan string, 1)
	readErrCh := make(chan error, 1)
	go func() {
		data, readErr := io.ReadAll(reader)
		if readErr != nil {
			readErrCh <- readErr
			return
		}

		value := strings.TrimSpace(string(data))
		if value == "" {
			readErrCh <- fmt.Errorf("Xvfb did not report a display number")
			return
		}
		displayNumber, convErr := strconv.Atoi(value)
		if convErr != nil {
			readErrCh <- fmt.Errorf("invalid Xvfb display number %q", value)
			return
		}
		displayCh <- fmt.Sprintf(":%d", displayNumber)
	}()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	select {
	case display := <-displayCh:
		return &XvfbProcess{cmd: cmd, exitCh: exitCh}, display, nil
	case readErr := <-readErrCh:
		_ = cmd.Process.Kill()
		<-exitCh
		return nil, "", fmt.Errorf("start Xvfb with -displayfd: %w%s", readErr, formatXvfbStderr(stderr.String()))
	case waitErr := <-exitCh:
		return nil, "", fmt.Errorf("start Xvfb with -displayfd: %w%s", waitErr, formatXvfbStderr(stderr.String()))
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-exitCh
		return nil, "", fmt.Errorf("start Xvfb with -displayfd: timeout waiting for display%s", formatXvfbStderr(stderr.String()))
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-exitCh
		return nil, "", fmt.Errorf("start Xvfb with -displayfd: %w", ctx.Err())
	}
}

func startXvfbWithRange(ctx context.Context, xvfbPath string) (*XvfbProcess, string, error) {
	var lastErr error

	for displayNumber := 99; displayNumber < 200; displayNumber++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, "", fmt.Errorf("start Xvfb: %w (last error: %v)", err, lastErr)
			}
			return nil, "", fmt.Errorf("start Xvfb: %w", err)
		}
		lockPath := fmt.Sprintf("/tmp/.X%d-lock", displayNumber)
		socketPath := fmt.Sprintf("/tmp/.X11-unix/X%d", displayNumber)
		if _, err := os.Stat(socketPath); err == nil {
			continue
		}
		if _, err := os.Stat(lockPath); err == nil {
			continue
		}

		display := fmt.Sprintf(":%d", displayNumber)
		var stderr bytes.Buffer
		cmd := exec.Command(xvfbPath, display, "-screen", "0", "1920x1080x24", "-nolisten", "tcp")
		cmd.Stdout = nil
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			lastErr = err
			continue
		}

		exitCh := make(chan error, 1)
		go func() {
			exitCh <- cmd.Wait()
		}()

		failed := false
		for range 50 {
			select {
			case waitErr := <-exitCh:
				lastErr = fmt.Errorf("%w%s", waitErr, formatXvfbStderr(stderr.String()))
				failed = true
			default:
			}
			if failed {
				break
			}

			if _, err := os.Stat(socketPath); err == nil {
				return &XvfbProcess{cmd: cmd, exitCh: exitCh}, display, nil
			}
			if err := SleepContext(ctx, 100*time.Millisecond); err != nil {
				failed = true
				lastErr = err
				break
			}
		}

		_ = cmd.Process.Kill()
		waitErr := <-exitCh
		if waitErr != nil {
			lastErr = fmt.Errorf("%w%s", waitErr, formatXvfbStderr(stderr.String()))
		} else {
			lastErr = fmt.Errorf("timeout waiting for Xvfb socket%s", formatXvfbStderr(stderr.String()))
		}

	}

	if lastErr != nil {
		return nil, "", fmt.Errorf("start Xvfb: no usable display found: %w", lastErr)
	}
	return nil, "", fmt.Errorf("start Xvfb: no usable display found")
}

func formatXvfbStderr(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return " | xvfb stderr: " + stderr
}

func transientDirRoots() []string {
	roots := make([]string, 0, 3)
	if value := strings.TrimSpace(os.Getenv("FLARESOLVERR_TMPDIR")); value != "" {
		roots = append(roots, value)
	}
	if value := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); value != "" {
		roots = append(roots, filepath.Join(value, "flaresolverr-go"))
	}
	roots = append(roots, "/dev/shm/flaresolverr-go")
	return roots
}
