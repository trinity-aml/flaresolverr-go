package webdriverbackend

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var chromedriverInjectionRE = regexp.MustCompile(`(?s)\{window\.cdc.*?;\}`)

func patchChromeDriverBinary(driverPath string) (string, string, error) {
	if stringsTrim(driverPath) == "" {
		return "", "", fmt.Errorf("chromedriver path is empty")
	}

	info, err := os.Stat(driverPath)
	if err != nil {
		return "", "", fmt.Errorf("stat chromedriver: %w", err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("chromedriver path points to a directory")
	}

	tempDir, err := os.MkdirTemp("", "flaresolverr-go-chromedriver-*")
	if err != nil {
		return "", "", fmt.Errorf("create chromedriver temp dir: %w", err)
	}

	patchedPath := filepath.Join(tempDir, filepath.Base(driverPath))
	content, err := os.ReadFile(driverPath)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return "", "", fmt.Errorf("read chromedriver: %w", err)
	}

	if !bytes.Contains(content, []byte("undetected chromedriver")) {
		match := chromedriverInjectionRE.Find(content)
		if len(match) > 0 {
			replacement := []byte(`{console.log("undetected chromedriver 1337!")}`)
			if len(replacement) < len(match) {
				replacement = append(replacement, bytes.Repeat([]byte(" "), len(match)-len(replacement))...)
			}
			content = bytes.Replace(content, match, replacement[:len(match)], 1)
		}
	}

	if err := os.WriteFile(patchedPath, content, 0o755); err != nil {
		_ = os.RemoveAll(tempDir)
		return "", "", fmt.Errorf("write patched chromedriver: %w", err)
	}

	return patchedPath, tempDir, nil
}

func stringsTrim(value string) string {
	return strings.TrimSpace(value)
}
