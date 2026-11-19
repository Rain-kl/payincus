// Package ota implements the Go CLI subcommands for PayIncus panel OTA updates,
// rollbacks, version inspection, and production DB readiness checks.
package ota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

const (
	defaultMinimumDiskFreeMB      int64 = 1024
	defaultBackupRetentionCount   int   = 3
	defaultReleaseRetentionCount  int   = 3
	defaultHealthCheckTimeout           = 90 * time.Second
	defaultHealthRetryInterval          = 2 * time.Second
)

type otaPaths struct {
	appDir            string
	installDir        string
	serviceName       string
	frontendURL       string
	adminFrontendURL  string
	backendURL        string
	logDir            string
	logPath           string
	updateDownloadDir string
	executionPath     string
	applyMode         string
	minDiskFreeMB     int64
	backupRetention   int
	releaseRetention  int
	httpClient        *http.Client
	logMu             sync.Mutex
}

func resolveOTAPaths(defaultLogFilename string) otaPaths {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	appDirRaw := envOrDefault("INCUDAL_APP_DIR", cwd)
	appDir, err := filepath.Abs(appDirRaw)
	if err != nil {
		appDir = filepath.Clean(appDirRaw)
	}
	installDirRaw := envOrDefault("INSTALL_DIR", deriveInstallDir(appDir))
	installDir, err := filepath.Abs(installDirRaw)
	if err != nil {
		installDir = filepath.Clean(installDirRaw)
	}
	logDirRaw := envOrDefault("SYSTEM_UPDATE_LOG_DIR", filepath.Join(installDir, "update-logs"))
	logDir, err := filepath.Abs(logDirRaw)
	if err != nil {
		logDir = filepath.Clean(logDirRaw)
	}
	logPathRaw := envOrDefault("SYSTEM_UPDATE_LOG_PATH", filepath.Join(logDir, defaultLogFilename))
	logPath, err := filepath.Abs(logPathRaw)
	if err != nil {
		logPath = filepath.Clean(logPathRaw)
	}

	downloadDir := envOrDefault("SYSTEM_UPDATE_DOWNLOAD_DIR", filepath.Join(installDir, ".incudal-update-downloads"))
	if absDownload, err := filepath.Abs(downloadDir); err == nil {
		downloadDir = absDownload
	}

	defaultPath := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	execPath := defaultPath
	if cur := os.Getenv("PATH"); cur != "" {
		execPath = cur + ":" + defaultPath
	}

	return otaPaths{
		appDir:            appDir,
		installDir:        installDir,
		serviceName:       envOrDefault("SERVICE_NAME", "incudal-backend"),
		frontendURL:       envOrDefault("FRONTEND_URL", "https://pay.payincus.com"),
		adminFrontendURL:  envOrDefault("ADMIN_FRONTEND_URL", "https://admin.payincus.com"),
		backendURL:        envOrDefault("BACKEND_URL", "http://127.0.0.1:3001"),
		logDir:            logDir,
		logPath:           logPath,
		updateDownloadDir: downloadDir,
		executionPath:     execPath,
		applyMode:         strings.ToLower(strings.TrimSpace(os.Getenv("SYSTEM_UPDATE_APPLY_MODE"))),
		minDiskFreeMB:     envInt64Positive("SYSTEM_UPDATE_MIN_DISK_MB", defaultMinimumDiskFreeMB),
		backupRetention:   envIntNonNegative("SYSTEM_UPDATE_BACKUP_RETENTION", defaultBackupRetentionCount),
		releaseRetention:  envIntNonNegative("SYSTEM_UPDATE_RELEASE_RETENTION", defaultReleaseRetentionCount),
		httpClient:        &http.Client{Timeout: 10 * time.Minute},
	}
}

func deriveInstallDir(resolvedAppDir string) string {
	clean := filepath.Clean(resolvedAppDir)
	if filepath.Base(clean) == "current" {
		return filepath.Dir(clean)
	}
	parent := filepath.Dir(clean)
	if filepath.Base(parent) == "releases" {
		return filepath.Dir(parent)
	}
	return clean
}

func (p *otaPaths) appendLog(msg string) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(p.logPath), 0o755)
	f, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "[%s] %s\n", time.Now().UTC().Format(time.RFC3339Nano), msg)
}

func (p *otaPaths) releasesDir() string {
	raw := envOrDefault("SYSTEM_UPDATE_RELEASES_DIR", filepath.Join(p.installDir, "releases"))
	if abs, err := filepath.Abs(raw); err == nil {
		return abs
	}
	return filepath.Clean(raw)
}

func (p *otaPaths) currentLinkPath() string {
	return filepath.Join(p.installDir, "current")
}

func (p *otaPaths) isAtomicReleaseLayout() bool {
	info, err := os.Lstat(p.currentLinkPath())
	return err == nil && (info.Mode()&os.ModeSymlink) != 0
}

func (p *otaPaths) currentReleaseTarget() string {
	resolved, err := filepath.EvalSymlinks(p.currentLinkPath())
	if err != nil {
		return ""
	}
	return resolved
}

func isSafeChildPath(parentDir, targetPath string) bool {
	absParent, err1 := filepath.Abs(parentDir)
	absTarget, err2 := filepath.Abs(targetPath)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(absParent, absTarget)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func assertSafeManagedPath(targetPath, installDir, envName string) error {
	if !isSafeChildPath(installDir, targetPath) {
		return fmt.Errorf("%s must stay inside installDir (%s): %s", envName, installDir, targetPath)
	}
	return nil
}

func buildPostgresEnvFromDatabaseURL(databaseURL string) ([]string, error) {
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return nil, fmt.Errorf("OTA database backup failed: DATABASE_URL is not a valid connection URL")
	}
	var env []string
	if h := parsed.Hostname(); h != "" {
		env = append(env, "PGHOST="+h)
	}
	if port := parsed.Port(); port != "" {
		env = append(env, "PGPORT="+port)
	}
	if parsed.User != nil {
		if u := parsed.User.Username(); u != "" {
			env = append(env, "PGUSER="+u)
		}
		if pass, ok := parsed.User.Password(); ok && pass != "" {
			env = append(env, "PGPASSWORD="+pass)
		}
	}
	if dbName := strings.TrimPrefix(parsed.Path, "/"); dbName != "" {
		if decoded, err := url.PathUnescape(dbName); err == nil {
			env = append(env, "PGDATABASE="+decoded)
		} else {
			env = append(env, "PGDATABASE="+dbName)
		}
	}
	if sslmode := parsed.Query().Get("sslmode"); sslmode != "" {
		env = append(env, "PGSSLMODE="+sslmode)
	}
	return env, nil
}

func (p *otaPaths) runCmd(ctx context.Context, timeout time.Duration, cwd string, extraEnv []string, name string, args ...string) error {
	p.appendLog(fmt.Sprintf("$ %s %s", name, strings.Join(args, " ")))
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if cwd == "" {
		cwd = p.installDir
	}
	cmd := exec.CommandContext(cmdCtx, name, args...)
	cmd.Dir = cwd
	cmd.Env = mergeEnv(os.Environ(), append([]string{"PATH=" + p.executionPath}, extraEnv...))

	logWriter := &otaLogFileWriter{paths: p}
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	if err := cmd.Run(); err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("Command timed out: %s %s", name, strings.Join(args, " "))
		}
		return fmt.Errorf("Command failed (%s %s): %w", name, strings.Join(args, " "), err)
	}
	return nil
}

type otaLogFileWriter struct {
	paths *otaPaths
}

func (w *otaLogFileWriter) Write(p []byte) (int, error) {
	w.paths.logMu.Lock()
	defer w.paths.logMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(w.paths.logPath), 0o755)
	f, err := os.OpenFile(w.paths.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return len(p), nil
	}
	defer f.Close()
	_, _ = f.Write(p)
	return len(p), nil
}

func mergeEnv(base, overrides []string) []string {
	kv := make(map[string]string, len(base)+len(overrides))
	var order []string
	setKV := func(entry string) {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			return
		}
		if _, exists := kv[k]; !exists {
			order = append(order, k)
		}
		kv[k] = v
	}
	for _, e := range base {
		setKV(e)
	}
	for _, e := range overrides {
		setKV(e)
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+kv[k])
	}
	return out
}

func (p *otaPaths) assertRequiredCommands(isArtifactMode bool) error {
	required := []string{"tar", "curl", "pg_dump", "systemctl", "bash"}
	if !isArtifactMode {
		required = append(required, "git", "go", "pnpm")
	}
	var missing []string
	for _, c := range required {
		if !commandAvailable(c, p.executionPath) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"OTA runtime preflight failed: missing required command(s): %s. Install the missing command(s) or add them to PATH before starting the update.",
			strings.Join(missing, ", "),
		)
	}
	return nil
}

func commandAvailable(name, pathValue string) bool {
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	for _, dir := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && !info.IsDir() && (info.Mode()&0o111) != 0 {
			return true
		}
	}
	return false
}

func (p *otaPaths) availableDiskMB(targetPath string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(targetPath, &st); err != nil {
		return 0, err
	}
	availBytes := uint64(st.Bavail) * uint64(st.Bsize)
	return int64(availBytes / (1024 * 1024)), nil
}

func (p *otaPaths) assertEnoughDiskSpace(stage string, requiredMB int64) error {
	availMB, err := p.availableDiskMB(p.installDir)
	if err != nil {
		return fmt.Errorf("check disk space for %s: %w", p.installDir, err)
	}
	p.appendLog(fmt.Sprintf("磁盘空间预检：%s，可用 %d MB，要求 %d MB", stage, availMB, requiredMB))
	if availMB < requiredMB {
		return fmt.Errorf(
			"磁盘空间不足，无法继续 OTA 更新：%s 至少需要 %d MB 可用空间，当前仅 %d MB。请清理 %s、旧 release 或扩容后重试。",
			stage, requiredMB, availMB, p.updateDownloadDir,
		)
	}
	return nil
}

func (p *otaPaths) cleanupUpdateDownloadDir(reason string) error {
	if err := assertSafeManagedPath(p.updateDownloadDir, p.installDir, "SYSTEM_UPDATE_DOWNLOAD_DIR"); err != nil {
		return err
	}
	if err := os.RemoveAll(p.updateDownloadDir); err != nil {
		return err
	}
	if err := os.MkdirAll(p.updateDownloadDir, 0o755); err != nil {
		return err
	}
	p.appendLog(fmt.Sprintf("已清理 OTA 下载缓存（%s）：%s", reason, p.updateDownloadDir))
	return nil
}

func (p *otaPaths) backupDatabase(ctx context.Context, backupDir, targetVersion, timestamp string) (string, error) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return "", fmt.Errorf("OTA database backup failed: DATABASE_URL is not configured")
	}
	pgEnv, err := buildPostgresEnvFromDatabaseURL(databaseURL)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	backupPath := filepath.Join(backupDir, fmt.Sprintf("db-pre-migrate-%s-%s.sql", targetVersion, timestamp))
	tmpPath := backupPath + ".tmp"
	_ = os.Remove(tmpPath)

	if err := p.runCmd(
		ctx,
		10*time.Minute,
		p.installDir,
		pgEnv,
		"pg_dump",
		"--format=plain",
		"--no-owner",
		"--no-privileges",
		"--file",
		tmpPath,
	); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("OTA database backup failed; migration was not started: %w", err)
	}
	if err := os.Rename(tmpPath, backupPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("OTA database backup rename failed: %w", err)
	}
	p.appendLog("Database pre-switch backup completed: " + backupPath)
	return backupPath, nil
}

// assertArtifactStaging validates the extracted release tarball directory
// contains all required Go + Vite assets (no server/dist or server/prisma).
func assertArtifactStaging(stagingDir string) error {
	required := []string{
		"package.json",
		"client/dist/user/index.html",
		"client/dist/admin/index.html",
		"server/incudal-server",
		"server/templates/install.sh",
		"scripts/verify-split-host.sh",
		"scripts/verify-production-readiness.sh",
		"scripts/verify-log-header-exposure.sh",
		".env.example",
		"version.json",
	}
	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(stagingDir, rel)); err != nil {
			return fmt.Errorf("OTA artifact is missing required path: %s", rel)
		}
	}
	return nil
}

func (p *otaPaths) downloadArtifact(ctx context.Context, taskID int32, artifact lib.OtaArtifactInfo) (string, error) {
	if err := os.MkdirAll(p.updateDownloadDir, 0o755); err != nil {
		return "", err
	}
	artifactPath := filepath.Join(p.updateDownloadDir, fmt.Sprintf("%d-%s", taskID, artifact.Name))
	sizeDesc := "unknown"
	if artifact.Size != nil {
		sizeDesc = strconv.FormatInt(*artifact.Size, 10)
	}
	p.appendLog(fmt.Sprintf("Downloading OTA artifact %s (%s bytes)", artifact.Name, sizeDesc))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("user-agent", "payincus-online-update")
	if token := lib.ReleaseToken(); token != "" && lib.IsTrustedReleaseHost(artifact.URL) {
		req.Header.Set("authorization", "Bearer "+token)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Failed to download OTA artifact: HTTP %d", resp.StatusCode)
	}

	tmpPath := artifactPath + ".download"
	_ = os.Remove(tmpPath)
	outFile, err := os.Create(tmpPath)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(outFile, hasher), resp.Body)
	closeErr := outFile.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", closeErr
	}
	if err := os.Rename(tmpPath, artifactPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	if artifact.Size != nil && written != *artifact.Size {
		return "", fmt.Errorf("OTA artifact size mismatch: expected %d, got %d", *artifact.Size, written)
	}
	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA != artifact.Sha256 {
		return "", fmt.Errorf("OTA artifact sha256 mismatch: expected %s, got %s", artifact.Sha256, actualSHA)
	}
	p.appendLog("OTA artifact verified: sha256=" + actualSHA)
	return artifactPath, nil
}

func (p *otaPaths) switchCurrentRelease(ctx context.Context, taskID int32, targetDir string) error {
	releasesDir := p.releasesDir()
	if err := os.MkdirAll(releasesDir, 0o755); err != nil {
		return err
	}
	currentLink := p.currentLinkPath()
	nextLink := filepath.Join(releasesDir, fmt.Sprintf(".next-current-%d-%d", taskID, time.Now().UnixMilli()))
	_ = os.Remove(nextLink)
	if err := os.Symlink(targetDir, nextLink); err != nil {
		return err
	}
	if err := os.Rename(nextLink, currentLink); err != nil {
		_ = os.Remove(nextLink)
		return err
	}
	_ = ctx
	p.appendLog("Switched current release to " + targetDir)
	return nil
}

func (p *otaPaths) chownInstallDir(ctx context.Context) error {
	if lib.RuntimePlatform() != "linux" {
		return nil
	}
	if err := exec.CommandContext(ctx, "id", "incudal").Run(); err != nil {
		p.appendLog("Skipping ownership fix: user incudal does not exist")
		return nil
	}
	return p.runCmd(ctx, 2*time.Minute, p.installDir, nil, "chown", "-R", "incudal:incudal", p.installDir)
}

func copyTreePreserve(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return exec.Command("cp", "-a", src, dst).Run()
}

func (p *otaPaths) restoreRuntimeAssets(backupDir, targetDir string) error {
	if err := copyTreePreserve(filepath.Join(backupDir, ".env"), filepath.Join(targetDir, ".env")); err != nil {
		return err
	}
	if err := copyTreePreserve(filepath.Join(backupDir, "server/certs"), filepath.Join(targetDir, "server/certs")); err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Join(p.installDir, "update-logs"), 0o755)
	_ = os.MkdirAll(filepath.Join(targetDir, "server/certs"), 0o755)
	return nil
}

func (p *otaPaths) writeVersionFile(ctx context.Context, root string) error {
	meta := lib.GetCurrentVersionMetadata(ctx, root)
	nowISO := time.Now().UTC().Format(time.RFC3339Nano)
	meta.DeployedAt = &nowISO
	payload, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "version.json"), append(payload, '\n'), 0o644)
}

func (p *otaPaths) waitForBackendHealth(ctx context.Context) error {
	deadline := time.Now().Add(defaultHealthCheckTimeout)
	attempt := 1
	lastErr := ""
	client := &http.Client{Timeout: 5 * time.Second}

	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.backendURL, "/")+"/api/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				p.appendLog(fmt.Sprintf("Backend health is ready after %d attempt(s)", attempt))
				return nil
			}
			lastErr = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err.Error()
		}
		p.appendLog(fmt.Sprintf("Backend health not ready after attempt %d (%s); retrying in 2s", attempt, lastErr))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(defaultHealthRetryInterval):
		}
		attempt++
	}
	return fmt.Errorf("Backend health did not become ready after restart: %s", lastErr)
}

func (p *otaPaths) verifyUpdatedRuntime(ctx context.Context, isArtifactMode bool) error {
	if err := p.chownInstallDir(ctx); err != nil {
		return err
	}
	p.appendLog("Restarting backend service; Goose startup auto-migration will apply any pending schema changes before /api/health opens")
	if err := p.runCmd(ctx, 2*time.Minute, p.installDir, nil, "systemctl", "restart", p.serviceName); err != nil {
		return err
	}
	if err := p.waitForBackendHealth(ctx); err != nil {
		return err
	}

	splitEnv := []string{
		"ENV_FILE=" + filepath.Join(p.installDir, ".env"),
		"FRONTEND_URL=" + p.frontendURL,
		"ADMIN_FRONTEND_URL=" + p.adminFrontendURL,
		"BACKEND_URL=" + p.backendURL,
	}
	workDir := p.appDir
	if target := p.currentReleaseTarget(); target != "" {
		workDir = target
	}
	splitEnv = append(splitEnv, "INCUDAL_APP_DIR="+workDir)

	if err := p.runCmd(ctx, 3*time.Minute, workDir, splitEnv, "bash", "scripts/verify-split-host.sh"); err != nil {
		return err
	}
	if err := p.runCmd(ctx, 4*time.Minute, workDir, splitEnv, "bash", "scripts/verify-production-readiness.sh"); err != nil {
		return err
	}
	if err := p.runCmd(ctx, 4*time.Minute, workDir, splitEnv, "bash", "scripts/verify-log-header-exposure.sh"); err != nil {
		return err
	}
	if !isArtifactMode {
		p.appendLog("Git source update completed; verified split host, readiness, and log/header posture")
	} else {
		p.appendLog("Artifact OTA verification completed; database migrations were verified cleanly at backend startup")
	}
	return nil
}

func (p *otaPaths) autoRollbackFromBackup(ctx context.Context, taskID int32, backupDir, timestamp string) (bool, error) {
	if _, err := os.Stat(backupDir); err != nil {
		p.appendLog("Auto rollback skipped: backup directory does not exist")
		return false, nil
	}

	if p.isAtomicReleaseLayout() {
		if currentTarget := p.currentReleaseTarget(); currentTarget != "" {
			failedBackup := filepath.Join(p.releasesDir(), "failed-update-"+timestamp)
			p.appendLog("Preserving failed release before auto rollback: " + failedBackup)
			_ = copyTreePreserve(currentTarget, failedBackup)
		}
		p.appendLog("Auto rollback switching current release back to: " + backupDir)
		if err := p.switchCurrentRelease(ctx, taskID, backupDir); err != nil {
			return false, err
		}
	} else {
		failedBackup := fmt.Sprintf("%s.failed-update.%s", p.installDir, timestamp)
		p.appendLog("Auto rollback starting from backup: " + backupDir)
		p.appendLog("Preserving failed install before auto rollback: " + failedBackup)
		_ = copyTreePreserve(p.installDir, failedBackup)
		if err := p.clearInstallDirPreserving([]string{"update-logs"}); err != nil {
			return false, err
		}
		if err := exec.Command("cp", "-a", backupDir+"/.", p.installDir).Run(); err != nil {
			return false, err
		}
		_ = os.MkdirAll(filepath.Join(p.installDir, "server/certs"), 0o755)
	}

	if err := p.chownInstallDir(ctx); err != nil {
		return false, err
	}
	if err := p.runCmd(ctx, 2*time.Minute, p.installDir, nil, "systemctl", "restart", p.serviceName); err != nil {
		return false, err
	}
	if err := p.waitForBackendHealth(ctx); err != nil {
		return false, err
	}
	workDir := p.appDir
	if target := p.currentReleaseTarget(); target != "" {
		workDir = target
	}
	if err := p.runCmd(ctx, 3*time.Minute, workDir, []string{
		"FRONTEND_URL=" + p.frontendURL,
		"ADMIN_FRONTEND_URL=" + p.adminFrontendURL,
		"BACKEND_URL=" + p.backendURL,
	}, "bash", "scripts/verify-split-host.sh"); err != nil {
		return false, err
	}
	p.appendLog("Auto rollback completed successfully")
	return true, nil
}

func (p *otaPaths) clearInstallDirPreserving(preserve []string) error {
	keep := make(map[string]bool, len(preserve))
	for _, name := range preserve {
		keep[name] = true
	}
	entries, err := os.ReadDir(p.installDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if keep[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(p.installDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (p *otaPaths) cleanupOldReleases(ctx context.Context, currentBackupPath string) error {
	if !p.isAtomicReleaseLayout() {
		p.appendLog("旧 release 清理已跳过：当前部署不是原子 current/releases 布局")
		return nil
	}
	releasesDir := p.releasesDir()
	if err := assertSafeManagedPath(releasesDir, p.installDir, "SYSTEM_UPDATE_RELEASES_DIR"); err != nil {
		return err
	}
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	protected := make(map[string]bool)
	if paths, err := db.ListRecentProtectedBackupPaths(ctx, p.backupRetention); err == nil {
		for _, bp := range paths {
			if abs, err := filepath.Abs(bp); err == nil {
				protected[abs] = true
			}
		}
	}
	if cur := p.currentReleaseTarget(); cur != "" {
		if abs, err := filepath.Abs(cur); err == nil {
			protected[abs] = true
		}
	}
	if currentBackupPath != "" {
		if abs, err := filepath.Abs(currentBackupPath); err == nil {
			protected[abs] = true
		}
	}

	type relEntry struct {
		name  string
		path  string
		mtime time.Time
	}
	var list []relEntry
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".next-current-") {
			continue
		}
		full := filepath.Join(releasesDir, entry.Name())
		info, err := os.Lstat(full)
		if err != nil || !info.IsDir() {
			continue
		}
		list = append(list, relEntry{name: entry.Name(), path: full, mtime: info.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mtime.After(list[j].mtime) })

	kept, deleted := 0, 0
	for _, rel := range list {
		abs, _ := filepath.Abs(rel.path)
		if !isSafeChildPath(releasesDir, abs) {
			continue
		}
		if protected[abs] {
			p.appendLog("保留受保护 release：" + rel.name)
			continue
		}
		if kept < p.releaseRetention {
			kept++
			continue
		}
		if err := os.RemoveAll(abs); err == nil {
			deleted++
			p.appendLog("已删除旧 release：" + rel.name)
		}
	}
	p.appendLog(fmt.Sprintf("旧 release 清理完成：保留 %d 个非保护 release，删除 %d 个旧 release，保护 %d 个路径", kept, deleted, len(protected)))
	return nil
}

func (p *otaPaths) applyArtifactUpdate(ctx context.Context, taskID int32, artifact lib.OtaArtifactInfo, targetVersion, backupDir, timestamp string) (string, error) {
	artifactPath, err := p.downloadArtifact(ctx, taskID, artifact)
	if err != nil {
		return "", err
	}
	stagingDir := filepath.Join(p.updateDownloadDir, fmt.Sprintf("staging-%d-%s", taskID, timestamp))
	_ = os.RemoveAll(stagingDir)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", err
	}
	if err := p.runCmd(ctx, 3*time.Minute, p.installDir, nil, "tar", "-xzf", artifactPath, "-C", stagingDir); err != nil {
		return "", err
	}
	if err := assertArtifactStaging(stagingDir); err != nil {
		return "", err
	}

	if p.isAtomicReleaseLayout() {
		currentTarget := p.currentReleaseTarget()
		if currentTarget == "" {
			return "", fmt.Errorf("Atomic release layout is missing current target")
		}
		releaseDir := filepath.Join(p.releasesDir(), fmt.Sprintf("%s-%s", targetVersion, timestamp))
		p.appendLog("Atomic release directory: " + releaseDir)
		if err := copyTreePreserve(stagingDir, releaseDir); err != nil {
			return "", err
		}
		_ = os.Chmod(filepath.Join(releaseDir, "server/incudal-server"), 0o755)
		if err := p.restoreRuntimeAssets(currentTarget, releaseDir); err != nil {
			return "", err
		}
		if err := p.writeVersionFile(ctx, releaseDir); err != nil {
			return "", err
		}
		if err := p.chownInstallDir(ctx); err != nil {
			return "", err
		}
		if err := p.switchCurrentRelease(ctx, taskID, releaseDir); err != nil {
			return "", err
		}
		return currentTarget, nil
	}

	p.appendLog("Backup directory: " + backupDir)
	if err := copyTreePreserve(p.installDir, backupDir); err != nil {
		return "", err
	}
	if err := p.clearInstallDirPreserving([]string{".env", ".git", ".gitconfig", ".incudal-update-downloads", "update-logs"}); err != nil {
		return "", err
	}
	if err := exec.Command("cp", "-a", stagingDir+"/.", p.installDir).Run(); err != nil {
		return "", err
	}
	_ = os.Chmod(filepath.Join(p.installDir, "server/incudal-server"), 0o755)
	if err := p.restoreRuntimeAssets(backupDir, p.installDir); err != nil {
		return "", err
	}
	if err := p.writeVersionFile(ctx, p.installDir); err != nil {
		return "", err
	}
	return backupDir, nil
}

func (p *otaPaths) applyGitUpdate(ctx context.Context, targetVersion, backupDir string) (string, error) {
	if err := p.runCmd(ctx, 2*time.Minute, p.appDir, nil, "git", "config", "--global", "--add", "safe.directory", p.appDir); err != nil {
		p.appendLog("warning: failed setting git safe.directory: " + err.Error())
	}
	if err := p.runCmd(ctx, 2*time.Minute, p.installDir, nil, "git", "fetch", "--tags", "--quiet"); err != nil {
		return "", err
	}
	if err := p.runCmd(ctx, 30*time.Second, p.installDir, nil, "git", "rev-parse", "--verify", targetVersion+"^{commit}"); err != nil {
		return "", err
	}
	p.appendLog("Backup directory: " + backupDir)
	if err := copyTreePreserve(p.installDir, backupDir); err != nil {
		return "", err
	}
	if err := p.runCmd(ctx, 2*time.Minute, p.installDir, nil, "git", "checkout", "--force", targetVersion); err != nil {
		return "", err
	}
	if err := p.runCmd(ctx, 2*time.Minute, p.installDir, nil, "git", "clean", "-fdx", "-e", ".env", "-e", ".gitconfig", "-e", "server/certs", "-e", "update-logs"); err != nil {
		return "", err
	}
	if err := p.restoreRuntimeAssets(backupDir, p.installDir); err != nil {
		return "", err
	}
	if err := p.runCmd(ctx, 10*time.Minute, p.installDir, []string{"NODE_ENV=development", "PNPM_CONFIG_PROD=false"}, "pnpm", "install", "--frozen-lockfile", "--prod=false"); err != nil {
		return "", err
	}
	if err := p.runCmd(ctx, 10*time.Minute, p.installDir, nil, "pnpm", "--filter", "client", "build"); err != nil {
		return "", err
	}
	if err := p.runCmd(ctx, 10*time.Minute, filepath.Join(p.installDir, "server-go"), []string{"CGO_ENABLED=0"}, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", "../server/incudal-server", "./cmd/server"); err != nil {
		return "", err
	}
	_ = os.Chmod(filepath.Join(p.installDir, "server/incudal-server"), 0o755)
	if err := p.writeVersionFile(ctx, p.installDir); err != nil {
		return "", err
	}
	return backupDir, nil
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt64Positive(key string, fallback int64) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func envIntNonNegative(key string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || v < 0 {
		return fallback
	}
	return v
}
