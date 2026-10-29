// Port of server/src/db/announcements.ts.
package db

import (
	"context"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// AnnouncementSenderRef is the nested sender projection.
type AnnouncementSenderRef struct {
	ID       int32  `json:"id"`
	Username string `json:"username"`
}

// AnnouncementHostRef is the nested host projection.
type AnnouncementHostRef struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// AnnouncementAdminItem matches the admin list projection (includes sender + host).
type AnnouncementAdminItem struct {
	ID             int32                   `json:"id"`
	Type           models.AnnouncementType `json:"type"`
	Title          string                  `json:"title"`
	Content        string                  `json:"content"`
	RecipientCount int32                   `json:"recipientCount"`
	HostID         *int32                  `json:"hostId"`
	TargetUserID   *int32                  `json:"targetUserId"`
	InstanceID     *int32                  `json:"instanceId"`
	CreatedAt      time.Time               `json:"createdAt"`
	Sender         *AnnouncementSenderRef  `json:"sender"`
	Host           *AnnouncementHostRef    `json:"host"`
}

// AnnouncementHostOwnerItem matches the host-owner projection (includes host only).
type AnnouncementHostOwnerItem struct {
	ID             int32                   `json:"id"`
	Type           models.AnnouncementType `json:"type"`
	Title          string                  `json:"title"`
	Content        string                  `json:"content"`
	RecipientCount int32                   `json:"recipientCount"`
	HostID         *int32                  `json:"hostId"`
	TargetUserID   *int32                  `json:"targetUserId"`
	InstanceID     *int32                  `json:"instanceId"`
	CreatedAt      time.Time               `json:"createdAt"`
	Host           *AnnouncementHostRef    `json:"host"`
}

// AnnouncementDetail matches getAnnouncementById (all columns + sender + host).
type AnnouncementDetail struct {
	ID             int32                   `json:"id"`
	Type           models.AnnouncementType `json:"type"`
	SenderID       int32                   `json:"senderId"`
	Title          string                  `json:"title"`
	Content        string                  `json:"content"`
	RecipientCount int32                   `json:"recipientCount"`
	HostID         *int32                  `json:"hostId"`
	TargetUserID   *int32                  `json:"targetUserId"`
	InstanceID     *int32                  `json:"instanceId"`
	CreatedAt      time.Time               `json:"createdAt"`
	Sender         *AnnouncementSenderRef  `json:"sender"`
	Host           *AnnouncementHostRef    `json:"host"`
}

// AnnouncementListOptions mirrors the Node list options.
type AnnouncementListOptions struct {
	Page     int
	PageSize int
	Type     string
	SenderID int64
	HostID   int64
}

// AnnouncementPage is the paginated list envelope.
type AnnouncementPage[T any] struct {
	Items      []T   `json:"items"`
	Total      int64 `json:"total"`
	Page       int   `json:"page"`
	PageSize   int   `json:"pageSize"`
	TotalPages int64 `json:"totalPages"`
}

type announcementRow struct {
	ID             int32     `gorm:"column:id"`
	Type           string    `gorm:"column:type"`
	Title          string    `gorm:"column:title"`
	Content        string    `gorm:"column:content"`
	RecipientCount int32     `gorm:"column:recipient_count"`
	HostID         *int32    `gorm:"column:host_id"`
	TargetUserID   *int32    `gorm:"column:target_user_id"`
	InstanceID     *int32    `gorm:"column:instance_id"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	SenderID       *int32    `gorm:"column:sender_id"`
	SenderUsername *string   `gorm:"column:sender_username"`
	HostRefID      *int32    `gorm:"column:host_ref_id"`
	HostRefName    *string   `gorm:"column:host_ref_name"`
}

const announcementSelectColumns = `
	a.id, a.type::text AS type, a.title, a.content, a.recipient_count,
	a.host_id, a.target_user_id, a.instance_id, a.created_at,
	s.id AS sender_id, s.username AS sender_username,
	h.id AS host_ref_id, h.name AS host_ref_name`

// GetAnnouncementList mirrors getAnnouncementList().
func GetAnnouncementList(ctx context.Context, options AnnouncementListOptions) (AnnouncementPage[AnnouncementAdminItem], error) {
	page := options.Page
	if page < 1 {
		page = 1
	}
	pageSize := options.PageSize
	if pageSize < 1 {
		pageSize = 20
	}

	where := "WHERE 1=1"
	args := []any{}
	if options.Type != "" {
		where += " AND a.type = ?"
		args = append(args, options.Type)
	}
	if options.SenderID != 0 {
		where += " AND a.sender_id = ?"
		args = append(args, options.SenderID)
	}

	var total int64
	countSQL := "SELECT COUNT(*) FROM announcements a " + where
	if err := DB.WithContext(ctx).Raw(countSQL, args...).Scan(&total).Error; err != nil {
		return AnnouncementPage[AnnouncementAdminItem]{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	listSQL := "SELECT " + announcementSelectColumns +
		" FROM announcements a" +
		" LEFT JOIN users s ON s.id = a.sender_id" +
		" LEFT JOIN hosts h ON h.id = a.host_id " +
		where + " ORDER BY a.created_at DESC LIMIT ? OFFSET ?"

	var rows []announcementRow
	if err := DB.WithContext(ctx).Raw(listSQL, listArgs...).Scan(&rows).Error; err != nil {
		return AnnouncementPage[AnnouncementAdminItem]{}, err
	}

	items := make([]AnnouncementAdminItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, AnnouncementAdminItem{
			ID:             row.ID,
			Type:           models.AnnouncementType(row.Type),
			Title:          row.Title,
			Content:        row.Content,
			RecipientCount: row.RecipientCount,
			HostID:         row.HostID,
			TargetUserID:   row.TargetUserID,
			InstanceID:     row.InstanceID,
			CreatedAt:      row.CreatedAt,
			Sender:         senderRef(row),
			Host:           hostRef(row),
		})
	}

	return AnnouncementPage[AnnouncementAdminItem]{
		Items:      items,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages(total, pageSize),
	}, nil
}

// GetHostOwnerAnnouncementList mirrors getHostOwnerAnnouncementList().
func GetHostOwnerAnnouncementList(ctx context.Context, senderID int64, options AnnouncementListOptions) (AnnouncementPage[AnnouncementHostOwnerItem], error) {
	page := options.Page
	if page < 1 {
		page = 1
	}
	pageSize := options.PageSize
	if pageSize < 1 {
		pageSize = 20
	}

	where := "WHERE a.sender_id = ? AND a.type IN ('host_broadcast', 'host_message')"
	args := []any{senderID}
	if options.HostID != 0 {
		where += " AND a.host_id = ?"
		args = append(args, options.HostID)
	}

	var total int64
	countSQL := "SELECT COUNT(*) FROM announcements a " + where
	if err := DB.WithContext(ctx).Raw(countSQL, args...).Scan(&total).Error; err != nil {
		return AnnouncementPage[AnnouncementHostOwnerItem]{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	listSQL := "SELECT " + announcementSelectColumns +
		" FROM announcements a" +
		" LEFT JOIN hosts h ON h.id = a.host_id " +
		where + " ORDER BY a.created_at DESC LIMIT ? OFFSET ?"

	var rows []announcementRow
	if err := DB.WithContext(ctx).Raw(listSQL, listArgs...).Scan(&rows).Error; err != nil {
		return AnnouncementPage[AnnouncementHostOwnerItem]{}, err
	}

	items := make([]AnnouncementHostOwnerItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, AnnouncementHostOwnerItem{
			ID:             row.ID,
			Type:           models.AnnouncementType(row.Type),
			Title:          row.Title,
			Content:        row.Content,
			RecipientCount: row.RecipientCount,
			HostID:         row.HostID,
			TargetUserID:   row.TargetUserID,
			InstanceID:     row.InstanceID,
			CreatedAt:      row.CreatedAt,
			Host:           hostRef(row),
		})
	}

	return AnnouncementPage[AnnouncementHostOwnerItem]{
		Items:      items,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages(total, pageSize),
	}, nil
}

// GetAnnouncementByID mirrors getAnnouncementById(); returns nil when absent.
func GetAnnouncementByID(ctx context.Context, id int64) (*AnnouncementDetail, error) {
	sql := "SELECT " + announcementSelectColumns +
		" FROM announcements a" +
		" LEFT JOIN users s ON s.id = a.sender_id" +
		" LEFT JOIN hosts h ON h.id = a.host_id" +
		" WHERE a.id = ? LIMIT 1"

	var rows []announcementRow
	if err := DB.WithContext(ctx).Raw(sql, id).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	row := rows[0]
	detail := &AnnouncementDetail{
		ID:             row.ID,
		Type:           models.AnnouncementType(row.Type),
		SenderID:       derefInt32(row.SenderID),
		Title:          row.Title,
		Content:        row.Content,
		RecipientCount: row.RecipientCount,
		HostID:         row.HostID,
		TargetUserID:   row.TargetUserID,
		InstanceID:     row.InstanceID,
		CreatedAt:      row.CreatedAt,
		Sender:         senderRef(row),
		Host:           hostRef(row),
	}
	return detail, nil
}

func senderRef(row announcementRow) *AnnouncementSenderRef {
	if row.SenderID == nil {
		return nil
	}
	return &AnnouncementSenderRef{ID: *row.SenderID, Username: derefString(row.SenderUsername)}
}

func hostRef(row announcementRow) *AnnouncementHostRef {
	if row.HostRefID == nil {
		return nil
	}
	return &AnnouncementHostRef{ID: *row.HostRefID, Name: derefString(row.HostRefName)}
}

func derefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func totalPages(total int64, pageSize int) int64 {
	if pageSize <= 0 {
		return 0
	}
	return (total + int64(pageSize) - 1) / int64(pageSize)
}
