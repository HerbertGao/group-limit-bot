package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/herbertgao/group-limit-bot/internal/version"
)

const (
	githubAPIBase = "https://api.github.com"
	repoOwner     = "HerbertGao"
	repoName      = "group-limit-bot"
	userAgent     = "group-limit-bot_updater/1.0"
)

// Updater checks the GitHub release API and swaps the running binary in place.
type Updater struct {
	client *http.Client
}

type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// NewUpdater constructs an Updater with sensible HTTP timeouts.
func NewUpdater() *Updater {
	return &Updater{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// GetCurrentVersion returns the currently running binary's embedded version.
func (u *Updater) GetCurrentVersion() string {
	return version.GetVersion()
}

// GetLatestVersion fetches the tag_name of the latest GitHub release.
func (u *Updater) GetLatestVersion() (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", githubAPIBase, repoOwner, repoName)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch latest release: HTTP %d", resp.StatusCode)
	}

	var r release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("decode release: %w", err)
	}
	return r.TagName, nil
}

// CompareVersion reports whether latest is strictly newer than current.
// Accepts both "v1.2.3" and "1.2.3" forms.
func (u *Updater) CompareVersion(current, latest string) bool {
	cp := parseVersionParts(strings.TrimPrefix(current, "v"))
	lp := parseVersionParts(strings.TrimPrefix(latest, "v"))
	n := len(cp)
	if len(lp) > n {
		n = len(lp)
	}
	for len(cp) < n {
		cp = append(cp, 0)
	}
	for len(lp) < n {
		lp = append(lp, 0)
	}
	for i := 0; i < n; i++ {
		if lp[i] > cp[i] {
			return true
		}
		if lp[i] < cp[i] {
			return false
		}
	}
	return false
}

func parseVersionParts(v string) []int {
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		// Use leading digits only, so "3-rc1" → 3 and "1.2.3-rc1" → [1,2,3].
		// A component with no leading digit (e.g. "dev-abc") terminates parsing.
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 {
			break
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

// targetAssetKeyword returns the platform tag embedded in release asset names,
// matching the matrix in .github/workflows/release.yml.
func (u *Updater) targetAssetKeyword() (string, error) {
	switch {
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return "linux_x86_64", nil
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		return "linux_arm64", nil
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		return "macos_x86_64", nil
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "macos_arm64", nil
	}
	return "", fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
}

// GetDownloadURL returns the asset URL for the current platform at the given tag.
func (u *Updater) GetDownloadURL(tag string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", githubAPIBase, repoOwner, repoName, tag)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch release %s: %w", tag, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch release %s: HTTP %d", tag, resp.StatusCode)
	}

	var r release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("decode release: %w", err)
	}

	keyword, err := u.targetAssetKeyword()
	if err != nil {
		return "", err
	}
	for _, a := range r.Assets {
		if strings.Contains(a.Name, keyword) {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("no release asset matched platform keyword %q", keyword)
}

// DownloadAndReplace downloads downloadURL and atomically replaces the running
// executable. The previous binary is kept alongside as <exe>.bak so operators
// can roll back if the new version misbehaves.
func (u *Updater) DownloadAndReplace(downloadURL string) error {
	fmt.Println("下载新版本中...")

	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read download body: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve executable symlinks: %w", err)
	}

	tempPath := exe + ".tmp"
	backupPath := exe + ".bak"

	// Write new binary alongside the current one. Executable needs 0755.
	//nolint:gosec // G306: executable permissions are required here.
	if err := os.WriteFile(tempPath, data, 0o755); err != nil {
		return fmt.Errorf("write temp binary: %w", err)
	}

	// Remove any stale backup from a prior update.
	if _, err := os.Stat(backupPath); err == nil {
		if err := os.Remove(backupPath); err != nil {
			_ = os.Remove(tempPath)
			return fmt.Errorf("remove old backup: %w", err)
		}
	}

	if err := os.Rename(exe, backupPath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("backup current executable: %w", err)
	}
	if err := os.Rename(tempPath, exe); err != nil {
		// Best-effort restore of the previous binary.
		_ = os.Rename(backupPath, exe)
		return fmt.Errorf("install new executable: %w", err)
	}

	fmt.Println("更新完成。")
	fmt.Printf("  新二进制: %s\n", exe)
	fmt.Printf("  备份:     %s(可手动删除)\n", backupPath)
	fmt.Println()
	fmt.Println("运行中的进程需重启才会加载新版本。若使用 systemd 管理:")
	fmt.Println("  sudo systemctl restart group-limit-bot")
	return nil
}

// CheckUpdate returns the latest release tag and whether it is strictly newer
// than the current embedded version.
func (u *Updater) CheckUpdate() (latest string, hasUpdate bool, err error) {
	latest, err = u.GetLatestVersion()
	if err != nil {
		return "", false, err
	}
	return latest, u.CompareVersion(u.GetCurrentVersion(), latest), nil
}

// Update runs the full flow: check latest → compare → download → swap.
func (u *Updater) Update() error {
	fmt.Println("检查更新...")
	current := u.GetCurrentVersion()
	fmt.Printf("  当前版本: %s\n", current)

	// Refuse to auto-update when the current binary carries no parseable
	// version number (e.g. local `dev-<sha>` builds or the default "dev"
	// fallback). Otherwise any tagged release would be treated as newer than
	// 0.0.0 and silently downgrade a source build.
	if len(parseVersionParts(strings.TrimPrefix(current, "v"))) == 0 {
		return fmt.Errorf(
			"current build %q is not a tagged release; refuse to auto-update. "+
				"Download a specific release from https://github.com/%s/%s/releases if needed",
			current, repoOwner, repoName,
		)
	}

	latest, err := u.GetLatestVersion()
	if err != nil {
		return err
	}
	fmt.Printf("  最新版本: %s\n", latest)

	if !u.CompareVersion(current, latest) {
		fmt.Println("当前已是最新版本。")
		return nil
	}

	downloadURL, err := u.GetDownloadURL(latest)
	if err != nil {
		return err
	}
	return u.DownloadAndReplace(downloadURL)
}
