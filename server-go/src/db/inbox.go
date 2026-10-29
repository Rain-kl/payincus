// Port of server/src/db/inbox.ts.
//
// Inbox messages are the in-app notification list. The TypeScript module returns
// plain projections (not Prisma models) with `createdAt` already converted via
// Date#toISOString and `data` left as the raw Json column, so the Go structs below
// mirror those projections rather than models.InboxMessage.
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

// CreateInboxMessageData mirrors the createInboxMessage() argument object.
//
// Data is a decoded JSON value; nil means "absent" and stores SQL NULL (the Node
// code passes `undefined`, which Prisma turns into the column default).
type CreateInboxMessageData struct {
	UserID    int32
	EventType string
	Title     string
	Content   string
	Data      any
}

// InboxMessage is the wire projection of a single inbox message.
type InboxMessage struct {
	ID        int32  `json:"id"`
	UserID    int32  `json:"userId"`
	EventType string `json:"eventType"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	IsRead    bool   `json:"isRead"`
	Data      any    `json:"data"`
	CreatedAt string `json:"createdAt"`
}

// PaginatedInboxMessages mirrors PaginatedInboxMessages.
type PaginatedInboxMessages struct {
	Messages   []InboxMessage `json:"messages"`
	Total      int64          `json:"total"`
	Page       int            `json:"page"`
	PageSize   int            `json:"pageSize"`
	TotalPages int64          `json:"totalPages"`
}

// InboxListOptions mirrors the getInboxMessages() options object. A nil IsRead
// means "no filter"; the zero page/pageSize mean "not provided".
type InboxListOptions struct {
	Page     int
	PageSize int
	IsRead   *bool
}

// HostNotificationRecipient mirrors the TypeScript interface of the same name.
type HostNotificationRecipient struct {
	UserID   int32   `json:"userId"`
	Username string  `json:"username"`
	Email    *string `json:"email"`
}

// CreateBulkInboxMessagesData mirrors createBulkMessages()'s argument object.
type CreateBulkInboxMessagesData struct {
	UserIDs   []int32
	EventType string
	Title     string
	Content   string
	Data      any
}

// inboxRow is the raw scan target for inbox_messages.
type inboxRow struct {
	ID        int32          `gorm:"column:id"`
	UserID    int32          `gorm:"column:user_id"`
	EventType string         `gorm:"column:event_type"`
	Title     string         `gorm:"column:title"`
	Content   string         `gorm:"column:content"`
	IsRead    bool           `gorm:"column:is_read"`
	Data      datatypes.JSON `gorm:"column:data"`
	CreatedAt time.Time      `gorm:"column:created_at"`
}

// clampInboxPagination mirrors clampInboxPagination. A zero value means
// "undefined"; note the Node version clamps an explicit pageSize of 0 to 1, which
// no caller ever produces.
func clampInboxPagination(page, pageSize int) (int, int) {
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

// encodeJSONColumn mirrors Prisma's InputJsonValue encoding: nil becomes SQL NULL,
// everything else is marshalled to JSON.
func encodeJSONColumn(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

// decodeJSONColumn mirrors Prisma's Json column read: JSON null stays nil.
func decodeJSONColumn(raw datatypes.JSON) (any, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func mapInboxRow(row inboxRow) (InboxMessage, error) {
	data, err := decodeJSONColumn(row.Data)
	if err != nil {
		return InboxMessage{}, err
	}
	return InboxMessage{
		ID:        row.ID,
		UserID:    row.UserID,
		EventType: row.EventType,
		Title:     row.Title,
		Content:   row.Content,
		IsRead:    row.IsRead,
		Data:      data,
		CreatedAt: formatISOTime(row.CreatedAt),
	}, nil
}

// ==================== 站内信 ====================

// CreateInboxMessage mirrors createInboxMessage(); it returns the new row id.
func CreateInboxMessage(ctx context.Context, data CreateInboxMessageData) (int32, error) {
	encodedData, err := encodeJSONColumn(data.Data)
	if err != nil {
		return 0, err
	}

	var id int32
	err = DB.WithContext(ctx).Raw(
		"INSERT INTO inbox_messages (user_id, event_type, title, content, data) VALUES (?, ?, ?, ?, ?) RETURNING id",
		data.UserID, data.EventType, data.Title, data.Content, encodedData,
	).Scan(&id).Error
	if err != nil {
		return 0, err
	}
	return id, nil
}

// GetInboxMessages mirrors getInboxMessages() (paginated, newest first).
func GetInboxMessages(ctx context.Context, userID int32, options InboxListOptions) (PaginatedInboxMessages, error) {
	page, pageSize := clampInboxPagination(options.Page, options.PageSize)

	where := "WHERE user_id = ?"
	args := []any{userID}
	if options.IsRead != nil {
		where += " AND is_read = ?"
		args = append(args, *options.IsRead)
	}

	var total int64
	if err := DB.WithContext(ctx).Raw("SELECT COUNT(*) FROM inbox_messages "+where, args...).Scan(&total).Error; err != nil {
		return PaginatedInboxMessages{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	var rows []inboxRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT id, user_id, event_type, title, content, is_read, data, created_at FROM inbox_messages "+
			where+" ORDER BY created_at DESC LIMIT ? OFFSET ?", listArgs...,
	).Scan(&rows).Error; err != nil {
		return PaginatedInboxMessages{}, err
	}

	messages := make([]InboxMessage, 0, len(rows))
	for _, row := range rows {
		message, err := mapInboxRow(row)
		if err != nil {
			return PaginatedInboxMessages{}, err
		}
		messages = append(messages, message)
	}

	return PaginatedInboxMessages{
		Messages:   messages,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages(total, pageSize),
	}, nil
}

// GetUnreadCount mirrors getUnreadCount().
func GetUnreadCount(ctx context.Context, userID int32) (int64, error) {
	var count int64
	err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM inbox_messages WHERE user_id = ? AND is_read = false", userID,
	).Scan(&count).Error
	return count, err
}

// MarkAsRead mirrors markAsRead(); it reports whether a row actually changed, so
// the route can answer 404 when the message is absent or owned by someone else.
func MarkAsRead(ctx context.Context, id, userID int32) (bool, error) {
	result := DB.WithContext(ctx).Exec(
		"UPDATE inbox_messages SET is_read = true WHERE id = ? AND user_id = ?", id, userID,
	)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// MarkAllAsRead mirrors markAllAsRead().
func MarkAllAsRead(ctx context.Context, userID int32) (int64, error) {
	result := DB.WithContext(ctx).Exec(
		"UPDATE inbox_messages SET is_read = true WHERE user_id = ? AND is_read = false", userID,
	)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// DeleteMessage mirrors deleteMessage().
func DeleteMessage(ctx context.Context, id, userID int32) (bool, error) {
	result := DB.WithContext(ctx).Exec(
		"DELETE FROM inbox_messages WHERE id = ? AND user_id = ?", id, userID,
	)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// DeleteReadMessages mirrors deleteReadMessages().
func DeleteReadMessages(ctx context.Context, userID int32) (int64, error) {
	result := DB.WithContext(ctx).Exec(
		"DELETE FROM inbox_messages WHERE user_id = ? AND is_read = true", userID,
	)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// CleanupOldMessages mirrors cleanupOldMessages(daysToKeep = 30).
func CleanupOldMessages(ctx context.Context, daysToKeep int) (int64, error) {
	if daysToKeep == 0 {
		daysToKeep = 30
	}
	cutoffDate := time.Now().AddDate(0, 0, -daysToKeep)

	result := DB.WithContext(ctx).Exec("DELETE FROM inbox_messages WHERE created_at < ?", cutoffDate)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// GetMessageByID mirrors getMessageById(); it returns nil when the message does
// not exist or belongs to a different user.
func GetMessageByID(ctx context.Context, id, userID int32) (*InboxMessage, error) {
	var rows []inboxRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT id, user_id, event_type, title, content, is_read, data, created_at FROM inbox_messages "+
			"WHERE id = ? AND user_id = ? LIMIT 1", id, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	message, err := mapInboxRow(rows[0])
	if err != nil {
		return nil, err
	}
	return &message, nil
}

// createBulkMessagesChunkSize keeps a single INSERT below PostgreSQL's bind
// parameter limit (5 params per row).
const createBulkMessagesChunkSize = 1000

// CreateBulkMessages mirrors createBulkMessages(); it returns the number of rows
// inserted.
func CreateBulkMessages(ctx context.Context, data CreateBulkInboxMessagesData) (int64, error) {
	if len(data.UserIDs) == 0 {
		return 0, nil
	}

	encodedData, err := encodeJSONColumn(data.Data)
	if err != nil {
		return 0, err
	}

	var inserted int64
	for start := 0; start < len(data.UserIDs); start += createBulkMessagesChunkSize {
		end := start + createBulkMessagesChunkSize
		if end > len(data.UserIDs) {
			end = len(data.UserIDs)
		}
		chunk := data.UserIDs[start:end]

		placeholders := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*5)
		for _, userID := range chunk {
			placeholders = append(placeholders, "(?, ?, ?, ?, ?)")
			args = append(args, userID, data.EventType, data.Title, data.Content, encodedData)
		}

		result := DB.WithContext(ctx).Exec(
			"INSERT INTO inbox_messages (user_id, event_type, title, content, data) VALUES "+
				strings.Join(placeholders, ", "), args...,
		)
		if result.Error != nil {
			return inserted, result.Error
		}
		inserted += result.RowsAffected
	}

	return inserted, nil
}

// GetAllActiveUserIds mirrors getAllActiveUserIds().
func GetAllActiveUserIds(ctx context.Context) ([]int32, error) {
	var rows []struct {
		ID int32 `gorm:"column:id"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT id FROM users WHERE status = ?", string(models.UserStatusActive),
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	ids := make([]int32, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// GetUniqueUserIdsByHostId mirrors getUniqueUserIdsByHostId().
func GetUniqueUserIdsByHostId(ctx context.Context, hostID int32) ([]int32, error) {
	var rows []struct {
		UserID int32 `gorm:"column:user_id"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT DISTINCT user_id FROM instances WHERE host_id = ? AND status <> ?",
		hostID, string(models.InstanceStatusDeleted),
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	ids := make([]int32, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.UserID)
	}
	return ids, nil
}

// GetUniqueUserIdsByInstanceIds mirrors getUniqueUserIdsByInstanceIds(). An empty
// instanceIds slice matches nothing, exactly like Prisma's `id: { in: [] }`.
func GetUniqueUserIdsByInstanceIds(ctx context.Context, hostID int32, instanceIDs []int32) ([]int32, error) {
	if len(instanceIDs) == 0 {
		return []int32{}, nil
	}

	placeholders, args := int32Placeholders(instanceIDs)
	args = append(args, hostID, string(models.InstanceStatusDeleted))

	var rows []struct {
		UserID int32 `gorm:"column:user_id"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT DISTINCT user_id FROM instances WHERE id IN ("+placeholders+") AND host_id = ? AND status <> ?",
		args...,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	ids := make([]int32, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.UserID)
	}
	return ids, nil
}

// int32Placeholders renders an IN (...) list of bind placeholders plus the
// matching args, avoiding a driver-specific array binding.
func int32Placeholders(values []int32) (string, []any) {
	placeholders := make([]string, 0, len(values))
	args := make([]any, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, "?")
		args = append(args, value)
	}
	return strings.Join(placeholders, ", "), args
}

// GetUniqueRecipientsByHostId mirrors getUniqueRecipientsByHostId(), including the
// `orderBy userId asc` and the optional instance filter (which is ignored when the
// slice is empty).
func GetUniqueRecipientsByHostId(ctx context.Context, hostID int32, instanceIDs []int32) ([]HostNotificationRecipient, error) {
	sql := "SELECT i.user_id, u.username, u.email FROM instances i " +
		"JOIN users u ON u.id = i.user_id WHERE i.host_id = ? AND i.status <> ?"
	args := []any{hostID, string(models.InstanceStatusDeleted)}
	if len(instanceIDs) > 0 {
		placeholders, instanceArgs := int32Placeholders(instanceIDs)
		sql += " AND i.id IN (" + placeholders + ")"
		args = append(args, instanceArgs...)
	}
	sql += " GROUP BY i.user_id, u.username, u.email ORDER BY i.user_id ASC"

	var rows []struct {
		UserID   int32   `gorm:"column:user_id"`
		Username string  `gorm:"column:username"`
		Email    *string `gorm:"column:email"`
	}
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	recipients := make([]HostNotificationRecipient, 0, len(rows))
	for _, row := range rows {
		recipients = append(recipients, HostNotificationRecipient{
			UserID:   row.UserID,
			Username: row.Username,
			Email:    row.Email,
		})
	}
	return recipients, nil
}
