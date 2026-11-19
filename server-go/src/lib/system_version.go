// Package lib shared release-version & GitHub OTA manifest helpers.
// Mirrors server/src/lib/system-version.ts so both HTTP routes (/api/admin/system-update)
// and CLI/OTA subcommands read identical version metadata and artifact manifests.
package lib

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const systemVersionGitTimeout = 30 * time.Second

// ReleaseTagPattern validates release tags like v1.2.3 or v1.2.3-rc.1.
var ReleaseTagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)

// OtaSha256Pattern validates lower-case hex SHA256 digests in OTA manifests.
var OtaSha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// HTTPDoer matches *http.Client for testable GitHub API calls.
type HTTPDoer interface {
	Do(request *http.Request) (*http.Response, error)
}

// SystemVersionMetadata mirrors VersionMetadata from system-version.ts.
type SystemVersionMetadata struct {
	Version    string   `json:"version"`
	GitTag     *string  `json:"gitTag"`
	GitCommit  *string  `json:"gitCommit"`
	BuildTime  *string  `json:"buildTime"`
	DeployedAt *string  `json:"deployedAt"`
	Changelog  []string `json:"changelog"`
}

// OtaArtifactInfo mirrors OtaArtifactInfo from system-version.ts.
type OtaArtifactInfo struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	URL      string `json:"url"`
	Sha256   string `json:"sha256"`
	Size     *int64 `json:"size"`
}

// OtaReleaseInfo mirrors OtaReleaseInfo from system-version.ts.
type OtaReleaseInfo struct {
	ManifestAvailable bool              `json:"manifestAvailable"`
	ManifestURL       *string           `json:"manifestUrl"`
	Artifacts         []OtaArtifactInfo `json:"artifacts"`
	Error             *string           `json:"error"`
}

// IsValidReleaseTag checks whether value matches vX.Y.Z([-+...])?.
func IsValidReleaseTag(value string) bool {
	return ReleaseTagPattern.MatchString(strings.TrimSpace(value))
}

// RuntimePlatform returns runtime.GOOS (e.g. linux, darwin).
func RuntimePlatform() string { return runtime.GOOS }

// RuntimeArch returns runtime.GOARCH (e.g. amd64, arm64).
func RuntimeArch() string { return runtime.GOARCH }

// SelectRuntimeArtifact picks the artifact matching current OS and arch.
func SelectRuntimeArtifact(artifacts []OtaArtifactInfo) *OtaArtifactInfo {
	platform := RuntimePlatform()
	arch := RuntimeArch()
	for i := range artifacts {
		if artifacts[i].Platform == platform && artifacts[i].Arch == arch {
			return &artifacts[i]
		}
	}
	return nil
}

// ReleaseRepository returns SYSTEM_UPDATE_RELEASE_REPOSITORY, GITHUB_REPO, or default.
func ReleaseRepository() string {
	if value := os.Getenv("SYSTEM_UPDATE_RELEASE_REPOSITORY"); value != "" {
		return value
	}
	if value := os.Getenv("GITHUB_REPO"); value != "" {
		return value
	}
	return "Rain-kl/payincus"
}

// ReleaseToken returns SYSTEM_UPDATE_RELEASE_TOKEN or GITHUB_TOKEN.
func ReleaseToken() string {
	if value := os.Getenv("SYSTEM_UPDATE_RELEASE_TOKEN"); value != "" {
		return value
	}
	return os.Getenv("GITHUB_TOKEN")
}

// IsTrustedReleaseHost returns true only for GitHub-owned HTTPS hosts where release tokens may be sent.
func IsTrustedReleaseHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "github.com" ||
		host == "api.github.com" ||
		host == "codeload.github.com" ||
		host == "objects.githubusercontent.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

// FetchReleaseJSON performs an authenticated (on GitHub hosts) GET JSON request.
func FetchReleaseJSON(ctx context.Context, client HTTPDoer, rawURL string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("accept", "application/vnd.github+json")
	request.Header.Set("user-agent", "payincus-online-update")
	if token := ReleaseToken(); token != "" && IsTrustedReleaseHost(rawURL) {
		request.Header.Set("authorization", "Bearer "+token)
	}

	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

// RunGitOutput runs a bounded git command in root, returning trimmed stdout and ok.
func RunGitOutput(ctx context.Context, root string, args ...string) (string, bool) {
	commandCtx, cancel := context.WithTimeout(ctx, systemVersionGitTimeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, "git", args...)
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(output)), true
}

// IsGitRepository checks whether root sits inside a git working tree.
func IsGitRepository(ctx context.Context, root string) bool {
	out, ok := RunGitOutput(ctx, root, "rev-parse", "--is-inside-work-tree")
	return ok && out == "true"
}

// ReadJSONFile reads and decodes a JSON file into target.
func ReadJSONFile(path string, target any) bool {
	payload, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(payload, target) == nil
}

// NormalizeChangelog converts a changelog array or newline string into <=50 clean entries.
func NormalizeChangelog(input any) []string {
	out := make([]string, 0, 4)

	switch typed := input.(type) {
	case []any:
		for _, item := range typed {
			if text := strings.TrimSpace(jsScalarString(item)); text != "" {
				out = append(out, text)
			}
			if len(out) >= 50 {
				break
			}
		}
	case string:
		for _, line := range strings.Split(typed, "\n") {
			line = strings.TrimSuffix(line, "\r")
			line = stripListMarker(line)
			if text := strings.TrimSpace(line); text != "" {
				out = append(out, text)
			}
			if len(out) >= 50 {
				break
			}
		}
	}

	return out
}

// GetPackageVersion reads version from package.json under root, or "0.0.0".
func GetPackageVersion(root string) string {
	var pkg map[string]any
	if ReadJSONFile(filepath.Join(root, "package.json"), &pkg) {
		if version, ok := pkg["version"].(string); ok && version != "" {
			return version
		}
	}
	return "0.0.0"
}

// GetTagChangelog extracts release changelog lines from git tag annotation or commit subject.
func GetTagChangelog(ctx context.Context, root, tag string) []string {
	if msg, ok := RunGitOutput(ctx, root, "tag", "-l", tag, "--format=%(contents)"); ok {
		if lines := NormalizeChangelog(msg); len(lines) > 0 {
			return lines
		}
	}
	if subject, ok := RunGitOutput(ctx, root, "log", "-1", "--pretty=%s", tag); ok {
		return NormalizeChangelog(subject)
	}
	return []string{}
}

// GetCurrentVersionMetadata resolves version.json + git metadata for root.
func GetCurrentVersionMetadata(ctx context.Context, root string) SystemVersionMetadata {
	var versionFile map[string]any
	hasVersionFile := ReadJSONFile(filepath.Join(root, "version.json"), &versionFile)

	gitTag, hasExactTag := RunGitOutput(ctx, root, "describe", "--tags", "--exact-match", "HEAD")
	if !hasExactTag {
		gitTag = ""
	}
	nearestTag, hasNearestTag := RunGitOutput(ctx, root, "describe", "--tags", "--abbrev=0")
	if !hasNearestTag {
		nearestTag = ""
	}
	gitCommit, hasCommit := RunGitOutput(ctx, root, "rev-parse", "--short=12", "HEAD")
	if !hasCommit {
		gitCommit = ""
	}
	packageVersion := GetPackageVersion(root)

	stringField := func(key string) string {
		if !hasVersionFile {
			return ""
		}
		v, _ := versionFile[key].(string)
		return v
	}

	fileVersion := stringField("version")
	fileGitTag := stringField("gitTag")
	fileGitCommit := stringField("gitCommit")
	fileBuildTime := stringField("buildTime")
	fileDeployedAt := stringField("deployedAt")
	var fileChangelog any
	if hasVersionFile {
		fileChangelog = versionFile["changelog"]
	}

	version := firstNonEmptyStr(fileVersion, gitTag, nearestTag, "v"+packageVersion)
	metadata := SystemVersionMetadata{
		Version:   version,
		Changelog: []string{},
	}
	if resolvedTag := firstNonEmptyStr(fileGitTag, gitTag, nearestTag); resolvedTag != "" {
		metadata.GitTag = strPtr(resolvedTag)
	}
	if resolvedCommit := firstNonEmptyStr(fileGitCommit, gitCommit); resolvedCommit != "" {
		metadata.GitCommit = strPtr(resolvedCommit)
	}
	if fileBuildTime != "" {
		metadata.BuildTime = strPtr(fileBuildTime)
	}
	if fileDeployedAt != "" {
		metadata.DeployedAt = strPtr(fileDeployedAt)
	}
	if changelog := NormalizeChangelog(fileChangelog); len(changelog) > 0 {
		metadata.Changelog = changelog
	} else if nearestTag != "" {
		metadata.Changelog = GetTagChangelog(ctx, root, nearestTag)
	}
	return metadata
}

// NormalizeOtaArtifact validates and decodes a single manifest artifact entry.
func NormalizeOtaArtifact(input any) *OtaArtifactInfo {
	value, ok := input.(map[string]any)
	if !ok {
		return nil
	}
	text := func(key string) string {
		raw, ok := value[key].(string)
		if !ok {
			return ""
		}
		return strings.TrimSpace(raw)
	}
	name := text("name")
	platform := text("platform")
	arch := text("arch")
	artifactURL := text("url")
	sha256 := strings.ToLower(text("sha256"))

	var size *int64
	if number, ok := value["size"].(float64); ok {
		if number >= 0 && number == math.Trunc(number) && math.Abs(number) <= float64(1<<53-1) {
			converted := int64(number)
			size = &converted
		}
	}
	if name == "" || platform == "" || arch == "" || artifactURL == "" || !OtaSha256Pattern.MatchString(sha256) {
		return nil
	}
	return &OtaArtifactInfo{
		Name:     name,
		Platform: platform,
		Arch:     arch,
		URL:      artifactURL,
		Sha256:   sha256,
		Size:     size,
	}
}

// GetOtaReleaseInfo fetches OTA manifest artifacts for tag from GitHub Releases.
func GetOtaReleaseInfo(ctx context.Context, client HTTPDoer, tag string) OtaReleaseInfo {
	repository := ReleaseRepository()
	releaseAPIURL := "https://api.github.com/repos/" + repository + "/releases/tags/" + url.PathEscape(tag)

	empty := func(reason string, manifestURL *string) OtaReleaseInfo {
		return OtaReleaseInfo{
			ManifestAvailable: false,
			ManifestURL:       manifestURL,
			Artifacts:         []OtaArtifactInfo{},
			Error:             &reason,
		}
	}

	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := FetchReleaseJSON(ctx, client, releaseAPIURL, &release); err != nil {
		return empty("读取 GitHub Release OTA manifest 失败: "+err.Error(), nil)
	}

	manifestURL := ""
	for _, asset := range release.Assets {
		if asset.Name == "ota-manifest.json" || asset.Name == "incudal-"+tag+"-ota-manifest.json" {
			manifestURL = asset.BrowserDownloadURL
			break
		}
	}
	if manifestURL == "" {
		return empty("GitHub Release 未提供 OTA manifest", nil)
	}

	var manifest struct {
		Artifacts []any `json:"artifacts"`
	}
	if err := FetchReleaseJSON(ctx, client, manifestURL, &manifest); err != nil {
		return empty("读取 GitHub Release OTA manifest 失败: "+err.Error(), nil)
	}

	artifacts := make([]OtaArtifactInfo, 0, len(manifest.Artifacts))
	for _, raw := range manifest.Artifacts {
		if artifact := NormalizeOtaArtifact(raw); artifact != nil {
			artifacts = append(artifacts, *artifact)
		}
	}
	if len(artifacts) == 0 {
		return empty("OTA manifest 中没有有效 artifact", strPtr(manifestURL))
	}
	return OtaReleaseInfo{
		ManifestAvailable: true,
		ManifestURL:       strPtr(manifestURL),
		Artifacts:         artifacts,
	}
}

// CanApplyUpdate checks whether tag can be applied via OTA artifact or Git checkout.
func CanApplyUpdate(ctx context.Context, client HTTPDoer, tag, root string) bool {
	if !IsValidReleaseTag(tag) {
		return false
	}
	ota := GetOtaReleaseInfo(ctx, client, tag)
	if SelectRuntimeArtifact(ota.Artifacts) != nil {
		return true
	}
	return IsGitRepository(ctx, root)
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func strPtr(v string) *string { return &v }

func stripListMarker(line string) string {
	if line == "" || (line[0] != '-' && line[0] != '*') {
		return line
	}
	rest := line[1:]
	trimmed := strings.TrimLeftFunc(rest, unicode.IsSpace)
	if len(trimmed) == len(rest) {
		return line
	}
	return trimmed
}

func jsScalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		if typed == math.Trunc(typed) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return "null"
	default:
		return ""
	}
}
