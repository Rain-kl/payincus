// Port of server/src/db/system-update-tasks.ts.
package db

import (
	"context"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// SystemUpdateTaskWithUser is the Prisma row plus
// `include: { startedBy: { select: { username: true } } }`.
type SystemUpdateTaskWithUser struct {
	models.SystemUpdateTask
	StartedByUsername *string
}

// systemUpdateTaskUsernames resolves started_by_user_id → username in one query,
// like Prisma's relation include.
func systemUpdateTaskUsernames(ctx context.Context, tasks []models.SystemUpdateTask) (map[int32]string, error) {
	usernames := make(map[int32]string)
	if len(tasks) == 0 {
		return usernames, nil
	}

	userIDs := make([]int32, 0, len(tasks))
	seen := make(map[int32]bool, len(tasks))
	for _, task := range tasks {
		if !seen[task.StartedByUserID] {
			seen[task.StartedByUserID] = true
			userIDs = append(userIDs, task.StartedByUserID)
		}
	}

	var users []struct {
		ID       int32  `gorm:"column:id"`
		Username string `gorm:"column:username"`
	}
	if err := DB.WithContext(ctx).
		Table("users").
		Select("id, username").
		Where("id IN ?", userIDs).
		Find(&users).Error; err != nil {
		return nil, err
	}
	for _, user := range users {
		usernames[user.ID] = user.Username
	}
	return usernames, nil
}

// systemUpdateTaskWithUser builds one relation-resolved row.
func systemUpdateTaskWithUser(task models.SystemUpdateTask, usernames map[int32]string) SystemUpdateTaskWithUser {
	row := SystemUpdateTaskWithUser{SystemUpdateTask: task}
	if username, found := usernames[task.StartedByUserID]; found {
		value := username
		row.StartedByUsername = &value
	}
	return row
}

// ListSystemUpdateTasks mirrors listSystemUpdateTasks(limit): newest first,
// `take: limit`, with the starter's username attached.
func ListSystemUpdateTasks(ctx context.Context, limit int) ([]SystemUpdateTaskWithUser, error) {
	var tasks []models.SystemUpdateTask
	if err := DB.WithContext(ctx).
		Order("created_at DESC").
		Limit(limit).
		Find(&tasks).Error; err != nil {
		return nil, err
	}

	usernames, err := systemUpdateTaskUsernames(ctx, tasks)
	if err != nil {
		return nil, err
	}

	rows := make([]SystemUpdateTaskWithUser, 0, len(tasks))
	for _, task := range tasks {
		rows = append(rows, systemUpdateTaskWithUser(task, usernames))
	}
	return rows, nil
}

// GetSystemUpdateTask mirrors getSystemUpdateTask(id): nil when the row is
// absent. Prisma's findUnique is a primary-key lookup; the zero ID is never a
// valid row, so it doubles as the not-found marker.
func GetSystemUpdateTask(ctx context.Context, id int32) (*SystemUpdateTaskWithUser, error) {
	var task models.SystemUpdateTask
	if err := DB.WithContext(ctx).
		Where("id = ?", id).
		Limit(1).
		Find(&task).Error; err != nil {
		return nil, err
	}
	if task.ID == 0 {
		return nil, nil
	}

	usernames, err := systemUpdateTaskUsernames(ctx, []models.SystemUpdateTask{task})
	if err != nil {
		return nil, err
	}
	row := systemUpdateTaskWithUser(task, usernames)
	return &row, nil
}

// HasRunningSystemUpdateTask mirrors hasRunningSystemUpdateTask: true while any
// task is pending or running (the updater is exclusive).
func HasRunningSystemUpdateTask(ctx context.Context) (bool, error) {
	var count int64
	if err := DB.WithContext(ctx).
		Model(&models.SystemUpdateTask{}).
		Where("status IN ?", []string{
			string(models.SystemUpdateTaskStatusPending),
			string(models.SystemUpdateTaskStatusRunning),
		}).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// CreateSystemUpdateTaskInput mirrors createSystemUpdateTask's argument object.
type CreateSystemUpdateTaskInput struct {
	TargetVersion   string
	FromVersion     *string
	StartedByUserID int32
	LogPath         string
}

// CreateSystemUpdateTask mirrors createSystemUpdateTask: inserts a pending
// update row and resolves StartedByUsername for the immediate 202 response.
func CreateSystemUpdateTask(ctx context.Context, input CreateSystemUpdateTaskInput) (*SystemUpdateTaskWithUser, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	logPath := input.LogPath
	row := models.SystemUpdateTask{
		TargetVersion:   input.TargetVersion,
		FromVersion:     input.FromVersion,
		Status:          models.SystemUpdateTaskStatusPending,
		StartedByUserID: input.StartedByUserID,
		LogPath:         &logPath,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	usernames, err := systemUpdateTaskUsernames(ctx, []models.SystemUpdateTask{row})
	if err != nil {
		return nil, err
	}
	withUser := systemUpdateTaskWithUser(row, usernames)
	return &withUser, nil
}

// UpdateSystemUpdateTaskLogPath updates a newly created task's log_path from the
// pending placeholder to `system-update-<id>.log` so /tasks/:id/logs can tail it
// immediately without waiting for the worker process to claim the row.
func UpdateSystemUpdateTaskLogPath(ctx context.Context, id int32, logPath string) error {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return DB.WithContext(ctx).
		Model(&models.SystemUpdateTask{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"log_path":   logPath,
			"updated_at": now,
		}).Error
}

// MarkSystemUpdateTaskFinishedInput mirrors markSystemUpdateTaskFinished's payload.
type MarkSystemUpdateTaskFinishedInput struct {
	Status       models.SystemUpdateTaskStatus
	ErrorMessage *string
	BackupPath   *string
}

// MarkSystemUpdateTaskFinished mirrors markSystemUpdateTaskFinished.
func MarkSystemUpdateTaskFinished(ctx context.Context, id int32, input MarkSystemUpdateTaskFinishedInput) error {
	now := time.Now().UTC().Truncate(time.Millisecond)
	updates := map[string]any{
		"status":        string(input.Status),
		"error_message": input.ErrorMessage,
		"backup_path":   input.BackupPath,
		"finished_at":   now,
		"updated_at":    now,
	}
	return DB.WithContext(ctx).
		Model(&models.SystemUpdateTask{}).
		Where("id = ?", id).
		Updates(updates).Error
}

// UpdateSystemUpdateTaskFromVersion records the pre-upgrade version detected at worker start.
func UpdateSystemUpdateTaskFromVersion(ctx context.Context, id int32, fromVersion string) error {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return DB.WithContext(ctx).
		Model(&models.SystemUpdateTask{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"from_version": fromVersion,
			"updated_at":   now,
		}).Error
}

// TryMarkSystemUpdateTaskRunning atomically transitions a task from one of allowedStatuses to running.
func TryMarkSystemUpdateTaskRunning(ctx context.Context, id int32, allowedStatuses []models.SystemUpdateTaskStatus, logPath string) (bool, error) {
	if len(allowedStatuses) == 0 {
		return false, nil
	}
	allowed := make([]string, 0, len(allowedStatuses))
	for _, s := range allowedStatuses {
		allowed = append(allowed, string(s))
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	res := DB.WithContext(ctx).
		Model(&models.SystemUpdateTask{}).
		Where("id = ? AND status IN ?", id, allowed).
		Updates(map[string]any{
			"status":        string(models.SystemUpdateTaskStatusRunning),
			"log_path":      logPath,
			"error_message": nil,
			"finished_at":   nil,
			"updated_at":    now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ListRecentProtectedBackupPaths returns recent backup_path values still protected against pruning.
func ListRecentProtectedBackupPaths(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	var tasks []models.SystemUpdateTask
	if err := DB.WithContext(ctx).
		Select("backup_path").
		Where("backup_path IS NOT NULL AND status IN ?", []string{
			string(models.SystemUpdateTaskStatusSuccess),
			string(models.SystemUpdateTaskStatusRolledBack),
		}).
		Order("finished_at DESC, id DESC").
		Limit(limit).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if task.BackupPath != nil && *task.BackupPath != "" {
			paths = append(paths, *task.BackupPath)
		}
	}
	return paths, nil
}

// FindFirstActiveAdminUserID resolves an active administrator ID for CLI-triggered OTA updates.
func FindFirstActiveAdminUserID(ctx context.Context, preferredID int32) (int32, error) {
	var user models.User
	if preferredID > 0 {
		if err := DB.WithContext(ctx).
			Select("id").
			Where("id = ? AND role = ? AND status = ?", preferredID, string(models.UserRoleAdmin), string(models.UserStatusActive)).
			Limit(1).
			Find(&user).Error; err != nil {
			return 0, err
		}
		if user.ID > 0 {
			return user.ID, nil
		}
	}
	if err := DB.WithContext(ctx).
		Select("id").
		Where("role = ? AND status = ?", string(models.UserRoleAdmin), string(models.UserStatusActive)).
		Order("id ASC").
		Limit(1).
		Find(&user).Error; err != nil {
		return 0, err
	}
	return user.ID, nil
}
