package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rain-kl/payincus/server-go/src/config"
	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// RunVersionCommand prints the active release version (used by incudal.sh & install-panel.sh).
func RunVersionCommand(args []string) int {
	_ = args
	p := resolveOTAPaths("version.log")
	meta := lib.GetCurrentVersionMetadata(context.Background(), p.appDir)
	fmt.Println(meta.Version)
	return 0
}

// RunVerifyProductionDBCommand runs Go production DB readiness verification.
func RunVerifyProductionDBCommand(args []string) int {
	_ = args
	config.LoadEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.Init(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[verify-production-db] ERROR: database init failed: %v\n", err)
		return 1
	}
	defer db.Close()
	return verifyProductionDatabase(ctx)
}

// RunOTAStartCommand replaces start-system-update-task.ts for CLI `apply-online-update.sh`.
func RunOTAStartCommand(args []string) int {
	config.LoadEnv()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: incudal-server ota-start v1.2.3")
		return 1
	}
	targetVersion := strings.TrimSpace(args[0])
	if !lib.IsValidReleaseTag(targetVersion) {
		fmt.Fprintln(os.Stderr, "Usage: incudal-server ota-start v1.2.3")
		return 1
	}

	p := resolveOTAPaths("system-update-start.log")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.Init(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "database init failed: %v\n", err)
		return 1
	}
	defer db.Close()

	httpClient := &http.Client{Timeout: 30 * time.Second}
	if !lib.CanApplyUpdate(ctx, httpClient, targetVersion, p.appDir) {
		fmt.Fprintln(os.Stderr, "No usable OTA artifact for this platform and the deployment directory is not a Git working tree; online updates need either a release artifact or a Git checkout.")
		return 1
	}
	running, err := db.HasRunningSystemUpdateTask(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check running update task failed: %v\n", err)
		return 1
	}
	if running {
		fmt.Fprintln(os.Stderr, "Another system update task is already running")
		return 1
	}

	if err := os.MkdirAll(p.logDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create log dir failed: %v\n", err)
		return 1
	}

	preferredUser, _ := strconv.ParseInt(os.Getenv("SYSTEM_UPDATE_STARTED_BY_USER_ID"), 10, 32)
	startedByID, err := db.FindFirstActiveAdminUserID(ctx, int32(preferredUser))
	if err != nil || startedByID <= 0 {
		fmt.Fprintln(os.Stderr, "No active administrator found for system update task")
		return 1
	}

	current := lib.GetCurrentVersionMetadata(ctx, p.appDir)
	fromVersion := current.Version
	pendingLogPath := filepath.Join(p.logDir, fmt.Sprintf("system-update-pending-%d.log", time.Now().UnixMilli()))
	task, err := db.CreateSystemUpdateTask(ctx, db.CreateSystemUpdateTaskInput{
		TargetVersion:   targetVersion,
		FromVersion:     &fromVersion,
		StartedByUserID: startedByID,
		LogPath:         pendingLogPath,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create update task failed: %v\n", err)
		return 1
	}
	logPath := filepath.Join(p.logDir, fmt.Sprintf("system-update-%d.log", task.ID))
	_ = db.UpdateSystemUpdateTaskLogPath(ctx, task.ID, logPath)
	task.LogPath = &logPath

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve executable failed: %v\n", err)
		return 1
	}
	cmd := exec.Command(exe, "ota-update", strconv.Itoa(int(task.ID)), targetVersion)
	cmd.Dir = p.appDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
		defer devNull.Close()
	}
	cmd.Env = mergeEnv(os.Environ(), []string{
		"INCUDAL_APP_DIR=" + p.appDir,
		"INSTALL_DIR=" + p.installDir,
		"SYSTEM_UPDATE_LOG_DIR=" + p.logDir,
		"SYSTEM_UPDATE_LOG_PATH=" + logPath,
	})
	if err := cmd.Start(); err != nil {
		msg := "Failed to start system update worker: " + err.Error()
		_ = db.MarkSystemUpdateTaskFinished(ctx, task.ID, db.MarkSystemUpdateTaskFinishedInput{
			Status:       models.SystemUpdateTaskStatusFailed,
			ErrorMessage: &msg,
		})
		fmt.Fprintf(os.Stderr, "%s\n", msg)
		return 1
	}

	out, _ := json.MarshalIndent(map[string]any{"task": task}, "", "  ")
	fmt.Println(string(out))
	return 0
}

// RunOTAUpdateCommand executes an atomic/legacy OTA update task (replaces run-system-update-task.ts).
func RunOTAUpdateCommand(args []string) int {
	config.LoadEnv()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: incudal-server ota-update <taskId> [targetVersion]")
		return 1
	}
	parsedID, err := strconv.ParseInt(args[0], 10, 32)
	if err != nil || parsedID <= 0 {
		fmt.Fprintln(os.Stderr, "Invalid task id")
		return 1
	}
	taskID := int32(parsedID)
	targetVersion := ""
	if len(args) >= 2 {
		targetVersion = strings.TrimSpace(args[1])
	}

	p := resolveOTAPaths(fmt.Sprintf("system-update-%d.log", taskID))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if err := db.Init(ctx); err != nil {
		p.appendLog("FATAL: database init failed: " + err.Error())
		return 1
	}
	defer db.Close()

	task, err := db.GetSystemUpdateTask(ctx, taskID)
	if err != nil || task == nil {
		p.appendLog("FATAL: System update task not found")
		return 1
	}
	if targetVersion == "" {
		targetVersion = strings.TrimSpace(task.TargetVersion)
	}
	if !lib.IsValidReleaseTag(targetVersion) {
		p.appendLog("FATAL: Invalid target version: " + targetVersion)
		return 1
	}

	_ = os.MkdirAll(p.logDir, 0o755)
	claimed, err := db.TryMarkSystemUpdateTaskRunning(ctx, taskID, []models.SystemUpdateTaskStatus{models.SystemUpdateTaskStatusPending}, p.logPath)
	if err != nil {
		p.appendLog("FATAL: failed claiming update task: " + err.Error())
		return 1
	}
	if !claimed {
		p.appendLog(fmt.Sprintf("System update task %d is already claimed or finished; skipping duplicate worker", taskID))
		return 0
	}

	p.appendLog(fmt.Sprintf("Starting system update task %d -> %s", taskID, targetVersion))
	currentMeta := lib.GetCurrentVersionMetadata(ctx, p.appDir)
	_ = db.UpdateSystemUpdateTaskFromVersion(ctx, taskID, currentMeta.Version)

	timestamp := time.Now().UTC().Format("20060102150405")
	backupDir := fmt.Sprintf("%s.bak.%s", p.installDir, timestamp)
	backupPath := backupDir
	var dbBackupPath string
	var restartAttempted bool

	finishErr := func(err error) int {
		msg := err.Error()
		p.appendLog("ERROR: " + msg)
		rolledBack := false
		if rbOK, rbErr := p.autoRollbackFromBackup(ctx, taskID, backupPath, timestamp); rbErr != nil {
			p.appendLog("AUTO ROLLBACK ERROR: " + rbErr.Error())
		} else {
			rolledBack = rbOK
		}
		if rolledBack && restartAttempted && dbBackupPath != "" {
			p.appendLog(fmt.Sprintf(
				"WARNING: 服务重启时可能已触发 Goose 自动迁移，代码已回滚但 DB 不会自动回滚；如需恢复 DB，请使用 %s 手工恢复。该备份将保留。",
				dbBackupPath,
			))
		}
		status := models.SystemUpdateTaskStatusFailed
		finalErr := msg
		if rolledBack {
			status = models.SystemUpdateTaskStatusRolledBack
			finalErr = "Update failed and was auto-rolled back: " + msg
		}
		if len(finalErr) > 5000 {
			finalErr = finalErr[:5000]
		}
		_ = db.MarkSystemUpdateTaskFinished(ctx, taskID, db.MarkSystemUpdateTaskFinishedInput{
			Status:       status,
			BackupPath:   &backupPath,
			ErrorMessage: &finalErr,
		})
		_ = p.cleanupUpdateDownloadDir("更新失败后")
		return 1
	}

	var artifact *lib.OtaArtifactInfo
	if p.applyMode != "git" {
		otaInfo := lib.GetOtaReleaseInfo(ctx, p.httpClient, targetVersion)
		artifact = lib.SelectRuntimeArtifact(otaInfo.Artifacts)
		if artifact == nil {
			reason := fmt.Sprintf("no artifact for %s/%s", lib.RuntimePlatform(), lib.RuntimeArch())
			if otaInfo.Error != nil && *otaInfo.Error != "" {
				reason = *otaInfo.Error
			}
			if p.applyMode == "artifact" {
				return finishErr(fmt.Errorf("OTA artifact mode requested but no usable artifact is available: %s", reason))
			}
			p.appendLog("No usable OTA artifact found; falling back to Git build mode: " + reason)
		} else {
			p.appendLog(fmt.Sprintf("Using OTA artifact %s for %s/%s", artifact.Name, lib.RuntimePlatform(), lib.RuntimeArch()))
		}
	}

	if err := p.assertRequiredCommands(artifact != nil); err != nil {
		return finishErr(err)
	}
	if err := p.cleanupUpdateDownloadDir("更新开始前"); err != nil {
		return finishErr(err)
	}
	if err := p.assertEnoughDiskSpace("更新开始前", p.minDiskFreeMB); err != nil {
		return finishErr(err)
	}

	if artifact != nil {
		var artifactMB int64
		if artifact.Size != nil {
			artifactMB = (*artifact.Size + (1024*1024 - 1)) / (1024 * 1024)
		}
		reqMB := p.minDiskFreeMB + artifactMB*3
		if reqMB < p.minDiskFreeMB {
			reqMB = p.minDiskFreeMB
		}
		if err := p.assertEnoughDiskSpace("下载、解压 OTA 包并备份当前版本", reqMB); err != nil {
			return finishErr(err)
		}
		bp, err := p.applyArtifactUpdate(ctx, taskID, *artifact, targetVersion, backupDir, timestamp)
		if err != nil {
			return finishErr(err)
		}
		backupPath = bp
	} else {
		if err := p.assertEnoughDiskSpace("Git 构建并备份当前版本", p.minDiskFreeMB*2); err != nil {
			return finishErr(err)
		}
		bp, err := p.applyGitUpdate(ctx, targetVersion, backupDir)
		if err != nil {
			return finishErr(err)
		}
		backupPath = bp
	}

	dbBackupTarget := backupDir
	if p.isAtomicReleaseLayout() {
		dbBackupTarget = filepath.Join(p.releasesDir(), fmt.Sprintf("db-backups-%d-%s", taskID, timestamp))
	}
	if bp, err := p.backupDatabase(ctx, dbBackupTarget, targetVersion, timestamp); err != nil {
		return finishErr(err)
	} else {
		dbBackupPath = bp
	}

	restartAttempted = true
	if err := p.verifyUpdatedRuntime(ctx, artifact != nil); err != nil {
		return finishErr(err)
	}

	_ = db.MarkSystemUpdateTaskFinished(ctx, taskID, db.MarkSystemUpdateTaskFinishedInput{
		Status:     models.SystemUpdateTaskStatusSuccess,
		BackupPath: &backupPath,
	})
	p.appendLog("System update completed successfully")
	if err := p.cleanupUpdateDownloadDir("更新任务结束后"); err != nil {
		p.appendLog("OTA 下载缓存清理失败：" + err.Error())
	}
	if err := p.cleanupOldReleases(ctx, backupPath); err != nil {
		p.appendLog("旧 release 清理失败：" + err.Error())
	}
	return 0
}

// RunOTARollbackCommand executes an atomic/legacy rollback task (replaces rollback-system-update-task.ts).
func RunOTARollbackCommand(args []string) int {
	config.LoadEnv()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: incudal-server ota-rollback <taskId>")
		return 1
	}
	parsedID, err := strconv.ParseInt(args[0], 10, 32)
	if err != nil || parsedID <= 0 {
		fmt.Fprintln(os.Stderr, "Invalid task id")
		return 1
	}
	taskID := int32(parsedID)

	p := resolveOTAPaths(fmt.Sprintf("system-update-%d-rollback.log", taskID))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := db.Init(ctx); err != nil {
		p.appendLog("FATAL: database init failed: " + err.Error())
		return 1
	}
	defer db.Close()

	task, err := db.GetSystemUpdateTask(ctx, taskID)
	if err != nil || task == nil || task.BackupPath == nil || *task.BackupPath == "" {
		p.appendLog("FATAL: Task backup path is missing")
		return 1
	}
	backupPath, err := filepath.Abs(*task.BackupPath)
	if err != nil {
		p.appendLog("FATAL: invalid backup path")
		return 1
	}
	legacyPrefix, _ := filepath.Abs(p.installDir + ".bak.")
	releasesAbs, _ := filepath.Abs(p.releasesDir())
	isLegacy := strings.HasPrefix(backupPath, legacyPrefix)
	isAtomic := isSafeChildPath(releasesAbs, backupPath)
	if (!isLegacy && !isAtomic) || func() bool { _, e := os.Stat(backupPath); return e != nil }() {
		p.appendLog("FATAL: Backup path is invalid or does not exist: " + backupPath)
		return 1
	}

	claimed, err := db.TryMarkSystemUpdateTaskRunning(ctx, taskID, []models.SystemUpdateTaskStatus{
		models.SystemUpdateTaskStatusSuccess,
		models.SystemUpdateTaskStatusFailed,
	}, p.logPath)
	if err != nil {
		p.appendLog("FATAL: failed claiming rollback task: " + err.Error())
		return 1
	}
	if !claimed {
		p.appendLog(fmt.Sprintf("System rollback task %d is already claimed or not rollbackable; skipping duplicate worker", taskID))
		return 0
	}

	failRollback := func(err error) int {
		msg := err.Error()
		p.appendLog("ROLLBACK ERROR: " + msg)
		if len(msg) > 5000 {
			msg = msg[:5000]
		}
		_ = db.MarkSystemUpdateTaskFinished(ctx, taskID, db.MarkSystemUpdateTaskFinishedInput{
			Status:       models.SystemUpdateTaskStatusFailed,
			BackupPath:   task.BackupPath,
			ErrorMessage: &msg,
		})
		return 1
	}

	rollbackBackup := fmt.Sprintf("%s.pre-rollback.%s", p.installDir, time.Now().UTC().Format("20060102150405"))
	if isAtomic && p.isAtomicReleaseLayout() {
		if currentTarget := p.currentReleaseTarget(); currentTarget != "" {
			p.appendLog("Preserving current release before rollback: " + rollbackBackup)
			if err := copyTreePreserve(currentTarget, rollbackBackup); err != nil {
				return failRollback(err)
			}
		}
		if err := p.switchCurrentRelease(ctx, taskID, backupPath); err != nil {
			return failRollback(err)
		}
	} else {
		if isAtomic {
			return failRollback(fmt.Errorf("Refusing legacy rollback for an atomic-release backup while current layout is not an atomic symlink; manual recovery required to avoid deleting the install directory"))
		}
		p.appendLog("Preserving current install before rollback: " + rollbackBackup)
		_ = copyTreePreserve(p.installDir, rollbackBackup)
		if err := p.clearInstallDirPreserving([]string{"update-logs"}); err != nil {
			return failRollback(err)
		}
		if err := exec.Command("cp", "-a", backupPath+"/.", p.installDir).Run(); err != nil {
			return failRollback(err)
		}
	}

	_ = os.MkdirAll(filepath.Join(p.installDir, "server/certs"), 0o755)
	if err := p.chownInstallDir(ctx); err != nil {
		return failRollback(err)
	}
	if err := p.runCmd(ctx, 2*time.Minute, p.installDir, nil, "systemctl", "restart", p.serviceName); err != nil {
		return failRollback(err)
	}
	if err := p.waitForBackendHealth(ctx); err != nil {
		return failRollback(err)
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
		return failRollback(err)
	}

	_ = db.MarkSystemUpdateTaskFinished(ctx, taskID, db.MarkSystemUpdateTaskFinishedInput{
		Status:     models.SystemUpdateTaskStatusRolledBack,
		BackupPath: task.BackupPath,
	})
	p.appendLog("Rollback completed successfully")
	return 0
}
