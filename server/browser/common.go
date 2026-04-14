package browser

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var headlessUserAgentRE = regexp.MustCompile(`(?i)HEADLESS`)

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
	Kind      string  `json:"kind"`
	Tag       string  `json:"tag"`
	Type      string  `json:"type"`
	Text      string  `json:"text"`
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Title     string  `json:"title"`
	AriaLabel string  `json:"ariaLabel"`
	Role      string  `json:"role"`
	ClassName string  `json:"className"`
	TabIndex  int64   `json:"tabIndex"`
	Disabled  bool    `json:"disabled"`
	Src       string  `json:"src"`
	Left      float64 `json:"left"`
	Top       float64 `json:"top"`
	Width     float64 `json:"width"`
	Height    float64 `json:"height"`
	Visible   bool    `json:"visible"`
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
			builder.WriteString(html.EscapeString(url.QueryEscape(name)))
			builder.WriteString(`" value="`)
			builder.WriteString(html.EscapeString(url.QueryEscape(value)))
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

func RelevantChallengeTargets(targets []ClickTarget) []ClickTarget {
	relevant := make([]ClickTarget, 0, len(targets))
	for _, target := range targets {
		if IsVerifyButtonTarget(target) || IsChallengeIframeTarget(target) {
			relevant = append(relevant, target)
		}
	}
	return relevant
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
			if target.Width >= 40 && target.Width <= 500 && target.Height >= 20 && target.Height <= 180 {
				candidates = append(candidates, target)
			}
		}
	}
	return candidates
}

func TabbableTargets(targets []ClickTarget) []ClickTarget {
	candidates := make([]ClickTarget, 0, len(targets))
	for _, target := range targets {
		if !target.Visible || target.Disabled || target.ID == "__flaresolverr-focus" {
			continue
		}
		if target.Width <= 0 || target.Height <= 0 {
			continue
		}
		if target.TabIndex >= 0 {
			candidates = append(candidates, target)
			continue
		}
		switch target.Tag {
		case "iframe", "button", "input", "textarea", "select":
			candidates = append(candidates, target)
		case "a":
			if target.Src == "" {
				candidates = append(candidates, target)
			}
		}
		if target.Role == "button" {
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
