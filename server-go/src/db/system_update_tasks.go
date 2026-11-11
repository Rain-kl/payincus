// Port of the READ half of server/src/db/system-update-tasks.ts.
//
// OTA is the highest-risk path in this repo (AGENTS.md §2.1) and the Go backend
// runs beside the live Node backend while the migration is in progress. The Go
// routes therefore only *observe* update history; creating, marking running and
// finishing tasks stay in Node. This file mirrors exactly the three read helpers
// that the read-only routes need:
//
//	listSystemUpdateTasks   → ListSystemUpdateTasks
//	getSystemUpdateTask     → GetSystemUpdateTask
//	hasRunningSystemUpdateTask → HasRunningSystemUpdateTask
//
// The write helpers (createSystemUpdateTask / markSystemUpdateTaskRunning /
// tryMarkSystemUpdateTaskRunning / markSystemUpdateTaskFinished) are deliberately
// NOT ported yet: nothing in the Go tree may start an OTA, so a write helper would
// be dead code with an inviting name. If the owner wires OTA after the traffic
// switch, port them together with the updater spawn path.
package db

import (
	"context"

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
