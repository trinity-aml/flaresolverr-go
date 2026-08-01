package flaresolverr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func findChromeBinary() string {
	candidates := []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"chrome",
	}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path
		}
	}
	return ""
}

func findChromeDriverBinary() string {
	candidates := []string{
		"chromedriver",
		"chromedriver-linux64",
	}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path
		}
	}
	return ""
}

// geckoInstallMarkers are the files a real Gecko build keeps beside its binary.
// geckodriver reads application.ini to identify the browser and rejects
// anything without it — "binary is not a Firefox executable". A distro wrapper
// in /usr/bin (snap and flatpak both ship one, as a shell script) has no such
// sibling, so handing that path to geckodriver fails browser construction.
var geckoInstallMarkers = []string{"application.ini", "platform.ini"}

// looksLikeGeckoInstall reports whether path is the real browser binary of a
// Gecko installation rather than a launcher pointing at one.
func looksLikeGeckoInstall(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	dir := filepath.Dir(resolved)
	for _, marker := range geckoInstallMarkers {
		if fileExists(filepath.Join(dir, marker)) {
			return true
		}
		// macOS splits the bundle: the binary sits in Contents/MacOS while the
		// .ini files sit in Contents/Resources.
		if fileExists(filepath.Join(dir, "..", "Resources", marker)) {
			return true
		}
	}
	return false
}

// firefoxCandidates lists every place a Gecko browser may live, best first.
//
// camoufox comes before *any* Firefox, including one on PATH: it is strictly
// preferred for CF bypass, and an unpacked ~/.cache release used to lose to a
// stock firefox simply because PATH was searched for both names before
// ~/.cache was consulted at all.
func firefoxCandidates() []string {
	var candidates []string
	add := func(path string) {
		if strings.TrimSpace(path) != "" {
			candidates = append(candidates, path)
		}
	}

	if path, err := exec.LookPath("camoufox"); err == nil {
		add(path)
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".cache", "camoufox", "camoufox"))
		add(filepath.Join(home, ".cache", "camoufox", "camoufox-bin"))
	}
	if path, err := exec.LookPath("firefox"); err == nil {
		add(path)
	}
	for _, path := range wellKnownFirefoxPaths {
		add(path)
	}

	return candidates
}

// wellKnownFirefoxPaths is where the real binary lives on distros whose PATH
// entry is a wrapper. A var, not a const block, so tests can pin it and stop
// depending on what happens to be installed on the machine running them.
var wellKnownFirefoxPaths = []string{
	"/usr/lib/firefox/firefox",
	"/usr/lib/firefox-esr/firefox-esr",
	"/opt/firefox/firefox",
}

// findFirefoxBinary returns the best Gecko browser on this machine, preferring
// daijro/camoufox over stock Firefox.
func findFirefoxBinary() string {
	candidates := firefoxCandidates()

	// A candidate geckodriver will actually accept wins outright, so a wrapper
	// script on PATH loses to the real binary behind it.
	for _, candidate := range candidates {
		if fileExists(candidate) && looksLikeGeckoInstall(candidate) {
			return candidate
		}
	}
	// Nothing verifiable: fall back to whatever exists, which is the historical
	// behaviour. Layouts this does not model must keep working, and a path
	// geckodriver rejects still produces a clearer error than finding nothing.
	for _, candidate := range candidates {
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

// findGeckoDriverBinary searches PATH for geckodriver.
func findGeckoDriverBinary() string {
	candidates := []string{"geckodriver"}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path
		}
	}
	return ""
}

// isFirefoxBinary returns true if the given executable path looks like a
// Gecko-based browser (firefox / camoufox). Detection is deliberately loose
// — matches on the file basename.
func isFirefoxBinary(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.Contains(base, "firefox") || strings.Contains(base, "camoufox")
}
