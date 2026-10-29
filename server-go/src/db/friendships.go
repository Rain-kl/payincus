// Port of server/src/db/friendships.ts.
//
// The friendship table is a single directed row per pair (@@unique([userId, friendId]))
// with a status state machine:
//
//	pending ──accept──▶ accepted ──remove──▶ removed
//	   │  ▲
//	   │  └── re-send (rejected → pending)
//	   └──reject──▶ rejected
//
// There is deliberately no friend-quota check in this module: the TypeScript
// friendships module and the /api/friends routes never read UserQuota.friendLimit
// (quota accounting lives in db/quota-operations.ts and is not wired into the
// friends flow). Porting a check here would invent behaviour, so the Go version
// keeps the same surface.
//
// Bidirectional uniqueness: every lookup scans both directions (user_id/friend_id),
// so a pair can never hold two rows. Every state transition is additionally
// guarded by a `WHERE ... AND status = '<expected>'` predicate, so a concurrent
// accept/reject/cancel that loses the race reports REQUEST_NOT_PENDING instead of
// silently overwriting the winner's state.
package db

import (
	"context"
	"errors"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== errors ====================

// Friendship state-machine errors mirror the Node `throw new Error('...')` codes;
// the route maps them onto APIError codes.
var (
	ErrFriendAlreadyFriends    = errors.New("ALREADY_FRIENDS")
	ErrFriendRequestPending    = errors.New("REQUEST_PENDING")
	ErrFriendRequestNotFound   = errors.New("REQUEST_NOT_FOUND")
	ErrFriendRequestNotPending = errors.New("REQUEST_NOT_PENDING")
	ErrFriendForbidden         = errors.New("FORBIDDEN")
	ErrFriendshipNotFound      = errors.New("FRIENDSHIP_NOT_FOUND")
)

// ==================== projections ====================

// FriendInfo mirrors the FriendInfo interface of getFriendList().
type FriendInfo struct {
	ID            int32                   `json:"id"`
	FriendshipID  int32                   `json:"friendshipId"`
	FriendID      int32                   `json:"friendId"`
	Username      string                  `json:"username"`
	Email         *string                 `json:"email"`
	AvatarStyle   string                  `json:"avatarStyle"`
	AvatarBadgeID *string                 `json:"avatarBadgeId"`
	Status        models.FriendshipStatus `json:"status"`
	CreatedAt     string                  `json:"createdAt"`
	AcceptedAt    *string                 `json:"acceptedAt"`
	InitiatedByMe bool                    `json:"initiatedByMe"`
	HostCount     int64                   `json:"hostCount"`
	InstanceCount int64                   `json:"instanceCount"`
}

// FriendRequest mirrors the FriendRequest interface of the pending/sent lists.
type FriendRequest struct {
	ID            int32   `json:"id"`
	UserID        int32   `json:"userId"`
	Username      string  `json:"username"`
	Email         *string `json:"email"`
	AvatarStyle   string  `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
	Remark        *string `json:"remark"`
	CreatedAt     string  `json:"createdAt"`
}

// FriendRequestHistory mirrors the FriendRequestHistory interface.
type FriendRequestHistory struct {
	ID            int32                   `json:"id"`
	UserID        int32                   `json:"userId"`
	Username      string                  `json:"username"`
	Email         *string                 `json:"email"`
	AvatarStyle   string                  `json:"avatarStyle"`
	AvatarBadgeID *string                 `json:"avatarBadgeId"`
	Remark        *string                 `json:"remark"`
	Status        models.FriendshipStatus `json:"status"`
	CreatedAt     string                  `json:"createdAt"`
	AcceptedAt    *string                 `json:"acceptedAt"`
	RejectedAt    *string                 `json:"rejectedAt"`
	InitiatedByMe bool                    `json:"initiatedByMe"`
}

// FriendSearchUser mirrors the searchUserByUsername() select projection.
type FriendSearchUser struct {
	ID            int32   `json:"id"`
	Username      string  `json:"username"`
	Email         *string `json:"email"`
	AvatarStyle   string  `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
}

// FriendshipUserRef is the nested user projection used by getFriendshipById().
type FriendshipUserRef struct {
	ID            int32   `json:"id"`
	Username      string  `json:"username"`
	Email         *string `json:"email"`
	AvatarStyle   string  `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
}

// FriendshipJSON is the friendship row shape returned by Prisma mutations
// (create/update/delete). It carries the scalar columns only, exactly like the
// Node `prisma.friendship.update(...)` result.
type FriendshipJSON struct {
	ID         int32                   `json:"id"`
	UserID     int32                   `json:"userId"`
	FriendID   int32                   `json:"friendId"`
	Status     models.FriendshipStatus `json:"status"`
	Remark     *string                 `json:"remark"`
	CreatedAt  string                  `json:"createdAt"`
	AcceptedAt *string                 `json:"acceptedAt"`
	RejectedAt *string                 `json:"rejectedAt"`
}

// FriendshipDetail mirrors getFriendshipById(): the row plus both user refs.
type FriendshipDetail struct {
	FriendshipJSON
	User   *FriendshipUserRef `json:"user"`
	Friend *FriendshipUserRef `json:"friend"`
}

// ==================== row + helpers ====================

type friendshipRow struct {
	ID         int32      `gorm:"column:id"`
	UserID     int32      `gorm:"column:user_id"`
	FriendID   int32      `gorm:"column:friend_id"`
	Status     string     `gorm:"column:status"`
	Remark     *string    `gorm:"column:remark"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	AcceptedAt *time.Time `gorm:"column:accepted_at"`
	RejectedAt *time.Time `gorm:"column:rejected_at"`
}

// friendshipJoinedRow adds the two user projections for the joined selects. The
// friendship columns are spelled out (rather than embedding friendshipRow) so the
// GORM scan target is unambiguous.
type friendshipJoinedRow struct {
	ID         int32      `gorm:"column:id"`
	UserID     int32      `gorm:"column:user_id"`
	FriendID   int32      `gorm:"column:friend_id"`
	Status     string     `gorm:"column:status"`
	Remark     *string    `gorm:"column:remark"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	AcceptedAt *time.Time `gorm:"column:accepted_at"`
	RejectedAt *time.Time `gorm:"column:rejected_at"`

	UserRefID           int32   `gorm:"column:user_ref_id"`
	UserUsername        string  `gorm:"column:user_username"`
	UserEmail           *string `gorm:"column:user_email"`
	UserAvatarStyle     string  `gorm:"column:user_avatar_style"`
	UserAvatarBadgeID   *string `gorm:"column:user_avatar_badge_id"`
	FriendRefID         int32   `gorm:"column:friend_ref_id"`
	FriendUsername      string  `gorm:"column:friend_username"`
	FriendEmail         *string `gorm:"column:friend_email"`
	FriendAvatarStyle   string  `gorm:"column:friend_avatar_style"`
	FriendAvatarBadgeID *string `gorm:"column:friend_avatar_badge_id"`
}

func (row friendshipJoinedRow) friendshipRow() friendshipRow {
	return friendshipRow{
		ID:         row.ID,
		UserID:     row.UserID,
		FriendID:   row.FriendID,
		Status:     row.Status,
		Remark:     row.Remark,
		CreatedAt:  row.CreatedAt,
		AcceptedAt: row.AcceptedAt,
		RejectedAt: row.RejectedAt,
	}
}

// friendshipColumns is the bare column list (no joins).
const friendshipColumns = `
	id, user_id, friend_id, status::text AS status, remark,
	created_at, accepted_at, rejected_at`

const friendshipJoinedColumns = `
	f.id, f.user_id, f.friend_id, f.status::text AS status, f.remark,
	f.created_at, f.accepted_at, f.rejected_at,
	u.id AS user_ref_id, u.username AS user_username, u.email AS user_email,
	u.avatar_style AS user_avatar_style, u.avatar_badge_id AS user_avatar_badge_id,
	fr.id AS friend_ref_id, fr.username AS friend_username, fr.email AS friend_email,
	fr.avatar_style AS friend_avatar_style, fr.avatar_badge_id AS friend_avatar_badge_id`

const friendshipJoinedFrom = ` FROM friendships f
	JOIN users u ON u.id = f.user_id
	JOIN users fr ON fr.id = f.friend_id `

// friendTimestamp renders a Date the way JSON.stringify does (UTC, ms, Z).
func friendTimestamp(value time.Time) string { return isoTimestamp(value) }

func friendTimestampPtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := isoTimestamp(*value)
	return &formatted
}

func friendshipJSONOf(row friendshipRow) FriendshipJSON {
	return FriendshipJSON{
		ID:         row.ID,
		UserID:     row.UserID,
		FriendID:   row.FriendID,
		Status:     models.FriendshipStatus(row.Status),
		Remark:     row.Remark,
		CreatedAt:  friendTimestamp(row.CreatedAt),
		AcceptedAt: friendTimestampPtr(row.AcceptedAt),
		RejectedAt: friendTimestampPtr(row.RejectedAt),
	}
}

func friendshipDetailOf(row friendshipJoinedRow) FriendshipDetail {
	return FriendshipDetail{
		FriendshipJSON: friendshipJSONOf(row.friendshipRow()),
		User: &FriendshipUserRef{
			ID:            row.UserRefID,
			Username:      row.UserUsername,
			Email:         row.UserEmail,
			AvatarStyle:   row.UserAvatarStyle,
			AvatarBadgeID: row.UserAvatarBadgeID,
		},
		Friend: &FriendshipUserRef{
			ID:            row.FriendRefID,
			Username:      row.FriendUsername,
			Email:         row.FriendEmail,
			AvatarStyle:   row.FriendAvatarStyle,
			AvatarBadgeID: row.FriendAvatarBadgeID,
		},
	}
}

func queryFriendshipRows(ctx context.Context, sql string, args ...any) ([]friendshipRow, error) {
	var rows []friendshipRow
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func queryFriendshipJoinedRows(ctx context.Context, sql string, args ...any) ([]friendshipJoinedRow, error) {
	var rows []friendshipJoinedRow
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// getFriendshipRowById reads one bare row (nil when absent).
func getFriendshipRowById(ctx context.Context, id int32) (*friendshipRow, error) {
	rows, err := queryFriendshipRows(ctx,
		"SELECT "+friendshipColumns+" FROM friendships WHERE id = ? LIMIT 1", id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ==================== state machine ====================

type friendRequestAction int

const (
	friendActionCreate friendRequestAction = iota
	friendActionAutoAccept
	friendActionResend
	friendActionAlreadyFriends
	friendActionPending
)

// planFriendRequest mirrors the branch ladder at the top of sendFriendRequest().
// It is a pure decision so the state machine can be unit-tested without a DB.
func planFriendRequest(existing *friendshipRow, userID, friendID int32) (friendRequestAction, int32) {
	if existing == nil {
		return friendActionCreate, 0
	}
	switch existing.Status {
	case string(models.FriendshipStatusAccepted):
		return friendActionAlreadyFriends, existing.ID
	case string(models.FriendshipStatusPending):
		// 对方已经向我发送了请求：直接接受
		if existing.UserID == friendID && existing.FriendID == userID {
			return friendActionAutoAccept, existing.ID
		}
		return friendActionPending, existing.ID
	case string(models.FriendshipStatusRejected):
		// 之前被拒绝过，允许重新发送
		return friendActionResend, existing.ID
	default:
		// removed: the TypeScript falls through to create().
		return friendActionCreate, existing.ID
	}
}

// ==================== request operations ====================

// SendFriendRequest mirrors sendFriendRequest(). remark nil means "no remark"
// (the TypeScript `remark || null`).
func SendFriendRequest(ctx context.Context, userID, friendID int32, remark *string) (FriendshipJSON, error) {
	rows, err := queryFriendshipRows(ctx,
		"SELECT "+friendshipColumns+" FROM friendships"+
			" WHERE (user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)"+
			" ORDER BY id ASC LIMIT 1",
		userID, friendID, friendID, userID)
	if err != nil {
		return FriendshipJSON{}, err
	}

	var existing *friendshipRow
	if len(rows) > 0 {
		existing = &rows[0]
	}

	action, existingID := planFriendRequest(existing, userID, friendID)
	switch action {
	case friendActionAlreadyFriends:
		return FriendshipJSON{}, ErrFriendAlreadyFriends
	case friendActionPending:
		return FriendshipJSON{}, ErrFriendRequestPending
	case friendActionAutoAccept:
		// 对方已向我发过请求：直接接受（复用 acceptFriendRequest 的状态校验）
		return AcceptFriendRequest(ctx, existingID, userID)
	case friendActionResend:
		// 被拒绝过：重置为 pending 并清空 rejectedAt。状态谓词防止与并发的
		// accept/reject 交叉覆盖。
		updated, err := queryFriendshipRows(ctx,
			"UPDATE friendships SET status = 'pending', remark = ?, rejected_at = NULL"+
				" WHERE id = ? AND status = 'rejected'"+
				" RETURNING "+friendshipColumns, remark, existingID)
		if err != nil {
			return FriendshipJSON{}, err
		}
		if len(updated) == 0 {
			return FriendshipJSON{}, ErrFriendRequestNotPending
		}
		return friendshipJSONOf(updated[0]), nil
	}

	// create
	created, err := queryFriendshipRows(ctx,
		"INSERT INTO friendships (user_id, friend_id, status, remark, created_at)"+
			" VALUES (?, ?, 'pending', ?, NOW())"+
			" RETURNING "+friendshipColumns, userID, friendID, remark)
	if err != nil {
		return FriendshipJSON{}, err
	}
	if len(created) == 0 {
		return FriendshipJSON{}, errors.New("friendship insert returned no row")
	}
	return friendshipJSONOf(created[0]), nil
}

// AcceptFriendRequest mirrors acceptFriendRequest(): only the *receiver* may
// accept, and only while the row is still pending.
func AcceptFriendRequest(ctx context.Context, requestID, userID int32) (FriendshipJSON, error) {
	request, err := getFriendshipRowById(ctx, requestID)
	if err != nil {
		return FriendshipJSON{}, err
	}
	if request == nil {
		return FriendshipJSON{}, ErrFriendRequestNotFound
	}
	// 只有接收者才能接受
	if request.FriendID != userID {
		return FriendshipJSON{}, ErrFriendForbidden
	}
	if request.Status != string(models.FriendshipStatusPending) {
		return FriendshipJSON{}, ErrFriendRequestNotPending
	}

	updated, err := queryFriendshipRows(ctx,
		"UPDATE friendships SET status = 'accepted', accepted_at = NOW()"+
			" WHERE id = ? AND status = 'pending'"+
			" RETURNING "+friendshipColumns, requestID)
	if err != nil {
		return FriendshipJSON{}, err
	}
	if len(updated) == 0 {
		// 并发下另一请求已经改过状态
		return FriendshipJSON{}, ErrFriendRequestNotPending
	}
	return friendshipJSONOf(updated[0]), nil
}

// RejectFriendRequest mirrors rejectFriendRequest().
func RejectFriendRequest(ctx context.Context, requestID, userID int32) (FriendshipJSON, error) {
	request, err := getFriendshipRowById(ctx, requestID)
	if err != nil {
		return FriendshipJSON{}, err
	}
	if request == nil {
		return FriendshipJSON{}, ErrFriendRequestNotFound
	}
	// 只有接收者才能拒绝
	if request.FriendID != userID {
		return FriendshipJSON{}, ErrFriendForbidden
	}
	if request.Status != string(models.FriendshipStatusPending) {
		return FriendshipJSON{}, ErrFriendRequestNotPending
	}

	updated, err := queryFriendshipRows(ctx,
		"UPDATE friendships SET status = 'rejected', rejected_at = NOW()"+
			" WHERE id = ? AND status = 'pending'"+
			" RETURNING "+friendshipColumns, requestID)
	if err != nil {
		return FriendshipJSON{}, err
	}
	if len(updated) == 0 {
		return FriendshipJSON{}, ErrFriendRequestNotPending
	}
	return friendshipJSONOf(updated[0]), nil
}

// CancelFriendRequest mirrors cancelFriendRequest(): only the initiator may
// cancel, and the row is deleted (not soft-deleted).
func CancelFriendRequest(ctx context.Context, requestID, userID int32) (FriendshipJSON, error) {
	request, err := getFriendshipRowById(ctx, requestID)
	if err != nil {
		return FriendshipJSON{}, err
	}
	if request == nil {
		return FriendshipJSON{}, ErrFriendRequestNotFound
	}
	// 只有发起者才能取消
	if request.UserID != userID {
		return FriendshipJSON{}, ErrFriendForbidden
	}
	if request.Status != string(models.FriendshipStatusPending) {
		return FriendshipJSON{}, ErrFriendRequestNotPending
	}

	deleted, err := queryFriendshipRows(ctx,
		"DELETE FROM friendships WHERE id = ? AND status = 'pending'"+
			" RETURNING "+friendshipColumns, requestID)
	if err != nil {
		return FriendshipJSON{}, err
	}
	if len(deleted) == 0 {
		return FriendshipJSON{}, ErrFriendRequestNotPending
	}
	return friendshipJSONOf(deleted[0]), nil
}

// RemoveFriend mirrors removeFriend(): a soft delete (status = removed) so both
// sides keep their history.
func RemoveFriend(ctx context.Context, friendshipID, userID int32) (FriendshipJSON, error) {
	friendship, err := getFriendshipRowById(ctx, friendshipID)
	if err != nil {
		return FriendshipJSON{}, err
	}
	if friendship == nil {
		return FriendshipJSON{}, ErrFriendshipNotFound
	}
	// 任一方都可以删除好友
	if friendship.UserID != userID && friendship.FriendID != userID {
		return FriendshipJSON{}, ErrFriendForbidden
	}

	updated, err := queryFriendshipRows(ctx,
		"UPDATE friendships SET status = 'removed' WHERE id = ?"+
			" RETURNING "+friendshipColumns, friendshipID)
	if err != nil {
		return FriendshipJSON{}, err
	}
	if len(updated) == 0 {
		return FriendshipJSON{}, ErrFriendshipNotFound
	}
	return friendshipJSONOf(updated[0]), nil
}

// ==================== queries ====================

// GetFriendList mirrors getFriendList(): accepted friendships in either
// direction, ordered by acceptedAt DESC, with host/instance counts.
func GetFriendList(ctx context.Context, userID int32) ([]FriendInfo, error) {
	rows, err := queryFriendshipJoinedRows(ctx,
		"SELECT "+friendshipJoinedColumns+friendshipJoinedFrom+
			"WHERE f.status = 'accepted' AND (f.user_id = ? OR f.friend_id = ?)"+
			" ORDER BY f.accepted_at DESC", userID, userID)
	if err != nil {
		return nil, err
	}

	friendIDs := make([]int32, 0, len(rows))
	for _, row := range rows {
		if row.UserID == userID {
			friendIDs = append(friendIDs, row.FriendRefID)
		} else {
			friendIDs = append(friendIDs, row.UserRefID)
		}
	}

	hostCounts, err := countUserHosts(ctx, friendIDs)
	if err != nil {
		return nil, err
	}
	instanceCounts, err := countUserInstances(ctx, friendIDs)
	if err != nil {
		return nil, err
	}

	friends := make([]FriendInfo, 0, len(rows))
	for _, row := range rows {
		iAmInitiator := row.UserID == userID
		other := FriendshipUserRef{
			ID:            row.UserRefID,
			Username:      row.UserUsername,
			Email:         row.UserEmail,
			AvatarStyle:   row.UserAvatarStyle,
			AvatarBadgeID: row.UserAvatarBadgeID,
		}
		if iAmInitiator {
			other = FriendshipUserRef{
				ID:            row.FriendRefID,
				Username:      row.FriendUsername,
				Email:         row.FriendEmail,
				AvatarStyle:   row.FriendAvatarStyle,
				AvatarBadgeID: row.FriendAvatarBadgeID,
			}
		}
		friends = append(friends, FriendInfo{
			ID:            other.ID,
			FriendshipID:  row.ID,
			FriendID:      other.ID,
			Username:      other.Username,
			Email:         other.Email,
			AvatarStyle:   other.AvatarStyle,
			AvatarBadgeID: other.AvatarBadgeID,
			Status:        models.FriendshipStatus(row.Status),
			CreatedAt:     friendTimestamp(row.CreatedAt),
			AcceptedAt:    friendTimestampPtr(row.AcceptedAt),
			InitiatedByMe: iAmInitiator,
			HostCount:     hostCounts[other.ID],
			InstanceCount: instanceCounts[other.ID],
		})
	}
	return friends, nil
}

// GetPendingFriendRequests mirrors getPendingFriendRequests().
func GetPendingFriendRequests(ctx context.Context, userID int32) ([]FriendRequest, error) {
	rows, err := queryFriendshipJoinedRows(ctx,
		"SELECT "+friendshipJoinedColumns+friendshipJoinedFrom+
			"WHERE f.friend_id = ? AND f.status = 'pending'"+
			" ORDER BY f.created_at DESC", userID)
	if err != nil {
		return nil, err
	}

	requests := make([]FriendRequest, 0, len(rows))
	for _, row := range rows {
		requests = append(requests, FriendRequest{
			ID:            row.ID,
			UserID:        row.UserRefID,
			Username:      row.UserUsername,
			Email:         row.UserEmail,
			AvatarStyle:   row.UserAvatarStyle,
			AvatarBadgeID: row.UserAvatarBadgeID,
			Remark:        row.Remark,
			CreatedAt:     friendTimestamp(row.CreatedAt),
		})
	}
	return requests, nil
}

// GetSentFriendRequests mirrors getSentFriendRequests().
func GetSentFriendRequests(ctx context.Context, userID int32) ([]FriendRequest, error) {
	rows, err := queryFriendshipJoinedRows(ctx,
		"SELECT "+friendshipJoinedColumns+friendshipJoinedFrom+
			"WHERE f.user_id = ? AND f.status = 'pending'"+
			" ORDER BY f.created_at DESC", userID)
	if err != nil {
		return nil, err
	}

	requests := make([]FriendRequest, 0, len(rows))
	for _, row := range rows {
		requests = append(requests, FriendRequest{
			ID:            row.ID,
			UserID:        row.FriendRefID,
			Username:      row.FriendUsername,
			Email:         row.FriendEmail,
			AvatarStyle:   row.FriendAvatarStyle,
			AvatarBadgeID: row.FriendAvatarBadgeID,
			Remark:        row.Remark,
			CreatedAt:     friendTimestamp(row.CreatedAt),
		})
	}
	return requests, nil
}

// AreFriends mirrors areFriends().
func AreFriends(ctx context.Context, userID1, userID2 int32) (bool, error) {
	var exists bool
	err := DB.WithContext(ctx).Raw(
		"SELECT EXISTS(SELECT 1 FROM friendships"+
			" WHERE status = 'accepted'"+
			" AND ((user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)))",
		userID1, userID2, userID2, userID1).Scan(&exists).Error
	if err != nil {
		return false, err
	}
	return exists, nil
}

// GetFriendIds mirrors getFriendIds(): accepted friends in either direction,
// de-duplicated while preserving first-seen order (Set insertion order).
func GetFriendIds(ctx context.Context, userID int32) ([]int32, error) {
	var rows []struct {
		UserID   int32 `gorm:"column:user_id"`
		FriendID int32 `gorm:"column:friend_id"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT user_id, friend_id FROM friendships"+
			" WHERE status = 'accepted' AND (user_id = ? OR friend_id = ?)", userID, userID).
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	seen := map[int32]bool{}
	ids := make([]int32, 0, len(rows))
	for _, row := range rows {
		friendID := row.UserID
		if row.UserID == userID {
			friendID = row.FriendID
		}
		if !seen[friendID] {
			seen[friendID] = true
			ids = append(ids, friendID)
		}
	}
	return ids, nil
}

// SearchUserByUsername mirrors searchUserByUsername(): case-insensitive exact
// match (Prisma `equals` + `mode: 'insensitive'` compiles to ILIKE), excluding the
// caller and inactive accounts.
func SearchUserByUsername(ctx context.Context, username string, excludeUserID int32) (*FriendSearchUser, error) {
	var rows []struct {
		ID            int32   `gorm:"column:id"`
		Username      string  `gorm:"column:username"`
		Email         *string `gorm:"column:email"`
		AvatarStyle   string  `gorm:"column:avatar_style"`
		AvatarBadgeID *string `gorm:"column:avatar_badge_id"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT id, username, email, avatar_style, avatar_badge_id FROM users"+
			" WHERE username ILIKE ? AND id <> ? AND status = 'active'"+
			" ORDER BY id ASC LIMIT 1", username, excludeUserID).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	return &FriendSearchUser{
		ID:            row.ID,
		Username:      row.Username,
		Email:         row.Email,
		AvatarStyle:   row.AvatarStyle,
		AvatarBadgeID: row.AvatarBadgeID,
	}, nil
}

// GetFriendshipById mirrors getFriendshipById(); nil when the row is absent.
func GetFriendshipById(ctx context.Context, friendshipID int32) (*FriendshipDetail, error) {
	rows, err := queryFriendshipJoinedRows(ctx,
		"SELECT "+friendshipJoinedColumns+friendshipJoinedFrom+
			"WHERE f.id = ? LIMIT 1", friendshipID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	detail := friendshipDetailOf(rows[0])
	return &detail, nil
}

// friendshipHistoryStatuses are the three terminal statuses a history query may
// select. `filter` additionally accepts "" and "all" (both mean "all three").
var friendshipHistoryStatuses = []string{"accepted", "rejected", "removed"}

// IsFriendHistoryFilter reports whether the query value is a supported filter.
// It mirrors parseFriendHistoryFilter's accept set, which includes "all".
func IsFriendHistoryFilter(value string) bool {
	if value == "" || value == "all" {
		return true
	}
	for _, candidate := range friendshipHistoryStatuses {
		if candidate == value {
			return true
		}
	}
	return false
}

// GetRequestHistory mirrors getRequestHistory(). filter "" or "all" means
// accepted+rejected+removed.
func GetRequestHistory(ctx context.Context, userID int32, filter string) ([]FriendRequestHistory, error) {
	if !IsFriendHistoryFilter(filter) {
		return nil, errors.New("INVALID_HISTORY_FILTER")
	}

	sql := "SELECT " + friendshipJoinedColumns + friendshipJoinedFrom +
		"WHERE f.status IN ('accepted', 'rejected', 'removed')" +
		" AND (f.friend_id = ? OR f.user_id = ?)"
	args := []any{userID, userID}
	if filter != "" && filter != "all" {
		sql += " AND f.status = ?"
		args = append(args, filter)
	}
	sql += " ORDER BY f.created_at DESC"

	rows, err := queryFriendshipJoinedRows(ctx, sql, args...)
	if err != nil {
		return nil, err
	}

	history := make([]FriendRequestHistory, 0, len(rows))
	for _, row := range rows {
		iAmInitiator := row.UserID == userID
		other := FriendshipUserRef{
			ID:            row.UserRefID,
			Username:      row.UserUsername,
			Email:         row.UserEmail,
			AvatarStyle:   row.UserAvatarStyle,
			AvatarBadgeID: row.UserAvatarBadgeID,
		}
		if iAmInitiator {
			other = FriendshipUserRef{
				ID:            row.FriendRefID,
				Username:      row.FriendUsername,
				Email:         row.FriendEmail,
				AvatarStyle:   row.FriendAvatarStyle,
				AvatarBadgeID: row.FriendAvatarBadgeID,
			}
		}
		history = append(history, FriendRequestHistory{
			ID:            row.ID,
			UserID:        other.ID,
			Username:      other.Username,
			Email:         other.Email,
			AvatarStyle:   other.AvatarStyle,
			AvatarBadgeID: other.AvatarBadgeID,
			Remark:        row.Remark,
			Status:        models.FriendshipStatus(row.Status),
			CreatedAt:     friendTimestamp(row.CreatedAt),
			AcceptedAt:    friendTimestampPtr(row.AcceptedAt),
			RejectedAt:    friendTimestampPtr(row.RejectedAt),
			InitiatedByMe: iAmInitiator,
		})
	}
	return history, nil
}

// CountUserHosts / CountUserInstances mirror the two per-user counts the friends
// list and the resources endpoint need.
func CountUserHosts(ctx context.Context, userID int32) (int64, error) {
	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM hosts WHERE user_id = ?", userID).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// CountUserPackages mirrors `prisma.package.count({ where: { userId } })`.
func CountUserPackages(ctx context.Context, userID int32) (int64, error) {
	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM packages WHERE user_id = ?", userID).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// ==================== internal counts ====================

func countUserHosts(ctx context.Context, userIDs []int32) (map[int32]int64, error) {
	counts := map[int32]int64{}
	if len(userIDs) == 0 {
		return counts, nil
	}
	var rows []struct {
		UserID int32 `gorm:"column:user_id"`
		Total  int64 `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT user_id, COUNT(*) AS total FROM hosts WHERE user_id IN (?) GROUP BY user_id",
		userIDs).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		counts[row.UserID] = row.Total
	}
	return counts, nil
}

// countUserInstances mirrors `prisma.instance.count({ where: { userId, status: {
// not: 'deleted' } } })` for every friend at once. The TypeScript issues one query
// per friend; the resulting counts are identical.
func countUserInstances(ctx context.Context, userIDs []int32) (map[int32]int64, error) {
	counts := map[int32]int64{}
	if len(userIDs) == 0 {
		return counts, nil
	}
	var rows []struct {
		UserID int32 `gorm:"column:user_id"`
		Total  int64 `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT user_id, COUNT(*) AS total FROM instances"+
			" WHERE user_id IN (?) AND status <> 'deleted' GROUP BY user_id",
		userIDs).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		counts[row.UserID] = row.Total
	}
	return counts, nil
}
