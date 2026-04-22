package flaresolverr

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const geckoDriverReleasesURL = "https://api.github.com/repos/mozilla/geckodriver/releases/latest"

var (
	managedGeckoDriverMu      sync.Mutex
	managedGeckoDriverPathMu  sync.Mutex
	managedGeckoDriverPath    string
	geckoDriverHTTPClient     = func() *http.Client {
		return &http.Client{Timeout: 120 * time.Second}
	}
)

type geckoDriverReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	ContentType        string `json:"content_type"`
}

type geckoDriverRelease struct {
	TagName string                    `json:"tag_name"`
	Assets  []geckoDriverReleaseAsset `json:"assets"`
}

// resolveGeckoDriverPath returns a usable path to a geckodriver binary. It
// honours an explicit DriverPath first, then optionally auto-downloads the
// latest release into the user cache, and finally falls back to PATH lookup.
func resolveGeckoDriverPath(cfg Config) (string, error) {
	cfg = cfg.withDefaults()

	if path := strings.TrimSpace(cfg.DriverPath); path != "" {
		return path, nil
	}

	var autoErr error
	if cfg.DriverAutoDownload {
		path, err := ensureManagedGeckoDriver(context.Background(), cfg)
		if err == nil && strings.TrimSpace(path) != "" {
			return path, nil
		}
		autoErr = err
	}
	if path := findGeckoDriverBinary(); path != "" {
		return path, nil
	}
	if autoErr != nil {
		return "", autoErr
	}
	return "", nil
}

// ensureManagedGeckoDriver downloads the latest geckodriver release once and
// caches the extracted binary. Subsequent calls reuse the cached path.
func ensureManagedGeckoDriver(ctx context.Context, cfg Config) (string, error) {
	cfg = cfg.withDefaults()

	if path := getManagedGeckoDriverPath(); path != "" {
		return path, nil
	}

	managedGeckoDriverMu.Lock()
	defer managedGeckoDriverMu.Unlock()

	if path := getManagedGeckoDriverPath(); path != "" {
		return path, nil
	}

	platform, binaryName, archiveExt, err := geckoDriverPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}

	cacheDir, err := managedGeckoDriverCacheDir(cfg)
	if err != nil {
		return "", err
	}

	release, err := fetchGeckoDriverLatestRelease(ctx)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(release.TagName)
	if version == "" {
		return "", fmt.Errorf("geckodriver release metadata missing tag_name")
	}

	finalPath := filepath.Join(cacheDir, version, platform, binaryName)
	if fileExists(finalPath) {
		storeManagedGeckoDriverPath(finalPath)
		return finalPath, nil
	}

	asset := matchGeckoDriverAsset(release.Assets, platform, archiveExt)
	if asset == nil {
		return "", fmt.Errorf("geckodriver release %s has no asset for %s/%s", version, runtime.GOOS, runtime.GOARCH)
	}

	cfg.Logger.Info("downloading geckodriver", "version", version, "platform", platform, "url", asset.BrowserDownloadURL)
	if err := downloadGeckoDriverArchive(ctx, asset.BrowserDownloadURL, finalPath, binaryName, archiveExt); err != nil {
		return "", err
	}

	storeManagedGeckoDriverPath(finalPath)
	return finalPath, nil
}

func geckoDriverPlatform(goos, goarch string) (string, string, string, error) {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return "linux64", "geckodriver", "tar.gz", nil
		case "arm64":
			return "linux-aarch64", "geckodriver", "tar.gz", nil
		case "386":
			return "linux32", "geckodriver", "tar.gz", nil
		}
	case "darwin":
		switch goarch {
		case "amd64":
			return "macos", "geckodriver", "tar.gz", nil
		case "arm64":
			return "macos-aarch64", "geckodriver", "tar.gz", nil
		}
	case "windows":
		switch goarch {
		case "amd64":
			return "win64", "geckodriver.exe", "zip", nil
		case "386":
			return "win32", "geckodriver.exe", "zip", nil
		case "arm64":
			return "win-aarch64", "geckodriver.exe", "zip", nil
		}
	}
	return "", "", "", fmt.Errorf("no geckodriver release published for %s/%s", goos, goarch)
}

// matchGeckoDriverAsset picks the release asset whose name contains the target
// platform slug and matches the expected archive extension.
func matchGeckoDriverAsset(assets []geckoDriverReleaseAsset, platform, archiveExt string) *geckoDriverReleaseAsset {
	targetSuffix := "-" + platform + "." + archiveExt
	for i := range assets {
		name := strings.ToLower(strings.TrimSpace(assets[i].Name))
		if strings.HasSuffix(name, targetSuffix) {
			return &assets[i]
		}
	}
	return nil
}

func fetchGeckoDriverLatestRelease(ctx context.Context) (*geckoDriverRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geckoDriverReleasesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := geckoDriverHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("geckodriver releases request failed with %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var release geckoDriverRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode geckodriver release metadata: %w", err)
	}
	return &release, nil
}

// downloadGeckoDriverArchive fetches the asset and extracts `binaryName` into
// targetPath. Handles both tar.gz (Linux/macOS) and zip (Windows).
func downloadGeckoDriverArchive(ctx context.Context, rawURL, targetPath, binaryName, archiveExt string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := geckoDriverHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download geckodriver archive failed with %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var binaryData []byte
	switch archiveExt {
	case "tar.gz":
		binaryData, err = extractTarGz(resp.Body, binaryName)
	case "zip":
		binaryData, err = extractZipFromStream(resp.Body, binaryName)
	default:
		return fmt.Errorf("unsupported geckodriver archive extension %q", archiveExt)
	}
	if err != nil {
		return fmt.Errorf("extract geckodriver archive: %w", err)
	}
	if len(binaryData) == 0 {
		return fmt.Errorf("geckodriver binary %s not found in archive", binaryName)
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

func extractTarGz(r io.Reader, binaryName string) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}
		return io.ReadAll(tr)
	}
}

func extractZipFromStream(r io.Reader, binaryName string) ([]byte, error) {
	// zip.NewReader needs a ReaderAt, so buffer the archive first. Geckodriver
	// zips are small (a few MB) so this is acceptable.
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return extractZipBinary(data, binaryName)
}

func extractZipBinary(archiveData []byte, binaryName string) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	if err != nil {
		return nil, err
	}
	for _, file := range reader.File {
		if filepath.Base(file.Name) != binaryName {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		return data, err
	}
	return nil, nil
}

func managedGeckoDriverCacheDir(cfg Config) (string, error) {
	if strings.TrimSpace(cfg.DriverCacheDir) != "" {
		dir := filepath.Join(cfg.DriverCacheDir, "..", "geckodriver")
		// If DriverCacheDir is a custom path already, put geckodriver next to
		// chromedriver rather than inside it.
		if strings.HasSuffix(strings.TrimRight(cfg.DriverCacheDir, "/"), "/chromedriver") {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", fmt.Errorf("create geckodriver cache dir: %w", err)
			}
			return dir, nil
		}
		if err := os.MkdirAll(cfg.DriverCacheDir, 0o755); err != nil {
			return "", fmt.Errorf("create driver cache dir: %w", err)
		}
		return cfg.DriverCacheDir, nil
	}

	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = filepath.Join(os.TempDir(), "flaresolverr-go-cache")
	}
	dir := filepath.Join(base, "flaresolverr-go", "geckodriver")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create geckodriver cache dir: %w", err)
	}
	return dir, nil
}

func getManagedGeckoDriverPath() string {
	managedGeckoDriverPathMu.Lock()
	defer managedGeckoDriverPathMu.Unlock()
	if managedGeckoDriverPath == "" {
		return ""
	}
	if !fileExists(managedGeckoDriverPath) {
		managedGeckoDriverPath = ""
		return ""
	}
	return managedGeckoDriverPath
}

func storeManagedGeckoDriverPath(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	managedGeckoDriverPathMu.Lock()
	managedGeckoDriverPath = path
	managedGeckoDriverPathMu.Unlock()
}
