package flaresolverr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const defaultChromeForTestingBaseURL = "https://googlechromelabs.github.io/chrome-for-testing"

var (
	chromeVersionRE                   = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)
	managedDriverMu                   sync.Mutex
	chromeForTestingHTTPClientFactory = func() *http.Client {
		return &http.Client{Timeout: 30 * time.Second}
	}
)

type chromeForTestingVersion struct {
	Version   string `json:"version"`
	Downloads struct {
		ChromeDriver []chromeForTestingDownload `json:"chromedriver"`
	} `json:"downloads"`
}

type chromeForTestingDownload struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
}

func resolveChromeDriverPath(cfg Config) (string, error) {
	cfg = cfg.withDefaults()

	if path := strings.TrimSpace(cfg.DriverPath); path != "" {
		return path, nil
	}

	var autoErr error
	if cfg.DriverAutoDownload {
		path, err := ensureManagedChromeDriver(context.Background(), cfg)
		if err == nil && strings.TrimSpace(path) != "" {
			return path, nil
		}
		autoErr = err
	}
	if path := findChromeDriverBinary(); path != "" {
		return path, nil
	}
	if autoErr != nil {
		return "", autoErr
	}
	return "", nil
}

func ensureManagedChromeDriver(ctx context.Context, cfg Config) (string, error) {
	cfg = cfg.withDefaults()

	browserPath := strings.TrimSpace(cfg.BrowserPath)
	if browserPath == "" {
		browserPath = findChromeBinary()
	}
	if browserPath == "" {
		return "", fmt.Errorf("chrome executable not found; cannot auto-download chromedriver")
	}

	browserVersion, err := detectChromeVersion(ctx, browserPath)
	if err != nil {
		return "", err
	}
	return ensureManagedChromeDriverVersion(ctx, cfg, browserVersion)
}

func ensureManagedChromeDriverVersion(ctx context.Context, cfg Config, browserVersion string) (string, error) {
	cfg = cfg.withDefaults()

	platform, binaryName, err := chromeForTestingPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}

	driverVersion, err := resolveChromeDriverVersion(ctx, cfg, browserVersion)
	if err != nil {
		return "", err
	}

	cacheDir, err := managedChromeDriverCacheDir(cfg)
	if err != nil {
		return "", err
	}
	finalPath := filepath.Join(cacheDir, driverVersion, platform, binaryName)
	if fileExists(finalPath) {
		return finalPath, nil
	}

	managedDriverMu.Lock()
	defer managedDriverMu.Unlock()

	if fileExists(finalPath) {
		return finalPath, nil
	}

	downloadURL, err := resolveChromeDriverDownloadURL(ctx, cfg, driverVersion, platform)
	if err != nil {
		return "", err
	}

	cfg.Logger.Info("downloading matching chromedriver", "browser_version", browserVersion, "driver_version", driverVersion, "platform", platform)
	if err := downloadChromeDriverArchive(ctx, downloadURL, finalPath, binaryName); err != nil {
		return "", err
	}
	return finalPath, nil
}

func detectChromeVersion(ctx context.Context, browserPath string) (string, error) {
	for _, args := range [][]string{{"--product-version"}, {"--version"}} {
		runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		output, err := exec.CommandContext(runCtx, browserPath, args...).CombinedOutput()
		cancel()
		version := parseChromeVersionOutput(string(output))
		if version != "" {
			return version, nil
		}
		if err == nil {
			continue
		}
	}
	return "", fmt.Errorf("detect chrome version from %q failed", browserPath)
}

func parseChromeVersionOutput(raw string) string {
	return chromeVersionRE.FindString(raw)
}

func resolveChromeDriverVersion(ctx context.Context, cfg Config, browserVersion string) (string, error) {
	baseURL := strings.TrimRight(cfg.ChromeForTestingURL, "/")
	buildKey, err := chromeBuildKey(browserVersion)
	if err != nil {
		return "", err
	}
	majorKey, err := chromeMajor(browserVersion)
	if err != nil {
		return "", err
	}

	var lastErr error
	for _, key := range []string{buildKey, majorKey} {
		version, err := fetchChromeForTestingText(ctx, baseURL+"/LATEST_RELEASE_"+key)
		if err == nil && strings.TrimSpace(version) != "" {
			return strings.TrimSpace(version), nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no matching release found")
	}
	return "", fmt.Errorf("resolve chromedriver version for chrome %s: %w", browserVersion, lastErr)
}

func resolveChromeDriverDownloadURL(ctx context.Context, cfg Config, driverVersion, platform string) (string, error) {
	baseURL := strings.TrimRight(cfg.ChromeForTestingURL, "/")
	endpoint := baseURL + "/" + url.PathEscape(driverVersion) + ".json"

	body, err := fetchChromeForTestingJSON(ctx, endpoint)
	if err != nil {
		return "", err
	}

	var version chromeForTestingVersion
	if err := json.Unmarshal(body, &version); err != nil {
		return "", fmt.Errorf("decode chrome for testing version metadata: %w", err)
	}

	for _, entry := range version.Downloads.ChromeDriver {
		if entry.Platform == platform && strings.TrimSpace(entry.URL) != "" {
			return entry.URL, nil
		}
	}
	return "", fmt.Errorf("chromedriver download for platform %s not found in version %s", platform, driverVersion)
}

func fetchChromeForTestingText(ctx context.Context, rawURL string) (string, error) {
	body, err := fetchChromeForTestingJSON(ctx, rawURL)
	if err == nil {
		return string(body), nil
	}

	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if reqErr != nil {
		return "", reqErr
	}
	resp, respErr := chromeForTestingHTTPClient().Do(req)
	if respErr != nil {
		return "", respErr
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("chrome for testing request %s failed with %d: %s", rawURL, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", readErr
	}
	return string(data), nil
}

func fetchChromeForTestingJSON(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := chromeForTestingHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("chrome for testing request %s failed with %d: %s", rawURL, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return io.ReadAll(resp.Body)
}

func chromeForTestingHTTPClient() *http.Client {
	return chromeForTestingHTTPClientFactory()
}

func managedChromeDriverCacheDir(cfg Config) (string, error) {
	if strings.TrimSpace(cfg.DriverCacheDir) != "" {
		if err := os.MkdirAll(cfg.DriverCacheDir, 0o755); err != nil {
			return "", fmt.Errorf("create driver cache dir: %w", err)
		}
		return cfg.DriverCacheDir, nil
	}

	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = filepath.Join(os.TempDir(), "flaresolverr-go-cache")
	}
	dir := filepath.Join(base, "flaresolverr-go", "chromedriver")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create driver cache dir: %w", err)
	}
	return dir, nil
}

func chromeForTestingPlatform(goos, goarch string) (string, string, error) {
	switch goos {
	case "linux":
		if goarch == "amd64" {
			return "linux64", "chromedriver", nil
		}
	case "darwin":
		if goarch == "amd64" {
			return "mac-x64", "chromedriver", nil
		}
		if goarch == "arm64" {
			return "mac-arm64", "chromedriver", nil
		}
	case "windows":
		if goarch == "386" {
			return "win32", "chromedriver.exe", nil
		}
		if goarch == "amd64" || goarch == "arm64" {
			return "win64", "chromedriver.exe", nil
		}
	}
	return "", "", fmt.Errorf("chrome for testing does not provide chromedriver for %s/%s", goos, goarch)
}

func chromeBuildKey(version string) (string, error) {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) < 3 {
		return "", fmt.Errorf("invalid chrome version %q", version)
	}
	return strings.Join(parts[:3], "."), nil
}

func chromeMajor(version string) (string, error) {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) < 1 || strings.TrimSpace(parts[0]) == "" {
		return "", fmt.Errorf("invalid chrome version %q", version)
	}
	return parts[0], nil
}

func downloadChromeDriverArchive(ctx context.Context, rawURL, targetPath, binaryName string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := chromeForTestingHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download chromedriver archive failed with %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	archiveData, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	reader, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	if err != nil {
		return fmt.Errorf("open chromedriver archive: %w", err)
	}

	var binaryData []byte
	for _, file := range reader.File {
		if filepath.Base(file.Name) != binaryName {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return err
		}
		binaryData, err = io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		break
	}
	if len(binaryData) == 0 {
		return fmt.Errorf("chromedriver binary %s not found in archive", binaryName)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	tempPath := targetPath + ".tmp"
	if err := os.WriteFile(tempPath, binaryData, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o755); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
