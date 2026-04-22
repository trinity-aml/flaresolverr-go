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

// findFirefoxBinary searches PATH for daijro/camoufox first (it is strictly
// preferred over vanilla Firefox for CF bypass) and falls back to a stock
// Firefox binary. It also checks the conventional ~/.cache/camoufox directory
// where the daijro release is unpacked.
func findFirefoxBinary() string {
	candidates := []string{"camoufox", "firefox"}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, rel := range []string{".cache/camoufox/camoufox", ".cache/camoufox/camoufox-bin"} {
			full := filepath.Join(home, rel)
			if info, err := os.Stat(full); err == nil && !info.IsDir() {
				return full
			}
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
