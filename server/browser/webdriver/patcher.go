package webdriverbackend

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var (
	chromedriverInjectionRE = regexp.MustCompile(`(?s)\{window\.cdc.*?;\}`)
	patchedDriverMu         sync.Mutex
)

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

	cacheDir, err := patchedChromeDriverCacheDir()
	if err != nil {
		return "", "", err
	}
	cacheKey := patchedChromeDriverCacheKey(driverPath, info)
	patchedDir := filepath.Join(cacheDir, cacheKey)
	patchedPath := filepath.Join(patchedDir, filepath.Base(driverPath))
	if fileExists(patchedPath) {
		return patchedPath, "", nil
	}

	patchedDriverMu.Lock()
	defer patchedDriverMu.Unlock()

	if fileExists(patchedPath) {
		return patchedPath, "", nil
	}

	content, err := os.ReadFile(driverPath)
	if err != nil {
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

	if err := os.MkdirAll(patchedDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create patched chromedriver cache dir: %w", err)
	}

	tempPath := patchedPath + ".tmp"
	if err := os.WriteFile(tempPath, content, 0o755); err != nil {
		_ = os.Remove(tempPath)
		return "", "", fmt.Errorf("write patched chromedriver: %w", err)
	}
	if err := os.Rename(tempPath, patchedPath); err != nil {
		_ = os.Remove(tempPath)
		return "", "", fmt.Errorf("persist patched chromedriver: %w", err)
	}

	return patchedPath, "", nil
}

func stringsTrim(value string) string {
	return strings.TrimSpace(value)
}

func patchedChromeDriverCacheDir() (string, error) {
	baseDir, err := os.UserCacheDir()
	if err != nil || stringsTrim(baseDir) == "" {
		baseDir = os.TempDir()
	}
	dir := filepath.Join(baseDir, "flaresolverr-go", "patched-chromedriver")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create patched chromedriver cache root: %w", err)
	}
	return dir, nil
}

func patchedChromeDriverCacheKey(driverPath string, info os.FileInfo) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", driverPath, info.Size(), info.ModTime().UnixNano())))
	return hex.EncodeToString(sum[:])
}

func fileExists(path string) bool {
	if stringsTrim(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
