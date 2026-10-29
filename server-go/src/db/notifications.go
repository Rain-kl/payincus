// Port of server/src/db/notifications.ts.
//
// The TypeScript module deliberately returns *shapes*, not Prisma models: config
// is re-serialised with JSON.stringify and enabled is narrowed to 1/0, so the wire
// format differs from the Prisma model (see types/database.ts NotificationChannel).
// The structs below mirror that TypeScript interface field-for-field.
package db

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== 类型 ====================

// NotificationChannelRow mirrors the TypeScript NotificationChannel interface
// (snake_case keys, config as a JSON *string*, enabled as 1/0).
type NotificationChannelRow struct {
	ID        int32                          `json:"id"`
	UserID    int32                          `json:"user_id"`
	Type      models.NotificationChannelType `json:"type"`
	Name      string                         `json:"name"`
	Config    string                         `json:"config"`
	Enabled   int                            `json:"enabled"`
	CreatedAt string                         `json:"created_at"`
}

// CreateNotificationChannelData mirrors createNotificationChannel()'s argument.
// Enabled follows the Node `data.enabled !== false` default: a nil pointer means
// "not provided" and stores true.
type CreateNotificationChannelData struct {
	UserID  int32
	Type    models.NotificationChannelType
	Name    string
	Config  any
	Enabled *bool
}

// UpdateNotificationChannelData mirrors updateNotificationChannel()'s argument.
// Nil pointers mean "key absent" and leave the column untouched.
type UpdateNotificationChannelData struct {
	Name           *string
	Config         any
	ConfigProvided bool
	Enabled        *bool
}

// CreateNotificationLogData mirrors createNotificationLog()'s argument.
type CreateNotificationLogData struct {
	ChannelID int32
	EventType string
	Message   string
	Status    models.NotificationLogStatus
	Error     *string
}

// NotificationLogItem is one row of getNotificationLogsByUserId().
type NotificationLogItem struct {
	ID          int32   `json:"id"`
	ChannelID   int32   `json:"channelId"`
	ChannelName string  `json:"channelName"`
	ChannelType string  `json:"channelType"`
	EventType   string  `json:"eventType"`
	Message     string  `json:"message"`
	Status      string  `json:"status"`
	Error       *string `json:"error"`
	CreatedAt   string  `json:"createdAt"`
}

// NotificationLogPage mirrors the getNotificationLogsByUserId() envelope.
type NotificationLogPage struct {
	Logs     []NotificationLogItem `json:"logs"`
	Total    int64                 `json:"total"`
	Page     int                   `json:"page"`
	PageSize int                   `json:"pageSize"`
}

// NotificationLogListOptions mirrors the options object of
// getNotificationLogsByUserId(). Status must already be a valid status value;
// anything else means "no filter".
type NotificationLogListOptions struct {
	Page     int
	PageSize int
	Status   string
}

// NotificationStats mirrors getNotificationStats().
type NotificationStats struct {
	Total   int64 `json:"total"`
	Sent    int64 `json:"sent"`
	Failed  int64 `json:"failed"`
	Pending int64 `json:"pending"`
}

// notificationLogStatuses mirrors the Set in the TypeScript module.
var notificationLogStatuses = map[string]bool{
	string(models.NotificationLogStatusPending): true,
	string(models.NotificationLogStatusSent):    true,
	string(models.NotificationLogStatusFailed):  true,
}

// NormalizeNotificationLogStatus mirrors normalizeNotificationLogStatus.
func NormalizeNotificationLogStatus(status string) string {
	if status != "" && notificationLogStatuses[status] {
		return status
	}
	return ""
}

// clampNotificationLogPagination mirrors clampNotificationLogPagination.
func clampNotificationLogPagination(page, pageSize int) (int, int) {
	clampedPage := 1
	if page > 0 {
		clampedPage = page
	}

	clampedPageSize := 20
	if pageSize != 0 {
		clampedPageSize = pageSize
		if clampedPageSize < 1 {
			clampedPageSize = 1
		}
		if clampedPageSize > 100 {
			clampedPageSize = 100
		}
	}
	return clampedPage, clampedPageSize
}

// notificationChannelRow is the raw scan target for notification_channels.
type notificationChannelRow struct {
	ID        int32          `gorm:"column:id"`
	UserID    int32          `gorm:"column:user_id"`
	Type      string         `gorm:"column:type"`
	Name      string         `gorm:"column:name"`
	Config    datatypes.JSON `gorm:"column:config"`
	Enabled   bool           `gorm:"column:enabled"`
	CreatedAt time.Time      `gorm:"column:created_at"`
}

const notificationChannelSelectColumns = `id, user_id, type::text AS type, name, config, enabled, created_at`

// mapNotificationChannelRow mirrors the mapping applied by every getter in the
// TypeScript module: JSON.stringify(config), enabled -> 1/0, toISOString().
func mapNotificationChannelRow(row notificationChannelRow) (NotificationChannelRow, error) {
	config, err := stringifyJSONColumn(row.Config)
	if err != nil {
		return NotificationChannelRow{}, err
	}

	enabled := 0
	if row.Enabled {
		enabled = 1
	}

	return NotificationChannelRow{
		ID:        row.ID,
		UserID:    row.UserID,
		Type:      models.NotificationChannelType(row.Type),
		Name:      row.Name,
		Config:    config,
		Enabled:   enabled,
		CreatedAt: formatISOTime(row.CreatedAt),
	}, nil
}

// stringifyJSONColumn mirrors JSON.stringify(config): a SQL NULL becomes the
// literal "null", which is what the Node code produces for a null Json column.
func stringifyJSONColumn(raw datatypes.JSON) (string, error) {
	decoded, err := decodeJSONColumn(raw)
	if err != nil {
		return "", err
	}
	if decoded == nil {
		return "null", nil
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ==================== 通知渠道 ====================

// GetNotificationChannelsByUserId mirrors getNotificationChannelsByUserId()
// (newest first).
func GetNotificationChannelsByUserId(ctx context.Context, userID int32) ([]NotificationChannelRow, error) {
	var rows []notificationChannelRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT "+notificationChannelSelectColumns+" FROM notification_channels "+
			"WHERE user_id = ? ORDER BY created_at DESC", userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	channels := make([]NotificationChannelRow, 0, len(rows))
	for _, row := range rows {
		channel, err := mapNotificationChannelRow(row)
		if err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	return channels, nil
}

// GetNotificationChannelById mirrors getNotificationChannelById(); nil when absent.
// The id is int64 because it comes straight from the route param: casting it to
// int32 would silently truncate ids above 2^31-1 and match the wrong row.
func GetNotificationChannelById(ctx context.Context, id int64) (*NotificationChannelRow, error) {
	var rows []notificationChannelRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT "+notificationChannelSelectColumns+" FROM notification_channels WHERE id = ? LIMIT 1", id,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	channel, err := mapNotificationChannelRow(rows[0])
	if err != nil {
		return nil, err
	}
	return &channel, nil
}

// CreateNotificationChannel mirrors createNotificationChannel(); it returns the
// new channel id.
func CreateNotificationChannel(ctx context.Context, data CreateNotificationChannelData) (int32, error) {
	enabled := true
	if data.Enabled != nil {
		enabled = *data.Enabled
	}

	encodedConfig, err := encodeJSONColumn(data.Config)
	if err != nil {
		return 0, err
	}

	var id int32
	err = DB.WithContext(ctx).Raw(
		"INSERT INTO notification_channels (user_id, type, name, config, enabled) VALUES (?, ?, ?, ?, ?) RETURNING id",
		data.UserID, string(data.Type), data.Name, encodedConfig, enabled,
	).Scan(&id).Error
	if err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateNotificationChannel mirrors updateNotificationChannel(); an empty option
// set is a no-op (Prisma never sees the call).
func UpdateNotificationChannel(ctx context.Context, id int64, data UpdateNotificationChannelData) error {
	sets := make([]string, 0, 3)
	args := make([]any, 0, 4)

	if data.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *data.Name)
	}
	if data.ConfigProvided {
		encodedConfig, err := encodeJSONColumn(data.Config)
		if err != nil {
			return err
		}
		sets = append(sets, "config = ?")
		args = append(args, encodedConfig)
	}
	if data.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, *data.Enabled)
	}

	if len(sets) == 0 {
		return nil
	}

	args = append(args, id)
	return DB.WithContext(ctx).Exec(
		"UPDATE notification_channels SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...,
	).Error
}

// DeleteNotificationChannel mirrors deleteNotificationChannel(). Prisma throws
// P2025 when the row is gone; the callers always look the row up first, so here a
// missing row is simply a no-op.
func DeleteNotificationChannel(ctx context.Context, id int64) error {
	return DB.WithContext(ctx).Exec("DELETE FROM notification_channels WHERE id = ?", id).Error
}

// GetEnabledChannelsByUserId mirrors getEnabledChannelsByUserId(): enabled user
// channels only (isGlobal = false), without an explicit ORDER BY.
func GetEnabledChannelsByUserId(ctx context.Context, userID int32) ([]NotificationChannelRow, error) {
	var rows []notificationChannelRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT "+notificationChannelSelectColumns+" FROM notification_channels "+
			"WHERE user_id = ? AND enabled = true AND is_global = false", userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	channels := make([]NotificationChannelRow, 0, len(rows))
	for _, row := range rows {
		channel, err := mapNotificationChannelRow(row)
		if err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	return channels, nil
}

// ==================== 通知日志 ====================

// CreateNotificationLog mirrors createNotificationLog(); it returns the new id.
func CreateNotificationLog(ctx context.Context, data CreateNotificationLogData) (int32, error) {
	var id int32
	err := DB.WithContext(ctx).Raw(
		"INSERT INTO notification_logs (channel_id, event_type, message, status, error) VALUES (?, ?, ?, ?, ?) RETURNING id",
		data.ChannelID, data.EventType, data.Message, string(data.Status), nullableString(data.Error),
	).Scan(&id).Error
	if err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateNotificationLogStatus mirrors updateNotificationLogStatus(); error is
// always written (a nil pointer stores SQL NULL).
func UpdateNotificationLogStatus(ctx context.Context, id int32, status models.NotificationLogStatus, errMessage *string) error {
	return DB.WithContext(ctx).Exec(
		"UPDATE notification_logs SET status = ?, error = ? WHERE id = ?", string(status), nullableString(errMessage), id,
	).Error
}

// getNotificationLogsByUserId mirrors getNotificationLogsByUserId(), including
// the inner join on the owning channel.
func GetNotificationLogsByUserId(ctx context.Context, userID int32, options NotificationLogListOptions) (NotificationLogPage, error) {
	page, pageSize := clampNotificationLogPagination(options.Page, options.PageSize)

	where := "WHERE c.user_id = ?"
	args := []any{userID}
	if status := NormalizeNotificationLogStatus(options.Status); status != "" {
		where += " AND l.status = ?"
		args = append(args, status)
	}

	from := " FROM notification_logs l JOIN notification_channels c ON c.id = l.channel_id "

	var total int64
	if err := DB.WithContext(ctx).Raw("SELECT COUNT(*)"+from+where, args...).Scan(&total).Error; err != nil {
		return NotificationLogPage{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	var rows []struct {
		ID          int32     `gorm:"column:id"`
		ChannelID   int32     `gorm:"column:channel_id"`
		ChannelName string    `gorm:"column:channel_name"`
		ChannelType string    `gorm:"column:channel_type"`
		EventType   string    `gorm:"column:event_type"`
		Message     string    `gorm:"column:message"`
		Status      string    `gorm:"column:status"`
		Error       *string   `gorm:"column:error"`
		CreatedAt   time.Time `gorm:"column:created_at"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT l.id, l.channel_id, c.name AS channel_name, c.type::text AS channel_type,"+
			" l.event_type, l.message, l.status::text AS status, l.error, l.created_at"+
			from+where+" ORDER BY l.created_at DESC LIMIT ? OFFSET ?", listArgs...,
	).Scan(&rows).Error; err != nil {
		return NotificationLogPage{}, err
	}

	logs := make([]NotificationLogItem, 0, len(rows))
	for _, row := range rows {
		logs = append(logs, NotificationLogItem{
			ID:          row.ID,
			ChannelID:   row.ChannelID,
			ChannelName: row.ChannelName,
			ChannelType: row.ChannelType,
			EventType:   row.EventType,
			Message:     row.Message,
			Status:      row.Status,
			Error:       row.Error,
			CreatedAt:   formatISOTime(row.CreatedAt),
		})
	}

	return NotificationLogPage{
		Logs:     logs,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}, nil
}

// GetNotificationStats mirrors getNotificationStats().
func GetNotificationStats(ctx context.Context, userID int32) (NotificationStats, error) {
	const from = " FROM notification_logs l JOIN notification_channels c ON c.id = l.channel_id WHERE c.user_id = ?"

	var stats NotificationStats
	if err := DB.WithContext(ctx).Raw("SELECT COUNT(*)"+from, userID).Scan(&stats.Total).Error; err != nil {
		return NotificationStats{}, err
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*)"+from+" AND l.status = ?", userID, string(models.NotificationLogStatusSent),
	).Scan(&stats.Sent).Error; err != nil {
		return NotificationStats{}, err
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*)"+from+" AND l.status = ?", userID, string(models.NotificationLogStatusFailed),
	).Scan(&stats.Failed).Error; err != nil {
		return NotificationStats{}, err
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*)"+from+" AND l.status = ?", userID, string(models.NotificationLogStatusPending),
	).Scan(&stats.Pending).Error; err != nil {
		return NotificationStats{}, err
	}
	return stats, nil
}

// CleanOldNotificationLogs mirrors cleanOldNotificationLogs(days = 30).
func CleanOldNotificationLogs(ctx context.Context, days int) (int64, error) {
	if days == 0 {
		days = 30
	}
	cutoffDate := time.Now().AddDate(0, 0, -days)

	result := DB.WithContext(ctx).Exec("DELETE FROM notification_logs WHERE created_at < ?", cutoffDate)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// GlobalNotificationChannel mirrors the select projection used by the
// /api/notifications/global-channels route.
type GlobalNotificationChannel struct {
	ID     int32          `json:"id"`
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Config datatypes.JSON `json:"config"`
}

// GetGlobalNotificationChannels mirrors
// `prisma.notificationChannel.findMany({ where: { isGlobal: true, enabled: true },
// orderBy: { createdAt: 'asc' }, select: { id, name, type, config } })`.
func GetGlobalNotificationChannels(ctx context.Context) ([]GlobalNotificationChannel, error) {
	var channels []GlobalNotificationChannel
	if err := DB.WithContext(ctx).Raw(
		"SELECT id, name, type::text AS type, config FROM notification_channels " +
			"WHERE is_global = true AND enabled = true ORDER BY created_at ASC",
	).Scan(&channels).Error; err != nil {
		return nil, err
	}
	return channels, nil
}
