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
