// Port of server/src/db/tickets.ts.
//
// The TypeScript module returns hand-built objects with camelCase keys (it is not
// just a Prisma passthrough), so every response view below spells its json tags
// explicitly. Two projection families exist and they must not be mixed:
//
//   - list rows (getUserTickets / getHostTickets / getOwnerAllTickets) select only
//     `instance: {id, name, status, iconBadgeId}` — the wire object has exactly
//     those four keys, never null-padded extras;
//   - the detail row (getTicketById) additionally selects incusId/ipv4/ipv6/cpu/
//     memory/disk/image plus imageName (looked up in system_images by remoteAlias)
//     and packageName.
//
// Ordering notes that are easy to get wrong:
//
//   - getUserTickets paginates in SQL (ORDER BY updated_at DESC + skip/take) and
//     returns the SQL order untouched;
//   - getHostTickets / getOwnerAllTickets fetch the WHOLE candidate set, apply the
//     queue filter, sort the complete queue with the Node comparator (stable, with
//     `timeOrder || a.id - b.id` as the final tie-break), and only then slice —
//     `total` is the size of the filtered queue, not the table count. The guard
//     test test-ticket-query-guards.ts pins exactly that ordering.
package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	ticketsSearchMaxLength = 128
	ticketsMaxPageSize     = 100
	ticketsMaxSafeInteger  = 1<<53 - 1
)

// ticketsStatuses mirrors TICKET_STATUSES.
var ticketsStatuses = map[string]bool{
	string(models.TicketStatusOpen):       true,
	string(models.TicketStatusInProgress): true,
	string(models.TicketStatusResolved):   true,
	string(models.TicketStatusClosed):     true,
}

// ticketsLinkTypes mirrors TICKET_LINK_TYPES.
var ticketsLinkTypes = map[string]bool{
	string(models.TicketObjectLinkTypeRechargeRecord):     true,
	string(models.TicketObjectLinkTypeOrderOperationCase): true,
	string(models.TicketObjectLinkTypeInstance):           true,
	string(models.TicketObjectLinkTypeHost):               true,
}

// ==================== wire types ====================

// TicketsTicketUserRef mirrors the `user: { select: {id, username, avatarStyle,
// avatarBadgeId} }` projection (used for ticket.user and message.sender).
type TicketsTicketUserRef struct {
	ID            int32   `json:"id"`
	Username      string  `json:"username"`
	AvatarStyle   string  `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
}

// TicketsTicketHostRef mirrors the host projection.
type TicketsTicketHostRef struct {
	ID          int32  `json:"id"`
	Name        string `json:"name"`
	UserID      int32  `json:"userId"`
	CountryCode string `json:"countryCode"`
}

// TicketsTicketInstanceRef is the *list* instance projection: four keys only.
type TicketsTicketInstanceRef struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	IconBadgeID *string `json:"iconBadgeId"`
}

// TicketsTicketInstanceDetail is the *detail* instance projection.
type TicketsTicketInstanceDetail struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	IconBadgeID *string `json:"iconBadgeId"`
	IncusID     *string `json:"incusId"`
	IPv4        *string `json:"ipv4"`
	IPv6        *string `json:"ipv6"`
	CPU         int32   `json:"cpu"`
	Memory      int32   `json:"memory"`
	Disk        int32   `json:"disk"`
	Image       string  `json:"image"`
	ImageName   *string `json:"imageName"`
	PackageName *string `json:"packageName"`
}

// TicketsLastMessage mirrors `messages[0]` reduced by the select.
type TicketsLastMessage struct {
	Content     string `json:"content"`
	IsFromOwner bool   `json:"isFromOwner"`
	CreatedAt   string `json:"createdAt"`
}

// TicketsTicketWithDetails mirrors TicketWithDetails (list projection).
type TicketsTicketWithDetails struct {
	ID                 int32                     `json:"id"`
	UserID             int32                     `json:"userId"`
	HostID             *int32                    `json:"hostId"`
	InstanceID         *int32                    `json:"instanceId"`
	Subject            string                    `json:"subject"`
	Category           string                    `json:"category"`
	Priority           string                    `json:"priority"`
	Status             string                    `json:"status"`
	CreatedAt          string                    `json:"createdAt"`
	UpdatedAt          string                    `json:"updatedAt"`
	ResolvedAt         *string                   `json:"resolvedAt"`
	ClosedAt           *string                   `json:"closedAt"`
	FirstResponseDueAt *string                   `json:"firstResponseDueAt"`
	ResolutionDueAt    *string                   `json:"resolutionDueAt"`
	FirstRespondedAt   *string                   `json:"firstRespondedAt"`
	SlaBreachedAt      *string                   `json:"slaBreachedAt"`
	SLAStatus          string                    `json:"slaStatus"`
	User               TicketsTicketUserRef      `json:"user"`
	Host               *TicketsTicketHostRef     `json:"host"`
	Instance           *TicketsTicketInstanceRef `json:"instance"`
	MessageCount       int64                     `json:"messageCount"`
	LastMessage        *TicketsLastMessage       `json:"lastMessage"`
}

// TicketsTicketWithDetailsExtended adds the needsReply flag (getUserTickets,
// getHostTickets, getOwnerAllTickets).
type TicketsTicketWithDetailsExtended struct {
	TicketsTicketWithDetails
	NeedsReply bool `json:"needsReply"`
}

// TicketsTicketDetail mirrors getTicketById()'s full response.
type TicketsTicketDetail struct {
	ID                 int32                        `json:"id"`
	UserID             int32                        `json:"userId"`
	HostID             *int32                       `json:"hostId"`
	InstanceID         *int32                       `json:"instanceId"`
	Subject            string                       `json:"subject"`
	Category           string                       `json:"category"`
	Priority           string                       `json:"priority"`
	Status             string                       `json:"status"`
	CreatedAt          string                       `json:"createdAt"`
	UpdatedAt          string                       `json:"updatedAt"`
	ResolvedAt         *string                      `json:"resolvedAt"`
	ClosedAt           *string                      `json:"closedAt"`
	FirstResponseDueAt *string                      `json:"firstResponseDueAt"`
	ResolutionDueAt    *string                      `json:"resolutionDueAt"`
	FirstRespondedAt   *string                      `json:"firstRespondedAt"`
	SlaBreachedAt      *string                      `json:"slaBreachedAt"`
	SLAStatus          string                       `json:"slaStatus"`
	User               TicketsTicketUserRef         `json:"user"`
	Host               *TicketsTicketHostRef        `json:"host"`
	Instance           *TicketsTicketInstanceDetail `json:"instance"`
	MessageCount       int64                        `json:"messageCount"`
	LastMessage        *TicketsLastMessage          `json:"lastMessage"`
}

// TicketsTicketMessageAttachment mirrors mapTicketMessageAttachment(). Note the
// absence of url/thumbnailUrl: the TypeScript mapper drops them on purpose.
type TicketsTicketMessageAttachment struct {
	ID              int32   `json:"id"`
	TicketID        int32   `json:"ticketId"`
	MessageID       int32   `json:"messageId"`
	UploaderID      int32   `json:"uploaderId"`
	Provider        string  `json:"provider"`
	ProviderVersion string  `json:"providerVersion"`
	ProviderFileID  *string `json:"providerFileId"`
	Filename        string  `json:"filename"`
	OriginalName    string  `json:"originalName"`
	MimeType        string  `json:"mimeType"`
	SizeBytes       int32   `json:"sizeBytes"`
	Width           *int32  `json:"width"`
	Height          *int32  `json:"height"`
	CreatedAt       string  `json:"createdAt"`
}

// TicketsTicketMessage mirrors TicketMessage.
type TicketsTicketMessage struct {
	ID          int32                            `json:"id"`
	TicketID    int32                            `json:"ticketId"`
	SenderID    int32                            `json:"senderId"`
	Content     string                           `json:"content"`
	IsFromOwner bool                             `json:"isFromOwner"`
	CreatedAt   string                           `json:"createdAt"`
	Attachments []TicketsTicketMessageAttachment `json:"attachments"`
	Sender      TicketsTicketUserRef             `json:"sender"`
}

// TicketsTicketInternalNote mirrors TicketInternalNote.
type TicketsTicketInternalNote struct {
	ID            int32  `json:"id"`
	TicketID      int32  `json:"ticketId"`
	ActorUserID   int32  `json:"actorUserId"`
	ActorUsername string `json:"actorUsername"`
	Content       string `json:"content"`
	CreatedAt     string `json:"createdAt"`
}

// TicketsTicketObjectLink mirrors TicketObjectLink.
type TicketsTicketObjectLink struct {
	ID                int32   `json:"id"`
	TicketID          int32   `json:"ticketId"`
	ObjectType        string  `json:"objectType"`
	ObjectID          int32   `json:"objectId"`
	ObjectLabel       *string `json:"objectLabel"`
	CreatedByUserID   int32   `json:"createdByUserId"`
	CreatedByUsername string  `json:"createdByUsername"`
	CreatedAt         string  `json:"createdAt"`
}

// TicketsTicketPage is the list envelope.
type TicketsTicketPage struct {
	Tickets    []TicketsTicketWithDetailsExtended `json:"tickets"`
	Total      int64                              `json:"total"`
	Page       int                                `json:"page"`
	PageSize   int                                `json:"pageSize"`
	TotalPages int64                              `json:"totalPages"`
}

// TicketsMessagePage is the message list envelope.
type TicketsMessagePage struct {
	Messages   []TicketsTicketMessage `json:"messages"`
	Total      int64                  `json:"total"`
	Page       int                    `json:"page"`
	PageSize   int                    `json:"pageSize"`
	TotalPages int64                  `json:"totalPages"`
}

// ==================== option / input types ====================

// TicketsAttachmentData mirrors CreateTicketMessageAttachmentData.
type TicketsAttachmentData struct {
	Provider        string
	ProviderVersion string
	ProviderFileID  *string
	Filename        string
	OriginalName    string
	MimeType        string
	SizeBytes       int32
	Width           *int32
	Height          *int32
	URL             string
	ThumbnailURL    *string
}

// TicketsCreateTicketData mirrors CreateTicketData.
type TicketsCreateTicketData struct {
	UserID      int32
	HostID      *int32
	InstanceID  *int32
	Subject     string
	Category    *string
	Priority    *models.TicketPriority
	Content     string
	Attachments []TicketsAttachmentData
}

// TicketsListOptions mirrors the user/host list options. Page/PageSize are
// pointers so "absent" stays distinguishable (clampPagination checks undefined).
type TicketsListOptions struct {
	Status   string
	Search   *string
	Page     *int
	PageSize *int
}

// TicketsOwnerListOptions mirrors getOwnerAllTickets' options object.
type TicketsOwnerListOptions struct {
	Status     string
	HostID     *int32
	SourceType string
	Queue      string
	Search     *string
	Page       *int
	PageSize   *int
}

// TicketsTicketAccess mirrors canUserAccessTicket's return value.
type TicketsTicketAccess struct {
	CanAccess bool
	IsOwner   bool
	IsCreator bool
	IsAdmin   bool
}

// TicketsAutoCloseCandidate mirrors getTicketsForAutoClose' row shape.
type TicketsAutoCloseCandidate struct {
	ID                  int32
	Subject             string
	UserID              int32
	HostID              *int32
	LastPublicMessageAt time.Time
}

// TicketsAutoClosedTicket mirrors AutoClosedTicket.
type TicketsAutoClosedTicket struct {
	ID      int32
	Subject string
	UserID  int32
	HostID  *int32
}

// TicketsMessageAttachmentForCleanup mirrors getTicketMessageAttachments' row.
type TicketsMessageAttachmentForCleanup struct {
	ID              int32   `gorm:"column:id"`
	TicketID        int32   `gorm:"column:ticket_id"`
	MessageID       int32   `gorm:"column:message_id"`
	ProviderVersion string  `gorm:"column:provider_version"`
	ProviderFileID  *string `gorm:"column:provider_file_id"`
	URL             string  `gorm:"column:url"`
}

// TicketsMessageRef mirrors getTicketMessageById's row.
type TicketsMessageRef struct {
	ID       int32 `json:"id"`
	TicketID int32 `json:"ticketId"`
	SenderID int32 `json:"senderId"`
}

// TicketsAttachmentRef mirrors getTicketMessageAttachmentById's row.
type TicketsAttachmentRef struct {
	ID        int32  `json:"id"`
	TicketID  int32  `json:"ticketId"`
	MessageID int32  `json:"messageId"`
	MimeType  string `json:"mimeType"`
	Filename  string `json:"filename"`
	URL       string `json:"url"`
}

// ==================== pure helpers ====================

// ticketsClampPagination mirrors clampPagination().
func ticketsClampPagination(page, pageSize *int, fallbackPageSize int, maxPageSize int) (int, int) {
	safePage := 1
	if page != nil && *page > 0 {
		safePage = *page
	}
	safePageSize := fallbackPageSize
	if pageSize != nil {
		safePageSize = *pageSize
		if safePageSize < 1 {
			safePageSize = 1
		}
		if safePageSize > maxPageSize {
			safePageSize = maxPageSize
		}
	}
	return safePage, safePageSize
}

// TicketsNormalizeTicketStatus mirrors normalizeTicketStatus().
func TicketsNormalizeTicketStatus(status string) string {
	if status == "active" {
		return status
	}
	if status != "" && ticketsStatuses[status] {
		return status
	}
	return ""
}

// ticketsSliceUTF16 mirrors `value.slice(0, maxUnits)` for UTF-16 code units.
func ticketsSliceUTF16(value string, maxUnits int) string {
	units := 0
	for index, runeValue := range value {
		width := 1
		if runeValue > 0xFFFF {
			width = 2
		}
		if units+width > maxUnits {
			return value[:index]
		}
		units += width
	}
	return value
}

// ticketsJSTrim mirrors String#trim(): JS also strips U+FEFF, which
// unicode.IsSpace does not consider whitespace.
func ticketsJSTrim(value string) string {
	return strings.Trim(strings.TrimSpace(value), "\uFEFF")
}

// ticketsNormalizeSearchTerm mirrors normalizeSearchTerm().
func ticketsNormalizeSearchTerm(search *string) string {
	if search == nil {
		return ""
	}
	return ticketsSliceUTF16(ticketsJSTrim(*search), ticketsSearchMaxLength)
}

// ticketsJSToNumber mirrors JavaScript's unary plus for strings.
func ticketsJSToNumber(raw string) (float64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, true // +"" === 0
	}

	sign := 1.0
	unsigned := trimmed
	if strings.HasPrefix(unsigned, "+") {
		unsigned = unsigned[1:]
	} else if strings.HasPrefix(unsigned, "-") {
		sign = -1
		unsigned = unsigned[1:]
	}

	switch {
	case strings.EqualFold(unsigned, "Infinity"):
		return sign * math.Inf(1), true
	case strings.HasPrefix(unsigned, "0x") || strings.HasPrefix(unsigned, "0X"):
		if sign < 0 || unsigned != trimmed {
			return 0, false
		}
		parsed, err := strconv.ParseUint(unsigned[2:], 16, 64)
		if err != nil {
			return 0, false
		}
		return float64(parsed), true
	case strings.HasPrefix(unsigned, "0b") || strings.HasPrefix(unsigned, "0B"):
		if sign < 0 || unsigned != trimmed {
			return 0, false
		}
		parsed, err := strconv.ParseUint(unsigned[2:], 2, 64)
		if err != nil {
			return 0, false
		}
		return float64(parsed), true
	case strings.HasPrefix(unsigned, "0o") || strings.HasPrefix(unsigned, "0O"):
		if sign < 0 || unsigned != trimmed {
			return 0, false
		}
		parsed, err := strconv.ParseUint(unsigned[2:], 8, 64)
		if err != nil {
			return 0, false
		}
		return float64(parsed), true
	}

	if !ticketsJSNumberPattern.MatchString(trimmed) {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// ticketsJSNumberPattern is the decimal syntax JavaScript's unary plus accepts.
var ticketsJSNumberPattern = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

// ticketsGetSearchID mirrors getSearchId(): a positive integer search term is
// also matched against the ticket id.
func ticketsGetSearchID(searchTerm string) *int64 {
	parsed, ok := ticketsJSToNumber(searchTerm)
	if !ok || math.IsInf(parsed, 0) || math.IsNaN(parsed) || parsed != math.Trunc(parsed) || parsed <= 0 {
		return nil
	}
	if parsed > ticketsMaxSafeInteger {
		return nil
	}
	value := int64(parsed)
	return &value
}

// ticketsAddMinutes mirrors addMinutes().
func ticketsAddMinutes(date time.Time, minutes int) time.Time {
	return date.Add(time.Duration(minutes) * time.Minute)
}

// ticketsSLAMinutes mirrors getSlaMinutes().
func ticketsSLAMinutes(priority string) (firstResponse, resolution int) {
	switch priority {
	case string(models.TicketPriorityUrgent):
		return 30, 240
	case string(models.TicketPriorityHigh):
		return 120, 720
	case string(models.TicketPriorityLow):
		return 1440, 5760
	default:
		return 480, 2880
	}
}

// ticketsSLAInput mirrors computeSlaStatus' input object.
type ticketsSLAInput struct {
	Status                 string
	FirstResponseDueAt     *time.Time
	ResolutionDueAt        *time.Time
	FirstRespondedAt       *time.Time
	ResolvedAt             *time.Time
	ClosedAt               *time.Time
	LastMessageIsFromOwner *bool
}

// TicketsComputeSLAStatus mirrors computeSlaStatus().
func TicketsComputeSLAStatus(input ticketsSLAInput, now time.Time) string {
	if input.Status == string(models.TicketStatusClosed) {
		return "closed"
	}
	if input.ResolvedAt != nil || input.Status == string(models.TicketStatusResolved) {
		if input.ResolvedAt != nil && input.ResolutionDueAt != nil &&
			!input.ResolvedAt.After(*input.ResolutionDueAt) {
			return "met"
		}
		return "overdue"
	}

	hasFirstResponse := input.FirstRespondedAt != nil
	if !hasFirstResponse && input.FirstResponseDueAt != nil && input.FirstResponseDueAt.Before(now) {
		return "overdue"
	}
	if input.ResolutionDueAt != nil && input.ResolutionDueAt.Before(now) {
		return "overdue"
	}

	soon := time.Hour
	if (!hasFirstResponse && input.FirstResponseDueAt != nil &&
		input.FirstResponseDueAt.Sub(now) <= soon) ||
		(input.ResolutionDueAt != nil && input.ResolutionDueAt.Sub(now) <= soon) {
		return "due_soon"
	}

	if !hasFirstResponse {
		return "waiting_first_response"
	}
	if input.LastMessageIsFromOwner != nil && *input.LastMessageIsFromOwner {
		return "waiting_user"
	}
	return "waiting_internal"
}

// ticketsISO mirrors `value.toISOString()`; nil stays nil.
func ticketsISO(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := isoTimestamp(*value)
	return &formatted
}

// TicketsNormalizeTicketObjectLinkType mirrors normalizeTicketObjectLinkType().
func TicketsNormalizeTicketObjectLinkType(objectType string) string {
	if objectType == "" || !ticketsLinkTypes[objectType] {
		return ""
	}
	return objectType
}

// TicketsMaskEmail mirrors maskEmail().
func TicketsMaskEmail(email *string) *string {
	if email == nil || *email == "" {
		return nil
	}
	parts := strings.SplitN(*email, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	name := []rune(parts[0])
	visibleCount := 2
	if len(name) < visibleCount {
		visibleCount = len(name)
	}
	visible := string(name[:visibleCount])
	masked := len(name) - visibleCount
	if masked < 2 {
		masked = 2
	}
	return nilableString(visible + strings.Repeat("*", masked) + "@" + parts[1])
}

// ticketsDecimalToString mirrors decimalToString().
func ticketsDecimalToString(value *models.Decimal) *string {
	if value == nil {
		return nil
	}
	text := value.String()
	return &text
}

// TicketsKnowledgeSuggestion mirrors one knowledgeSuggestions entry.
type TicketsKnowledgeSuggestion struct {
	Title string   `json:"title"`
	Steps []string `json:"steps"`
}

// ticketsKnowledgeSuggestions mirrors getKnowledgeSuggestions().
func ticketsKnowledgeSuggestions(category string) []TicketsKnowledgeSuggestion {
	suggestions := map[string][]TicketsKnowledgeSuggestion{
		"billing": {
			{Title: "核对充值与余额", Steps: []string{"检查最近充值状态", "查看余额流水", "如需补偿只跳转调账审批"}},
			{Title: "支付异常处理", Steps: []string{"核对 provider 摘要", "检查是否存在重复回调", "必要时登记订单争议"}},
		},
		"technical": {
			{Title: "实例故障处理", Steps: []string{"确认实例状态和宿主机在线状态", "检查最近任务失败原因", "必要时转交付保障处理"}},
			{Title: "终端或网络问题", Steps: []string{"确认实例运行中", "检查 NAT/端口映射", "保留用户侧报错截图"}},
		},
		"abuse": {
			{Title: "风控处理", Steps: []string{"保留用户说明和证据", "只做内部备注", "高风险限制需走人工审核"}},
		},
		"general": {
			{Title: "通用客服处理", Steps: []string{"先确认用户账户状态", "核对关联实例或订单", "用户可见回复与内部备注分开记录"}},
		},
	}
	if value, ok := suggestions[category]; ok {
		return value
	}
	return suggestions["general"]
}

// ==================== row projections ====================

type ticketsTicketRow struct {
	ID                 int32      `gorm:"column:id"`
	UserID             int32      `gorm:"column:user_id"`
	HostID             *int32     `gorm:"column:host_id"`
	InstanceID         *int32     `gorm:"column:instance_id"`
	Subject            string     `gorm:"column:subject"`
	Category           string     `gorm:"column:category"`
	Priority           string     `gorm:"column:priority"`
	Status             string     `gorm:"column:status"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
	ResolvedAt         *time.Time `gorm:"column:resolved_at"`
	ClosedAt           *time.Time `gorm:"column:closed_at"`
	FirstResponseDueAt *time.Time `gorm:"column:first_response_due_at"`
	ResolutionDueAt    *time.Time `gorm:"column:resolution_due_at"`
	FirstRespondedAt   *time.Time `gorm:"column:first_responded_at"`
	SlaBreachedAt      *time.Time `gorm:"column:sla_breached_at"`

	UserRefID            int32   `gorm:"column:user_ref_id"`
	UserRefUsername      string  `gorm:"column:user_ref_username"`
	UserRefAvatarStyle   string  `gorm:"column:user_ref_avatar_style"`
	UserRefAvatarBadgeID *string `gorm:"column:user_ref_avatar_badge_id"`

	HostRefID          *int32  `gorm:"column:host_ref_id"`
	HostRefName        *string `gorm:"column:host_ref_name"`
	HostRefUserID      *int32  `gorm:"column:host_ref_user_id"`
	HostRefCountryCode *string `gorm:"column:host_ref_country_code"`

	InstanceRefID          *int32  `gorm:"column:instance_ref_id"`
	InstanceRefName        *string `gorm:"column:instance_ref_name"`
	InstanceRefStatus      *string `gorm:"column:instance_ref_status"`
	InstanceRefIconBadgeID *string `gorm:"column:instance_ref_icon_badge_id"`

	// Detail-only instance columns (NULL for the list query).
	InstanceRefIncusID     *string `gorm:"column:instance_ref_incus_id"`
	InstanceRefIPv4        *string `gorm:"column:instance_ref_ipv4"`
	InstanceRefIPv6        *string `gorm:"column:instance_ref_ipv6"`
	InstanceRefCPU         *int32  `gorm:"column:instance_ref_cpu"`
	InstanceRefMemory      *int32  `gorm:"column:instance_ref_memory"`
	InstanceRefDisk        *int32  `gorm:"column:instance_ref_disk"`
	InstanceRefImage       *string `gorm:"column:instance_ref_image"`
	InstanceRefPackageName *string `gorm:"column:instance_ref_package_name"`

	MessageCount    int64      `gorm:"column:message_count"`
	LastContent     *string    `gorm:"column:last_content"`
	LastIsFromOwner *bool      `gorm:"column:last_is_from_owner"`
	LastCreatedAt   *time.Time `gorm:"column:last_created_at"`
}

const ticketsListSelect = `
	t.id, t.user_id, t.host_id, t.instance_id, t.subject, t.category,
	t.priority::text AS priority, t.status::text AS status,
	t.created_at, t.updated_at, t.resolved_at, t.closed_at,
	t.first_response_due_at, t.resolution_due_at, t.first_responded_at, t.sla_breached_at,
	u.id AS user_ref_id, u.username AS user_ref_username,
	u.avatar_style AS user_ref_avatar_style, u.avatar_badge_id AS user_ref_avatar_badge_id,
	h.id AS host_ref_id, h.name AS host_ref_name,
	h.user_id AS host_ref_user_id, h.country_code AS host_ref_country_code,
	i.id AS instance_ref_id, i.name AS instance_ref_name,
	i.status::text AS instance_ref_status, i.icon_badge_id AS instance_ref_icon_badge_id,
	(SELECT COUNT(*) FROM ticket_messages m WHERE m.ticket_id = t.id) AS message_count,
	lm.content AS last_content, lm.is_from_owner AS last_is_from_owner,
	lm.created_at AS last_created_at`

const ticketsDetailSelect = `
	t.id, t.user_id, t.host_id, t.instance_id, t.subject, t.category,
	t.priority::text AS priority, t.status::text AS status,
	t.created_at, t.updated_at, t.resolved_at, t.closed_at,
	t.first_response_due_at, t.resolution_due_at, t.first_responded_at, t.sla_breached_at,
	u.id AS user_ref_id, u.username AS user_ref_username,
	u.avatar_style AS user_ref_avatar_style, u.avatar_badge_id AS user_ref_avatar_badge_id,
	h.id AS host_ref_id, h.name AS host_ref_name,
	h.user_id AS host_ref_user_id, h.country_code AS host_ref_country_code,
	i.id AS instance_ref_id, i.name AS instance_ref_name,
	i.status::text AS instance_ref_status, i.icon_badge_id AS instance_ref_icon_badge_id,
	i.incus_id AS instance_ref_incus_id, i.ipv4 AS instance_ref_ipv4, i.ipv6 AS instance_ref_ipv6,
	i.cpu AS instance_ref_cpu, i.memory AS instance_ref_memory, i.disk AS instance_ref_disk,
	i.image AS instance_ref_image, p.name AS instance_ref_package_name,
	(SELECT COUNT(*) FROM ticket_messages m WHERE m.ticket_id = t.id) AS message_count,
	lm.content AS last_content, lm.is_from_owner AS last_is_from_owner,
	lm.created_at AS last_created_at`

const ticketsFrom = `
	FROM tickets t
	JOIN users u ON u.id = t.user_id
	LEFT JOIN hosts h ON h.id = t.host_id
	LEFT JOIN instances i ON i.id = t.instance_id`

const ticketsLastMessageJoin = `
	LEFT JOIN LATERAL (
		SELECT content, is_from_owner, created_at
		FROM ticket_messages
		WHERE ticket_id = t.id
		ORDER BY created_at DESC
		LIMIT 1
	) lm ON TRUE`

func ticketsUserRef(row ticketsTicketRow) TicketsTicketUserRef {
	return TicketsTicketUserRef{
		ID:            row.UserRefID,
		Username:      row.UserRefUsername,
		AvatarStyle:   row.UserRefAvatarStyle,
		AvatarBadgeID: row.UserRefAvatarBadgeID,
	}
}

func ticketsHostRef(row ticketsTicketRow) *TicketsTicketHostRef {
	if row.HostRefID == nil {
		return nil
	}
	return &TicketsTicketHostRef{
		ID:          *row.HostRefID,
		Name:        derefString(row.HostRefName),
		UserID:      derefInt32(row.HostRefUserID),
		CountryCode: derefString(row.HostRefCountryCode),
	}
}

func ticketsInstanceRef(row ticketsTicketRow) *TicketsTicketInstanceRef {
	if row.InstanceRefID == nil {
		return nil
	}
	return &TicketsTicketInstanceRef{
		ID:          *row.InstanceRefID,
		Name:        derefString(row.InstanceRefName),
		Status:      derefString(row.InstanceRefStatus),
		IconBadgeID: row.InstanceRefIconBadgeID,
	}
}

func ticketsLastMessageRef(row ticketsTicketRow) *TicketsLastMessage {
	if row.LastCreatedAt == nil {
		return nil
	}
	return &TicketsLastMessage{
		Content:     derefString(row.LastContent),
		IsFromOwner: row.LastIsFromOwner != nil && *row.LastIsFromOwner,
		CreatedAt:   isoTimestamp(*row.LastCreatedAt),
	}
}

func ticketsSLAFromRow(row ticketsTicketRow, now time.Time) string {
	return TicketsComputeSLAStatus(ticketsSLAInput{
		Status:                 row.Status,
		FirstResponseDueAt:     row.FirstResponseDueAt,
		ResolutionDueAt:        row.ResolutionDueAt,
		FirstRespondedAt:       row.FirstRespondedAt,
		ResolvedAt:             row.ResolvedAt,
		ClosedAt:               row.ClosedAt,
		LastMessageIsFromOwner: row.LastIsFromOwner,
	}, now)
}

// ticketsMapListDetail builds the list projection plus needsReply.
func ticketsMapListDetail(row ticketsTicketRow, now time.Time, needsReply bool) TicketsTicketWithDetailsExtended {
	return TicketsTicketWithDetailsExtended{
		TicketsTicketWithDetails: TicketsTicketWithDetails{
			ID:                 row.ID,
			UserID:             row.UserID,
			HostID:             row.HostID,
			InstanceID:         row.InstanceID,
			Subject:            row.Subject,
			Category:           row.Category,
			Priority:           row.Priority,
			Status:             row.Status,
			CreatedAt:          isoTimestamp(row.CreatedAt),
			UpdatedAt:          isoTimestamp(row.UpdatedAt),
			ResolvedAt:         ticketsISO(row.ResolvedAt),
			ClosedAt:           ticketsISO(row.ClosedAt),
			FirstResponseDueAt: ticketsISO(row.FirstResponseDueAt),
			ResolutionDueAt:    ticketsISO(row.ResolutionDueAt),
			FirstRespondedAt:   ticketsISO(row.FirstRespondedAt),
			SlaBreachedAt:      ticketsISO(row.SlaBreachedAt),
			SLAStatus:          ticketsSLAFromRow(row, now),
			User:               ticketsUserRef(row),
			Host:               ticketsHostRef(row),
			Instance:           ticketsInstanceRef(row),
			MessageCount:       row.MessageCount,
			LastMessage:        ticketsLastMessageRef(row),
		},
		NeedsReply: needsReply,
	}
}

// ==================== 工单操作 ====================

// TicketsCreateTicket mirrors createTicket().
func TicketsCreateTicket(ctx context.Context, data TicketsCreateTicketData) (ticketID int32, messageID int32, err error) {
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		createdAt := time.Now()
		priority := string(models.TicketPriorityNormal)
		if data.Priority != nil && *data.Priority != "" {
			priority = string(*data.Priority)
		}
		firstResponseMinutes, resolutionMinutes := ticketsSLAMinutes(priority)
		category := "general"
		if data.Category != nil && *data.Category != "" {
			category = *data.Category
		}

		var ticketRow struct {
			ID int32 `gorm:"column:id"`
		}
		if err := tx.Raw(
			`INSERT INTO tickets
				(user_id, host_id, instance_id, subject, category, priority, status,
				 created_at, updated_at, first_response_due_at, resolution_due_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?::"TicketStatus", ?, ?, ?, ?)
			 RETURNING id`,
			data.UserID, data.HostID, data.InstanceID, data.Subject, category, priority,
			string(models.TicketStatusOpen),
			createdAt, createdAt,
			ticketsAddMinutes(createdAt, firstResponseMinutes),
			ticketsAddMinutes(createdAt, resolutionMinutes),
		).Scan(&ticketRow).Error; err != nil {
			return err
		}
		ticketID = ticketRow.ID

		var messageRow struct {
			ID int32 `gorm:"column:id"`
		}
		if err := tx.Raw(
			`INSERT INTO ticket_messages (ticket_id, sender_id, content, is_from_owner, created_at)
			 VALUES (?, ?, ?, false, ?) RETURNING id`,
			ticketID, data.UserID, data.Content, createdAt,
		).Scan(&messageRow).Error; err != nil {
			return err
		}
		messageID = messageRow.ID

		if len(data.Attachments) > 0 {
			if err := ticketsInsertAttachments(tx, ticketID, messageID, data.UserID, data.Attachments, createdAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return ticketID, messageID, nil
}

// ticketsInsertAttachments mirrors ticketMessageAttachment.createMany().
func ticketsInsertAttachments(tx *gorm.DB, ticketID, messageID, uploaderID int32, attachments []TicketsAttachmentData, createdAt time.Time) error {
	placeholders := make([]string, 0, len(attachments))
	args := make([]any, 0, len(attachments)*14)
	for _, attachment := range attachments {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			ticketID, messageID, uploaderID,
			attachment.Provider, attachment.ProviderVersion, attachment.ProviderFileID,
			attachment.Filename, attachment.OriginalName, attachment.MimeType,
			attachment.SizeBytes, attachment.Width, attachment.Height,
			attachment.URL, attachment.ThumbnailURL,
		)
	}
	sql := `INSERT INTO ticket_message_attachments
		(ticket_id, message_id, uploader_id, provider, provider_version, provider_file_id,
		 filename, original_name, mime_type, size_bytes, width, height, url, thumbnail_url)
		VALUES ` + strings.Join(placeholders, ", ")
	return tx.Exec(sql, args...).Error
}

// TicketsGetTicketByID mirrors getTicketById(); returns nil when absent.
func TicketsGetTicketByID(ctx context.Context, ticketID int32) (*TicketsTicketDetail, error) {
	sql := "SELECT " + ticketsDetailSelect + ticketsFrom +
		" LEFT JOIN packages p ON p.id = i.package_id" + ticketsLastMessageJoin +
		" WHERE t.id = ? LIMIT 1"

	var rows []ticketsTicketRow
	if err := DB.WithContext(ctx).Raw(sql, ticketID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]

	var imageName *string
	if row.InstanceRefImage != nil && *row.InstanceRefImage != "" {
		var image struct {
			Name string `gorm:"column:name"`
		}
		if err := DB.WithContext(ctx).
			Raw("SELECT name FROM system_images WHERE remote_alias = ? LIMIT 1", *row.InstanceRefImage).
			Scan(&image).Error; err != nil {
			return nil, err
		}
		if image.Name != "" {
			imageName = &image.Name
		}
	}

	var instance *TicketsTicketInstanceDetail
	if row.InstanceRefID != nil {
		instance = &TicketsTicketInstanceDetail{
			ID:          *row.InstanceRefID,
			Name:        derefString(row.InstanceRefName),
			Status:      derefString(row.InstanceRefStatus),
			IconBadgeID: row.InstanceRefIconBadgeID,
			IncusID:     row.InstanceRefIncusID,
			IPv4:        row.InstanceRefIPv4,
			IPv6:        row.InstanceRefIPv6,
			CPU:         derefInt32(row.InstanceRefCPU),
			Memory:      derefInt32(row.InstanceRefMemory),
			Disk:        derefInt32(row.InstanceRefDisk),
			Image:       derefString(row.InstanceRefImage),
			ImageName:   imageName,
			PackageName: row.InstanceRefPackageName,
		}
	}

	detail := &TicketsTicketDetail{
		ID:                 row.ID,
		UserID:             row.UserID,
		HostID:             row.HostID,
		InstanceID:         row.InstanceID,
		Subject:            row.Subject,
		Category:           row.Category,
		Priority:           row.Priority,
		Status:             row.Status,
		CreatedAt:          isoTimestamp(row.CreatedAt),
		UpdatedAt:          isoTimestamp(row.UpdatedAt),
		ResolvedAt:         ticketsISO(row.ResolvedAt),
		ClosedAt:           ticketsISO(row.ClosedAt),
		FirstResponseDueAt: ticketsISO(row.FirstResponseDueAt),
		ResolutionDueAt:    ticketsISO(row.ResolutionDueAt),
		FirstRespondedAt:   ticketsISO(row.FirstRespondedAt),
		SlaBreachedAt:      ticketsISO(row.SlaBreachedAt),
		SLAStatus:          ticketsSLAFromRow(row, time.Now()),
		User:               ticketsUserRef(row),
		Host:               ticketsHostRef(row),
		Instance:           instance,
		MessageCount:       row.MessageCount,
		LastMessage:        ticketsLastMessageRef(row),
	}
	return detail, nil
}

// ticketsBuildUserWhere mirrors the where-builder inside getUserTickets().
func ticketsBuildUserWhere(userID int32, status, searchTerm string) (string, []any) {
	conditions := []string{"t.user_id = ?"}
	args := []any{userID}

	if status == "active" {
		conditions = append(conditions, "t.status <> 'closed'")
	} else if status != "" {
		conditions = append(conditions, "t.status = ?")
		args = append(args, status)
	}

	if searchTerm != "" {
		pattern := "%" + searchTerm + "%"
		if searchID := ticketsGetSearchID(searchTerm); searchID != nil {
			conditions = append(conditions, "(t.id = ? OR t.subject ILIKE ?)")
			args = append(args, *searchID, pattern)
		} else {
			conditions = append(conditions, "t.subject ILIKE ?")
			args = append(args, pattern)
		}
	}

	return strings.Join(conditions, " AND "), args
}

// TicketsGetUserTickets mirrors getUserTickets().
func TicketsGetUserTickets(ctx context.Context, userID int32, options TicketsListOptions) (TicketsTicketPage, error) {
	page, pageSize := ticketsClampPagination(options.Page, options.PageSize, 10, ticketsMaxPageSize)
	status := TicketsNormalizeTicketStatus(options.Status)
	searchTerm := ticketsNormalizeSearchTerm(options.Search)

	where, args := ticketsBuildUserWhere(userID, status, searchTerm)

	var total int64
	if err := DB.WithContext(ctx).Raw("SELECT COUNT(*) FROM tickets t WHERE "+where, args...).
		Scan(&total).Error; err != nil {
		return TicketsTicketPage{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	sql := "SELECT " + ticketsListSelect + ticketsFrom + ticketsLastMessageJoin +
		" WHERE " + where + " ORDER BY t.updated_at DESC LIMIT ? OFFSET ?"

	var rows []ticketsTicketRow
	if err := DB.WithContext(ctx).Raw(sql, listArgs...).Scan(&rows).Error; err != nil {
		return TicketsTicketPage{}, err
	}

	now := time.Now()
	tickets := make([]TicketsTicketWithDetailsExtended, 0, len(rows))
	for _, row := range rows {
		// 用户视角：最后一条消息来自宿主机主人 → 需要用户回复。
		needsReply := row.Status != string(models.TicketStatusClosed) &&
			row.LastIsFromOwner != nil && *row.LastIsFromOwner
		tickets = append(tickets, ticketsMapListDetail(row, now, needsReply))
	}

	return TicketsTicketPage{
		Tickets:    tickets,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages(total, pageSize),
	}, nil
}

// ticketsBuildHostWhere mirrors the where-builder inside getHostTickets().
func ticketsBuildHostWhere(hostID int32, status, searchTerm string) (string, []any) {
	conditions := []string{"t.host_id = ?"}
	args := []any{hostID}

	if status == "active" {
		conditions = append(conditions, "t.status <> 'closed'")
	} else if status != "" {
		conditions = append(conditions, "t.status = ?")
		args = append(args, status)
	}

	if searchTerm != "" {
		pattern := "%" + searchTerm + "%"
		if searchID := ticketsGetSearchID(searchTerm); searchID != nil {
			conditions = append(conditions, "(t.id = ? OR t.subject ILIKE ? OR u.username ILIKE ?)")
			args = append(args, *searchID, pattern, pattern)
		} else {
			conditions = append(conditions, "(t.subject ILIKE ? OR u.username ILIKE ?)")
			args = append(args, pattern, pattern)
		}
	}

	return strings.Join(conditions, " AND "), args
}

// ticketsSortQueue mirrors the Node queue comparator: needsReply first, then
// status, then priority, then updatedAt (oldest-first for needsReply rows), with
// the ticket id as the deterministic final tie-break.
func ticketsSortQueue(tickets []TicketsTicketWithDetailsExtended) {
	priorityOrder := map[string]int{
		string(models.TicketPriorityUrgent): 0,
		string(models.TicketPriorityHigh):   1,
		string(models.TicketPriorityNormal): 2,
		string(models.TicketPriorityLow):    3,
	}
	statusOrder := map[string]int{
		string(models.TicketStatusOpen):       0,
		string(models.TicketStatusInProgress): 1,
		string(models.TicketStatusResolved):   2,
		string(models.TicketStatusClosed):     3,
	}

	sortSliceStable(tickets, func(a, b TicketsTicketWithDetailsExtended) bool {
		if a.NeedsReply != b.NeedsReply {
			return a.NeedsReply
		}
		if a.Status != b.Status {
			return statusOrder[a.Status] < statusOrder[b.Status]
		}
		if a.Priority != b.Priority {
			return priorityOrder[a.Priority] < priorityOrder[b.Priority]
		}
		aTime := ticketsParseISOTime(a.UpdatedAt)
		bTime := ticketsParseISOTime(b.UpdatedAt)
		if aTime != bTime {
			if a.NeedsReply {
				return aTime < bTime
			}
			return bTime < aTime
		}
		return a.ID < b.ID
	})
}

// ticketsParseISOTime parses the RFC3339-with-millis form we emit.
func ticketsParseISOTime(value string) int64 {
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z07:00", value)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

// ticketsOwnerNeedsReply mirrors the owner-side needsReply rule (last message
// comes from the customer).
func ticketsOwnerNeedsReply(row ticketsTicketRow) bool {
	return row.Status != string(models.TicketStatusClosed) &&
		row.LastIsFromOwner != nil && !*row.LastIsFromOwner
}

// TicketsGetHostTickets mirrors getHostTickets().
func TicketsGetHostTickets(ctx context.Context, hostID int32, options TicketsListOptions) (TicketsTicketPage, error) {
	page, pageSize := ticketsClampPagination(options.Page, options.PageSize, 10, ticketsMaxPageSize)
	status := TicketsNormalizeTicketStatus(options.Status)
	searchTerm := ticketsNormalizeSearchTerm(options.Search)

	where, args := ticketsBuildHostWhere(hostID, status, searchTerm)
	sql := "SELECT " + ticketsListSelect + ticketsFrom + ticketsLastMessageJoin +
		" WHERE " + where + " ORDER BY t.updated_at DESC"

	var rows []ticketsTicketRow
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return TicketsTicketPage{}, err
	}

	now := time.Now()
	tickets := make([]TicketsTicketWithDetailsExtended, 0, len(rows))
	for _, row := range rows {
		tickets = append(tickets, ticketsMapListDetail(row, now, ticketsOwnerNeedsReply(row)))
	}

	ticketsSortQueue(tickets)

	total := int64(len(tickets))
	start := (page - 1) * pageSize
	if start > len(tickets) {
		start = len(tickets)
	}
	end := start + pageSize
	if end > len(tickets) {
		end = len(tickets)
	}
	paginated := tickets[start:end]

	return TicketsTicketPage{
		Tickets:    paginated,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages(total, pageSize),
	}, nil
}

// TicketsGetOwnerAllTickets mirrors getOwnerAllTickets(). ownerID nil means
// "administrator view" (every ticket).
func TicketsGetOwnerAllTickets(ctx context.Context, ownerID *int32, options TicketsOwnerListOptions) (TicketsTicketPage, error) {
	page, pageSize := ticketsClampPagination(options.Page, options.PageSize, 10, ticketsMaxPageSize)
	status := TicketsNormalizeTicketStatus(options.Status)
	searchTerm := ticketsNormalizeSearchTerm(options.Search)

	conditions := make([]string, 0, 4)
	args := make([]any, 0, 4)

	sourceType := options.SourceType
	if ownerID != nil {
		conditions = append(conditions, "h.user_id = ?")
		args = append(args, *ownerID)
	} else if sourceType == "user" {
		conditions = append(conditions, "t.host_id IS NULL")
	} else if sourceType == "official" {
		conditions = append(conditions, "EXISTS (SELECT 1 FROM users hu WHERE hu.id = h.user_id AND hu.role = 'admin')")
	}

	if status == "active" {
		conditions = append(conditions, "t.status <> 'closed'")
	} else if status != "" {
		conditions = append(conditions, "t.status = ?")
		args = append(args, status)
	}

	if options.HostID != nil && *options.HostID > 0 && sourceType != "user" {
		conditions = append(conditions, "t.host_id = ?")
		args = append(args, *options.HostID)
	}

	if searchTerm != "" {
		pattern := "%" + searchTerm + "%"
		if searchID := ticketsGetSearchID(searchTerm); searchID != nil {
			conditions = append(conditions, "(t.id = ? OR t.subject ILIKE ? OR u.username ILIKE ?)")
			args = append(args, *searchID, pattern, pattern)
		} else {
			conditions = append(conditions, "(t.subject ILIKE ? OR u.username ILIKE ?)")
			args = append(args, pattern, pattern)
		}
	}

	where := "1=1"
	if len(conditions) > 0 {
		where = strings.Join(conditions, " AND ")
	}

	sql := "SELECT " + ticketsListSelect + ticketsFrom + ticketsLastMessageJoin +
		" WHERE " + where + " ORDER BY t.updated_at DESC"

	var rows []ticketsTicketRow
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return TicketsTicketPage{}, err
	}

	now := time.Now()
	tickets := make([]TicketsTicketWithDetailsExtended, 0, len(rows))
	for _, row := range rows {
		tickets = append(tickets, ticketsMapListDetail(row, now, ticketsOwnerNeedsReply(row)))
	}

	if options.Queue != "" {
		filtered := make([]TicketsTicketWithDetailsExtended, 0, len(tickets))
		for _, ticket := range tickets {
			if options.Queue == "pending" {
				if ticket.NeedsReply {
					filtered = append(filtered, ticket)
				}
				continue
			}
			if ticket.SLAStatus == options.Queue {
				filtered = append(filtered, ticket)
			}
		}
		tickets = filtered
	}

	ticketsSortQueue(tickets)

	total := int64(len(tickets))
	start := (page - 1) * pageSize
	if start > len(tickets) {
		start = len(tickets)
	}
	end := start + pageSize
	if end > len(tickets) {
		end = len(tickets)
	}
	paginated := tickets[start:end]

	return TicketsTicketPage{
		Tickets:    paginated,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages(total, pageSize),
	}, nil
}

// TicketsGetTicketMessages mirrors getTicketMessages().
func TicketsGetTicketMessages(ctx context.Context, ticketID int32, page, pageSize *int) (TicketsMessagePage, error) {
	safePage, safePageSize := ticketsClampPagination(page, pageSize, 50, ticketsMaxPageSize)
	skip := (safePage - 1) * safePageSize

	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM ticket_messages WHERE ticket_id = ?", ticketID,
	).Scan(&total).Error; err != nil {
		return TicketsMessagePage{}, err
	}

	var rows []struct {
		ID            int32     `gorm:"column:id"`
		TicketID      int32     `gorm:"column:ticket_id"`
		SenderID      int32     `gorm:"column:sender_id"`
		Content       string    `gorm:"column:content"`
		IsFromOwner   bool      `gorm:"column:is_from_owner"`
		CreatedAt     time.Time `gorm:"column:created_at"`
		Username      string    `gorm:"column:username"`
		AvatarStyle   string    `gorm:"column:avatar_style"`
		AvatarBadgeID *string   `gorm:"column:avatar_badge_id"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT m.id, m.ticket_id, m.sender_id, m.content, m.is_from_owner, m.created_at,
		        s.username, s.avatar_style, s.avatar_badge_id
		 FROM ticket_messages m
		 JOIN users s ON s.id = m.sender_id
		 WHERE m.ticket_id = ?
		 ORDER BY m.created_at ASC
		 LIMIT ? OFFSET ?`, ticketID, safePageSize, skip,
	).Scan(&rows).Error; err != nil {
		return TicketsMessagePage{}, err
	}

	messageIDs := make([]int32, 0, len(rows))
	for _, row := range rows {
		messageIDs = append(messageIDs, row.ID)
	}
	attachmentsByMessage, err := ticketsLoadAttachments(ctx, messageIDs)
	if err != nil {
		return TicketsMessagePage{}, err
	}

	messages := make([]TicketsTicketMessage, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, TicketsTicketMessage{
			ID:          row.ID,
			TicketID:    row.TicketID,
			SenderID:    row.SenderID,
			Content:     row.Content,
			IsFromOwner: row.IsFromOwner,
			CreatedAt:   isoTimestamp(row.CreatedAt),
			Attachments: attachmentsByMessage[row.ID],
			Sender: TicketsTicketUserRef{
				ID:            row.SenderID,
				Username:      row.Username,
				AvatarStyle:   row.AvatarStyle,
				AvatarBadgeID: row.AvatarBadgeID,
			},
		})
	}

	return TicketsMessagePage{
		Messages:   messages,
		Total:      total,
		Page:       safePage,
		PageSize:   safePageSize,
		TotalPages: totalPages(total, safePageSize),
	}, nil
}

type ticketsAttachmentRow struct {
	ID              int32     `gorm:"column:id"`
	TicketID        int32     `gorm:"column:ticket_id"`
	MessageID       int32     `gorm:"column:message_id"`
	UploaderID      int32     `gorm:"column:uploader_id"`
	Provider        string    `gorm:"column:provider"`
	ProviderVersion string    `gorm:"column:provider_version"`
	ProviderFileID  *string   `gorm:"column:provider_file_id"`
	Filename        string    `gorm:"column:filename"`
	OriginalName    string    `gorm:"column:original_name"`
	MimeType        string    `gorm:"column:mime_type"`
	SizeBytes       int32     `gorm:"column:size_bytes"`
	Width           *int32    `gorm:"column:width"`
	Height          *int32    `gorm:"column:height"`
	CreatedAt       time.Time `gorm:"column:created_at"`
}

func ticketsLoadAttachments(ctx context.Context, messageIDs []int32) (map[int32][]TicketsTicketMessageAttachment, error) {
	result := map[int32][]TicketsTicketMessageAttachment{}
	if len(messageIDs) == 0 {
		return result, nil
	}

	placeholders := make([]string, 0, len(messageIDs))
	args := make([]any, 0, len(messageIDs))
	for _, id := range messageIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}

	var rows []ticketsAttachmentRow
	sql := `SELECT id, ticket_id, message_id, uploader_id, provider, provider_version,
	               provider_file_id, filename, original_name, mime_type, size_bytes,
	               width, height, created_at
	        FROM ticket_message_attachments
	        WHERE message_id IN (` + strings.Join(placeholders, ", ") + `)
	        ORDER BY id ASC`
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	for _, row := range rows {
		result[row.MessageID] = append(result[row.MessageID], TicketsTicketMessageAttachment{
			ID:              row.ID,
			TicketID:        row.TicketID,
			MessageID:       row.MessageID,
			UploaderID:      row.UploaderID,
			Provider:        row.Provider,
			ProviderVersion: row.ProviderVersion,
			ProviderFileID:  row.ProviderFileID,
			Filename:        row.Filename,
			OriginalName:    row.OriginalName,
			MimeType:        row.MimeType,
			SizeBytes:       row.SizeBytes,
			Width:           row.Width,
			Height:          row.Height,
			CreatedAt:       isoTimestamp(row.CreatedAt),
		})
	}
	return result, nil
}

// TicketsCannotReplyClosedMessage is the exact TypeScript Error message; routes
// recognise it as a handled payload error and map it to 400 INVALID_PARAMS.
const TicketsCannotReplyClosedMessage = "Cannot reply to a closed ticket"

// TicketsAddTicketMessage mirrors addTicketMessage().
func TicketsAddTicketMessage(ctx context.Context, ticketID, senderID int32, content string, isFromOwner bool, attachments []TicketsAttachmentData) (TicketsTicketMessage, error) {
	var messageID int32

	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repliedAt := time.Now()

		update := tx.Exec(
			"UPDATE tickets SET updated_at = ? WHERE id = ? AND status <> 'closed'",
			repliedAt, ticketID,
		)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected == 0 {
			return errors.New(TicketsCannotReplyClosedMessage)
		}

		if isFromOwner {
			if err := tx.Exec(
				"UPDATE tickets SET first_responded_at = ?, updated_at = ? WHERE id = ? AND first_responded_at IS NULL",
				repliedAt, repliedAt, ticketID,
			).Error; err != nil {
				return err
			}
			if err := tx.Exec(
				"UPDATE tickets SET status = ?, updated_at = ? WHERE id = ? AND status = ?",
				string(models.TicketStatusInProgress), repliedAt, ticketID, string(models.TicketStatusOpen),
			).Error; err != nil {
				return err
			}
		} else {
			// 客户回复已解决工单时重开；open 工单仍等待客服首响推进状态。
			if err := tx.Exec(
				`UPDATE tickets
				    SET status = ?, resolved_at = NULL, closed_at = NULL, updated_at = ?
				  WHERE id = ? AND status = ?`,
				string(models.TicketStatusInProgress), repliedAt, ticketID, string(models.TicketStatusResolved),
			).Error; err != nil {
				return err
			}
		}

		var created struct {
			ID int32 `gorm:"column:id"`
		}
		if err := tx.Raw(
			`INSERT INTO ticket_messages (ticket_id, sender_id, content, is_from_owner, created_at)
			 VALUES (?, ?, ?, ?, ?) RETURNING id`,
			ticketID, senderID, content, isFromOwner, repliedAt,
		).Scan(&created).Error; err != nil {
			return err
		}
		messageID = created.ID

		if len(attachments) > 0 {
			if err := ticketsInsertAttachments(tx, ticketID, messageID, senderID, attachments, repliedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return TicketsTicketMessage{}, err
	}

	return TicketsGetTicketMessageByID(ctx, messageID)
}

// TicketsGetTicketMessageByID mirrors the findUniqueOrThrow(include sender +
// attachments) used by addTicketMessage.
func TicketsGetTicketMessageByID(ctx context.Context, messageID int32) (TicketsTicketMessage, error) {
	var rows []struct {
		ID            int32     `gorm:"column:id"`
		TicketID      int32     `gorm:"column:ticket_id"`
		SenderID      int32     `gorm:"column:sender_id"`
		Content       string    `gorm:"column:content"`
		IsFromOwner   bool      `gorm:"column:is_from_owner"`
		CreatedAt     time.Time `gorm:"column:created_at"`
		Username      string    `gorm:"column:username"`
		AvatarStyle   string    `gorm:"column:avatar_style"`
		AvatarBadgeID *string   `gorm:"column:avatar_badge_id"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT m.id, m.ticket_id, m.sender_id, m.content, m.is_from_owner, m.created_at,
		        s.username, s.avatar_style, s.avatar_badge_id
		 FROM ticket_messages m
		 JOIN users s ON s.id = m.sender_id
		 WHERE m.id = ? LIMIT 1`, messageID,
	).Scan(&rows).Error; err != nil {
		return TicketsTicketMessage{}, err
	}
	if len(rows) == 0 {
		return TicketsTicketMessage{}, fmt.Errorf("ticket message %d not found", messageID)
	}

	row := rows[0]
	attachmentsByMessage, err := ticketsLoadAttachments(ctx, []int32{messageID})
	if err != nil {
		return TicketsTicketMessage{}, err
	}

	return TicketsTicketMessage{
		ID:          row.ID,
		TicketID:    row.TicketID,
		SenderID:    row.SenderID,
		Content:     row.Content,
		IsFromOwner: row.IsFromOwner,
		CreatedAt:   isoTimestamp(row.CreatedAt),
		Attachments: attachmentsByMessage[messageID],
		Sender: TicketsTicketUserRef{
			ID:            row.SenderID,
			Username:      row.Username,
			AvatarStyle:   row.AvatarStyle,
			AvatarBadgeID: row.AvatarBadgeID,
		},
	}, nil
}

// TicketsUpdateTicketStatus mirrors updateTicketStatus().
func TicketsUpdateTicketStatus(ctx context.Context, ticketID int32, status models.TicketStatus) error {
	type update struct {
		columns []string
		args    []any
	}

	var (
		columns []string
		args    []any
	)

	switch status {
	case models.TicketStatusResolved:
		var rows []struct {
			ResolutionDueAt *time.Time `gorm:"column:resolution_due_at"`
			SlaBreachedAt   *time.Time `gorm:"column:sla_breached_at"`
		}
		if err := DB.WithContext(ctx).Raw(
			"SELECT resolution_due_at, sla_breached_at FROM tickets WHERE id = ? LIMIT 1", ticketID,
		).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			// Prisma findUniqueOrThrow(P2025) → 500 in the route.
			return fmt.Errorf("ticket %d not found", ticketID)
		}

		resolvedAt := time.Now()
		columns = append(columns, "resolved_at = ?")
		args = append(args, resolvedAt)
		if rows[0].ResolutionDueAt != nil && resolvedAt.After(*rows[0].ResolutionDueAt) && rows[0].SlaBreachedAt == nil {
			columns = append(columns, "sla_breached_at = ?")
			args = append(args, resolvedAt)
		}

	case models.TicketStatusClosed:
		columns = append(columns, "closed_at = ?")
		args = append(args, time.Now())

	case models.TicketStatusOpen, models.TicketStatusInProgress:
		columns = append(columns, "resolved_at = NULL", "closed_at = NULL")
	}

	columns = append(columns, "status = ?", "updated_at = ?")
	args = append(args, string(status), time.Now())

	sql := "UPDATE tickets SET " + strings.Join(columns, ", ") + " WHERE id = ?"
	args = append(args, ticketID)
	return DB.WithContext(ctx).Exec(sql, args...).Error
}

// TicketsCanUserCreateTicket mirrors canUserCreateTicket().
func TicketsCanUserCreateTicket(ctx context.Context, userID, hostID int32) (bool, error) {
	var count int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM instances WHERE user_id = ? AND host_id = ? AND status <> 'deleted'",
		userID, hostID,
	).Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// TicketsCanUserAccessTicket mirrors canUserAccessTicket().
func TicketsCanUserAccessTicket(ctx context.Context, userID, ticketID int32, userRole string) (TicketsTicketAccess, error) {
	var rows []struct {
		UserID     int32  `gorm:"column:user_id"`
		HostUserID *int32 `gorm:"column:host_user_id"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT t.user_id, h.user_id AS host_user_id
		 FROM tickets t
		 LEFT JOIN hosts h ON h.id = t.host_id
		 WHERE t.id = ? LIMIT 1`, ticketID,
	).Scan(&rows).Error; err != nil {
		return TicketsTicketAccess{}, err
	}
	if len(rows) == 0 {
		return TicketsTicketAccess{}, nil
	}

	isCreator := rows[0].UserID == userID
	isOwner := rows[0].HostUserID != nil && *rows[0].HostUserID == userID
	isAdmin := userRole == "admin"

	return TicketsTicketAccess{
		CanAccess: isCreator || isOwner || isAdmin,
		// 管理员视为所有者
		IsOwner:   isOwner || isAdmin,
		IsCreator: isCreator,
		IsAdmin:   isAdmin,
	}, nil
}

// TicketsGetOwnerPendingTicketCount mirrors getOwnerPendingTicketCount().
func TicketsGetOwnerPendingTicketCount(ctx context.Context, ownerID *int32) (int64, error) {
	where := "t.status <> 'closed'"
	args := []any{}
	if ownerID != nil {
		where += " AND h.user_id = ?"
		args = append(args, *ownerID)
	}

	var count int64
	sql := `SELECT COUNT(*) FROM tickets t
	        LEFT JOIN hosts h ON h.id = t.host_id
	        WHERE ` + where + `
	          AND (SELECT m.is_from_owner FROM ticket_messages m
	               WHERE m.ticket_id = t.id ORDER BY m.created_at DESC LIMIT 1) = false`
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// TicketsGetUserOpenTicketCount mirrors getUserOpenTicketCount().
func TicketsGetUserOpenTicketCount(ctx context.Context, userID int32) (int64, error) {
	var count int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM tickets WHERE user_id = ? AND status IN ('open', 'in_progress')",
		userID,
	).Scan(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// TicketsGetTicketsForAutoClose mirrors getTicketsForAutoClose().
func TicketsGetTicketsForAutoClose(ctx context.Context, timeoutMs int64) ([]TicketsAutoCloseCandidate, error) {
	cutoffTime := time.Now().Add(-time.Duration(timeoutMs) * time.Millisecond)

	var rows []ticketsTicketRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT " + ticketsListSelect + ticketsFrom + ticketsLastMessageJoin +
			" WHERE t.status = 'resolved'",
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	candidates := make([]TicketsAutoCloseCandidate, 0, len(rows))
	for _, row := range rows {
		if row.LastIsFromOwner == nil || !*row.LastIsFromOwner || row.LastCreatedAt == nil {
			continue
		}
		if !row.LastCreatedAt.Before(cutoffTime) {
			continue
		}
		candidates = append(candidates, TicketsAutoCloseCandidate{
			ID:                  row.ID,
			Subject:             row.Subject,
			UserID:              row.UserID,
			HostID:              row.HostID,
			LastPublicMessageAt: *row.LastCreatedAt,
		})
	}
	return candidates, nil
}

// TicketsAutoCloseTickets mirrors autoCloseTickets(): only the tickets this run
// actually closed are returned, so callers never double-notify.
func TicketsAutoCloseTickets(ctx context.Context, ticketIDs []int32, timeoutMs int64) ([]TicketsAutoClosedTicket, error) {
	if len(ticketIDs) == 0 {
		return []TicketsAutoClosedTicket{}, nil
	}

	seen := map[int32]bool{}
	uniqueIDs := make([]int32, 0, len(ticketIDs))
	for _, id := range ticketIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		uniqueIDs = append(uniqueIDs, id)
	}

	cutoffTime := time.Now().Add(-time.Duration(timeoutMs) * time.Millisecond)
	closed := make([]TicketsAutoClosedTicket, 0, len(uniqueIDs))

	for _, ticketID := range uniqueIDs {
		var result *TicketsAutoClosedTicket
		err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var rows []struct {
				ID          int32      `gorm:"column:id"`
				Subject     string     `gorm:"column:subject"`
				UserID      int32      `gorm:"column:user_id"`
				HostID      *int32     `gorm:"column:host_id"`
				Status      string     `gorm:"column:status"`
				UpdatedAt   time.Time  `gorm:"column:updated_at"`
				IsFromOwner *bool      `gorm:"column:is_from_owner"`
				CreatedAt   *time.Time `gorm:"column:created_at"`
			}
			if err := tx.Raw(
				`SELECT t.id, t.subject, t.user_id, t.host_id, t.status::text AS status, t.updated_at,
				        lm.is_from_owner, lm.created_at
				 FROM tickets t
				 LEFT JOIN LATERAL (
				   SELECT is_from_owner, created_at FROM ticket_messages
				   WHERE ticket_id = t.id ORDER BY created_at DESC LIMIT 1
				 ) lm ON TRUE
				 WHERE t.id = ? LIMIT 1`, ticketID,
			).Scan(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				result = nil
				return nil
			}
			latest := rows[0]
			if latest.Status != string(models.TicketStatusResolved) ||
				latest.IsFromOwner == nil || !*latest.IsFromOwner ||
				latest.CreatedAt == nil || !latest.CreatedAt.Before(cutoffTime) {
				result = nil
				return nil
			}

			update := tx.Exec(
				`UPDATE tickets SET status = ?, closed_at = ?, updated_at = ?
				  WHERE id = ? AND status = ? AND updated_at = ?`,
				string(models.TicketStatusClosed), time.Now(), time.Now(),
				latest.ID, string(models.TicketStatusResolved), latest.UpdatedAt,
			)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected == 0 {
				result = nil
				return nil
			}

			result = &TicketsAutoClosedTicket{
				ID:      latest.ID,
				Subject: latest.Subject,
				UserID:  latest.UserID,
				HostID:  latest.HostID,
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if result != nil {
			closed = append(closed, *result)
		}
	}

	return closed, nil
}

// TicketsDeleteTicketMessage mirrors deleteTicketMessage(). Prisma's delete
// throws when the row is gone, so a zero-row delete becomes an error here too.
func TicketsDeleteTicketMessage(ctx context.Context, messageID int32) (bool, error) {
	result := DB.WithContext(ctx).Exec("DELETE FROM ticket_messages WHERE id = ?", messageID)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, fmt.Errorf("ticket message %d not found", messageID)
	}
	return true, nil
}

// TicketsGetTicketMessageAttachments mirrors getTicketMessageAttachments().
func TicketsGetTicketMessageAttachments(ctx context.Context, messageID int32) ([]TicketsMessageAttachmentForCleanup, error) {
	var rows []TicketsMessageAttachmentForCleanup
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, ticket_id, message_id, provider_version, provider_file_id, url
		 FROM ticket_message_attachments WHERE message_id = ?`, messageID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// TicketsGetTicketMessageByIDRef mirrors getTicketMessageById().
func TicketsGetTicketMessageByIDRef(ctx context.Context, messageID int32) (*TicketsMessageRef, error) {
	var rows []TicketsMessageRef
	if err := DB.WithContext(ctx).Raw(
		"SELECT id, ticket_id, sender_id FROM ticket_messages WHERE id = ? LIMIT 1", messageID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// TicketsGetTicketMessageAttachmentByID mirrors getTicketMessageAttachmentById().
func TicketsGetTicketMessageAttachmentByID(ctx context.Context, attachmentID int32) (*TicketsAttachmentRef, error) {
	var rows []TicketsAttachmentRef
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, ticket_id, message_id, mime_type, filename, url
		 FROM ticket_message_attachments WHERE id = ? LIMIT 1`, attachmentID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ==================== 客服工作台上下文 ====================

// TicketsUserContext mirrors userContext in getAdminTicketSuccessContext().
type TicketsUserContext struct {
	ID          int32   `json:"id"`
	Username    string  `json:"username"`
	EmailMasked *string `json:"emailMasked"`
	Role        string  `json:"role"`
	Status      string  `json:"status"`
	Balance     *string `json:"balance"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	Counts      struct {
		Instances       int64 `json:"instances"`
		TicketsCreated  int64 `json:"ticketsCreated"`
		RechargeRecords int64 `json:"rechargeRecords"`
		BalanceLogs     int64 `json:"balanceLogs"`
	} `json:"counts"`
}

// TicketsRecentOrder mirrors one recentOrders entry.
type TicketsRecentOrder struct {
	ID            int32   `json:"id"`
	OrderNo       string  `json:"orderNo"`
	TradeNo       *string `json:"tradeNo"`
	Amount        *string `json:"amount"`
	ActualAmount  *string `json:"actualAmount"`
	Status        string  `json:"status"`
	FailReason    *string `json:"failReason"`
	PaymentMethod *string `json:"paymentMethod"`
	CreatedAt     string  `json:"createdAt"`
	CompletedAt   *string `json:"completedAt"`
}

// TicketsRecentOrderCase mirrors one recentOrderCases entry.
type TicketsRecentOrderCase struct {
	ID           int32   `json:"id"`
	SourceType   string  `json:"sourceType"`
	SourceID     int32   `json:"sourceId"`
	OrderNo      *string `json:"orderNo"`
	Status       string  `json:"status"`
	Reason       string  `json:"reason"`
	Result       *string `json:"result"`
	RefundAmount *string `json:"refundAmount"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
}

// TicketsRecentInstanceHost mirrors the nested host select.
type TicketsRecentInstanceHost struct {
	ID     int32  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// TicketsRecentInstancePackage mirrors the nested package select.
type TicketsRecentInstancePackage struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// TicketsRecentInstance mirrors one recentInstances entry.
type TicketsRecentInstance struct {
	ID        int32                         `json:"id"`
	Name      string                        `json:"name"`
	Status    string                        `json:"status"`
	HostID    int32                         `json:"hostId"`
	CPU       int32                         `json:"cpu"`
	Memory    int32                         `json:"memory"`
	Disk      int32                         `json:"disk"`
	CreatedAt string                        `json:"createdAt"`
	ExpiresAt *string                       `json:"expiresAt"`
	Host      *TicketsRecentInstanceHost    `json:"host"`
	Package   *TicketsRecentInstancePackage `json:"package"`
}

// TicketsTimelineEntry mirrors one timeline item.
type TicketsTimelineEntry struct {
	Type      string `json:"type"`
	ID        int32  `json:"id"`
	Title     string `json:"title"`
	Actor     string `json:"actor"`
	Content   string `json:"content"`
	CreatedAt string `json:"createdAt"`
}

// TicketsQuickActions mirrors quickActions.
type TicketsQuickActions struct {
	NotifyUser            bool    `json:"notifyUser"`
	BalanceAdjustmentPath string  `json:"balanceAdjustmentPath"`
	UserPath              string  `json:"userPath"`
	InstancePath          *string `json:"instancePath"`
	HostPath              *string `json:"hostPath"`
}

// TicketsSupportContext mirrors getAdminTicketSuccessContext()'s response.
type TicketsSupportContext struct {
	Ticket               *TicketsTicketDetail         `json:"ticket"`
	UserContext          *TicketsUserContext          `json:"userContext"`
	RecentOrders         []TicketsRecentOrder         `json:"recentOrders"`
	RecentOrderCases     []TicketsRecentOrderCase     `json:"recentOrderCases"`
	RecentInstances      []TicketsRecentInstance      `json:"recentInstances"`
	Links                []TicketsTicketObjectLink    `json:"links"`
	InternalNotes        []TicketsTicketInternalNote  `json:"internalNotes"`
	Timeline             []TicketsTimelineEntry       `json:"timeline"`
	KnowledgeSuggestions []TicketsKnowledgeSuggestion `json:"knowledgeSuggestions"`
	QuickActions         TicketsQuickActions          `json:"quickActions"`
}

// TicketsGetAdminTicketSuccessContext mirrors getAdminTicketSuccessContext().
func TicketsGetAdminTicketSuccessContext(ctx context.Context, ticketID int32) (*TicketsSupportContext, error) {
	ticket, err := TicketsGetTicketByID(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	if ticket == nil {
		return nil, nil
	}

	// --- user + counts ---
	var userRows []struct {
		ID              int32     `gorm:"column:id"`
		Username        string    `gorm:"column:username"`
		Email           *string   `gorm:"column:email"`
		Role            string    `gorm:"column:role"`
		Status          string    `gorm:"column:status"`
		Balance         string    `gorm:"column:balance"`
		CreatedAt       time.Time `gorm:"column:created_at"`
		UpdatedAt       time.Time `gorm:"column:updated_at"`
		Instances       int64     `gorm:"column:instances"`
		TicketsCreated  int64     `gorm:"column:tickets_created"`
		RechargeRecords int64     `gorm:"column:recharge_records"`
		BalanceLogs     int64     `gorm:"column:balance_logs"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT u.id, u.username, u.email, u.role::text AS role, u.status::text AS status,
		        u.balance::text AS balance, u.created_at, u.updated_at,
		        (SELECT COUNT(*) FROM instances x WHERE x.user_id = u.id) AS instances,
		        (SELECT COUNT(*) FROM tickets x WHERE x.user_id = u.id) AS tickets_created,
		        (SELECT COUNT(*) FROM recharge_records x WHERE x.user_id = u.id) AS recharge_records,
		        (SELECT COUNT(*) FROM balance_logs x WHERE x.user_id = u.id) AS balance_logs
		 FROM users u WHERE u.id = ? LIMIT 1`, ticket.UserID,
	).Scan(&userRows).Error; err != nil {
		return nil, err
	}

	// --- recent recharge records ---
	var orderRows []struct {
		ID            int32      `gorm:"column:id"`
		OrderNo       string     `gorm:"column:order_no"`
		TradeNo       *string    `gorm:"column:trade_no"`
		Amount        string     `gorm:"column:amount"`
		ActualAmount  *string    `gorm:"column:actual_amount"`
		Status        string     `gorm:"column:status"`
		FailReason    *string    `gorm:"column:fail_reason"`
		PaymentMethod *string    `gorm:"column:payment_method"`
		CreatedAt     time.Time  `gorm:"column:created_at"`
		CompletedAt   *time.Time `gorm:"column:completed_at"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, order_no, trade_no, amount::text AS amount, actual_amount::text AS actual_amount,
		        status::text AS status, fail_reason, payment_method, created_at, completed_at
		 FROM recharge_records WHERE user_id = ?
		 ORDER BY created_at DESC LIMIT 5`, ticket.UserID,
	).Scan(&orderRows).Error; err != nil {
		return nil, err
	}

	// --- recent order operation cases ---
	var caseRows []struct {
		ID           int32     `gorm:"column:id"`
		SourceType   string    `gorm:"column:source_type"`
		SourceID     int32     `gorm:"column:source_id"`
		OrderNo      *string   `gorm:"column:order_no"`
		Status       string    `gorm:"column:status"`
		Reason       string    `gorm:"column:reason"`
		Result       *string   `gorm:"column:result"`
		RefundAmount *string   `gorm:"column:refund_amount"`
		CreatedAt    time.Time `gorm:"column:created_at"`
		UpdatedAt    time.Time `gorm:"column:updated_at"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, source_type, source_id, order_no, status::text AS status, reason, result,
		        refund_amount::text AS refund_amount, created_at, updated_at
		 FROM order_operation_cases WHERE user_id = ?
		 ORDER BY created_at DESC LIMIT 5`, ticket.UserID,
	).Scan(&caseRows).Error; err != nil {
		return nil, err
	}

	// --- recent instances ---
	var instanceRows []struct {
		ID          int32      `gorm:"column:id"`
		Name        string     `gorm:"column:name"`
		Status      string     `gorm:"column:status"`
		HostID      int32      `gorm:"column:host_id"`
		CPU         int32      `gorm:"column:cpu"`
		Memory      int32      `gorm:"column:memory"`
		Disk        int32      `gorm:"column:disk"`
		CreatedAt   time.Time  `gorm:"column:created_at"`
		ExpiresAt   *time.Time `gorm:"column:expires_at"`
		HostName    *string    `gorm:"column:host_name"`
		HostStatus  *string    `gorm:"column:host_status"`
		PackageID   *int32     `gorm:"column:package_id"`
		PackageName *string    `gorm:"column:package_name"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT i.id, i.name, i.status::text AS status, i.host_id, i.cpu, i.memory, i.disk,
		        i.created_at, i.expires_at,
		        h.name AS host_name, h.status::text AS host_status,
		        p.id AS package_id, p.name AS package_name
		 FROM instances i
		 LEFT JOIN hosts h ON h.id = i.host_id
		 LEFT JOIN packages p ON p.id = i.package_id
		 WHERE i.user_id = ? AND i.status <> 'deleted'
		 ORDER BY i.created_at DESC LIMIT 8`, ticket.UserID,
	).Scan(&instanceRows).Error; err != nil {
		return nil, err
	}

	// --- links / notes / messages ---
	links, err := TicketsGetTicketObjectLinks(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	notes, err := TicketsGetTicketInternalNotes(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	var messageRows []struct {
		ID          int32     `gorm:"column:id"`
		SenderID    int32     `gorm:"column:sender_id"`
		Content     string    `gorm:"column:content"`
		IsFromOwner bool      `gorm:"column:is_from_owner"`
		CreatedAt   time.Time `gorm:"column:created_at"`
		Username    string    `gorm:"column:username"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT m.id, m.sender_id, m.content, m.is_from_owner, m.created_at, s.username
		 FROM ticket_messages m JOIN users s ON s.id = m.sender_id
		 WHERE m.ticket_id = ? ORDER BY m.created_at ASC`, ticketID,
	).Scan(&messageRows).Error; err != nil {
		return nil, err
	}

	// --- assemble ---
	var userContext *TicketsUserContext
	if len(userRows) > 0 {
		row := userRows[0]
		balance := row.Balance
		userContext = &TicketsUserContext{
			ID:          row.ID,
			Username:    row.Username,
			EmailMasked: TicketsMaskEmail(row.Email),
			Role:        row.Role,
			Status:      row.Status,
			Balance:     &balance,
			CreatedAt:   isoTimestamp(row.CreatedAt),
			UpdatedAt:   isoTimestamp(row.UpdatedAt),
		}
		userContext.Counts.Instances = row.Instances
		userContext.Counts.TicketsCreated = row.TicketsCreated
		userContext.Counts.RechargeRecords = row.RechargeRecords
		userContext.Counts.BalanceLogs = row.BalanceLogs
	}

	recentOrders := make([]TicketsRecentOrder, 0, len(orderRows))
	for _, row := range orderRows {
		amount := row.Amount
		recentOrders = append(recentOrders, TicketsRecentOrder{
			ID:            row.ID,
			OrderNo:       row.OrderNo,
			TradeNo:       ticketsMaskTradeNo(row.TradeNo),
			Amount:        &amount,
			ActualAmount:  row.ActualAmount,
			Status:        row.Status,
			FailReason:    row.FailReason,
			PaymentMethod: row.PaymentMethod,
			CreatedAt:     isoTimestamp(row.CreatedAt),
			CompletedAt:   ticketsISO(row.CompletedAt),
		})
	}

	recentOrderCases := make([]TicketsRecentOrderCase, 0, len(caseRows))
	for _, row := range caseRows {
		recentOrderCases = append(recentOrderCases, TicketsRecentOrderCase{
			ID:           row.ID,
			SourceType:   row.SourceType,
			SourceID:     row.SourceID,
			OrderNo:      row.OrderNo,
			Status:       row.Status,
			Reason:       row.Reason,
			Result:       row.Result,
			RefundAmount: row.RefundAmount,
			CreatedAt:    isoTimestamp(row.CreatedAt),
			UpdatedAt:    isoTimestamp(row.UpdatedAt),
		})
	}

	recentInstances := make([]TicketsRecentInstance, 0, len(instanceRows))
	for _, row := range instanceRows {
		instance := TicketsRecentInstance{
			ID:        row.ID,
			Name:      row.Name,
			Status:    row.Status,
			HostID:    row.HostID,
			CPU:       row.CPU,
			Memory:    row.Memory,
			Disk:      row.Disk,
			CreatedAt: isoTimestamp(row.CreatedAt),
			ExpiresAt: ticketsISO(row.ExpiresAt),
		}
		if row.HostName != nil {
			instance.Host = &TicketsRecentInstanceHost{
				ID:     row.HostID,
				Name:   *row.HostName,
				Status: derefString(row.HostStatus),
			}
		}
		if row.PackageID != nil && row.PackageName != nil {
			instance.Package = &TicketsRecentInstancePackage{ID: *row.PackageID, Name: *row.PackageName}
		}
		recentInstances = append(recentInstances, instance)
	}

	timeline := make([]TicketsTimelineEntry, 0, len(messageRows)+len(notes)+len(links))
	for _, message := range messageRows {
		title := "用户回复"
		if message.IsFromOwner {
			title = "客服回复"
		}
		timeline = append(timeline, TicketsTimelineEntry{
			Type:      "message",
			ID:        message.ID,
			Title:     title,
			Actor:     message.Username,
			Content:   message.Content,
			CreatedAt: isoTimestamp(message.CreatedAt),
		})
	}
	for _, note := range notes {
		timeline = append(timeline, TicketsTimelineEntry{
			Type:      "internal_note",
			ID:        note.ID,
			Title:     "内部备注",
			Actor:     note.ActorUsername,
			Content:   note.Content,
			CreatedAt: note.CreatedAt,
		})
	}
	for _, link := range links {
		content := "#" + strconv.FormatInt(int64(link.ObjectID), 10)
		if link.ObjectLabel != nil {
			content = *link.ObjectLabel
		}
		timeline = append(timeline, TicketsTimelineEntry{
			Type:      "object_link",
			ID:        link.ID,
			Title:     "关联 " + link.ObjectType,
			Actor:     link.CreatedByUsername,
			Content:   content,
			CreatedAt: link.CreatedAt,
		})
	}
	// Array#sort is stable, so equal timestamps keep the message → note → link
	// insertion order above.
	sortSliceStable(timeline, func(a, b TicketsTimelineEntry) bool {
		return ticketsParseISOTime(a.CreatedAt) < ticketsParseISOTime(b.CreatedAt)
	})

	quickActions := TicketsQuickActions{
		NotifyUser:            true,
		BalanceAdjustmentPath: "/admin/billing?userId=" + strconv.FormatInt(int64(ticket.UserID), 10) + "&source=ticket&ticketId=" + strconv.FormatInt(int64(ticket.ID), 10),
		UserPath:              "/admin/users/" + strconv.FormatInt(int64(ticket.UserID), 10),
	}
	if ticket.InstanceID != nil {
		path := "/admin/instances/" + strconv.FormatInt(int64(*ticket.InstanceID), 10)
		quickActions.InstancePath = &path
	}
	if ticket.HostID != nil {
		path := "/admin/resources/hosts/" + strconv.FormatInt(int64(*ticket.HostID), 10)
		quickActions.HostPath = &path
	}

	if links == nil {
		links = []TicketsTicketObjectLink{}
	}
	if notes == nil {
		notes = []TicketsTicketInternalNote{}
	}

	return &TicketsSupportContext{
		Ticket:               ticket,
		UserContext:          userContext,
		RecentOrders:         recentOrders,
		RecentOrderCases:     recentOrderCases,
		RecentInstances:      recentInstances,
		Links:                links,
		InternalNotes:        notes,
		Timeline:             timeline,
		KnowledgeSuggestions: ticketsKnowledgeSuggestions(ticket.Category),
		QuickActions:         quickActions,
	}, nil
}

// ticketsMaskTradeNo mirrors
// `tradeNo ? tradeNo.slice(0,4) + '***' + tradeNo.slice(-4) : null`.
func ticketsMaskTradeNo(tradeNo *string) *string {
	if tradeNo == nil || *tradeNo == "" {
		return nil
	}
	value := *tradeNo
	head := ticketsSliceUTF16(value, 4)
	tail := ticketsSliceTailUTF16(value, 4)
	masked := head + "***" + tail
	return &masked
}

// ticketsSliceTailUTF16 mirrors `value.slice(-n)`.
func ticketsSliceTailUTF16(value string, units int) string {
	encoded := utf16.Encode([]rune(value))
	if len(encoded) <= units {
		return value
	}
	return string(utf16.Decode(encoded[len(encoded)-units:]))
}

// ==================== 内部备注 / 关联对象 ====================

// TicketsCreateTicketInternalNote mirrors createTicketInternalNote().
func TicketsCreateTicketInternalNote(ctx context.Context, ticketID, actorUserID int32, actorUsername, content string) (TicketsTicketInternalNote, error) {
	var row struct {
		ID            int32     `gorm:"column:id"`
		TicketID      int32     `gorm:"column:ticket_id"`
		ActorUserID   int32     `gorm:"column:actor_user_id"`
		ActorUsername string    `gorm:"column:actor_username"`
		Content       string    `gorm:"column:content"`
		CreatedAt     time.Time `gorm:"column:created_at"`
	}
	if err := DB.WithContext(ctx).Raw(
		`INSERT INTO ticket_internal_notes (ticket_id, actor_user_id, actor_username, content, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 RETURNING id, ticket_id, actor_user_id, actor_username, content, created_at`,
		ticketID, actorUserID, actorUsername, content, time.Now(),
	).Scan(&row).Error; err != nil {
		return TicketsTicketInternalNote{}, err
	}

	return TicketsTicketInternalNote{
		ID:            row.ID,
		TicketID:      row.TicketID,
		ActorUserID:   row.ActorUserID,
		ActorUsername: row.ActorUsername,
		Content:       row.Content,
		CreatedAt:     isoTimestamp(row.CreatedAt),
	}, nil
}

// TicketsGetTicketInternalNotes mirrors the internalNotes query (createdAt asc).
func TicketsGetTicketInternalNotes(ctx context.Context, ticketID int32) ([]TicketsTicketInternalNote, error) {
	var rows []struct {
		ID            int32     `gorm:"column:id"`
		TicketID      int32     `gorm:"column:ticket_id"`
		ActorUserID   int32     `gorm:"column:actor_user_id"`
		ActorUsername string    `gorm:"column:actor_username"`
		Content       string    `gorm:"column:content"`
		CreatedAt     time.Time `gorm:"column:created_at"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, ticket_id, actor_user_id, actor_username, content, created_at
		 FROM ticket_internal_notes WHERE ticket_id = ? ORDER BY created_at ASC`, ticketID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	notes := make([]TicketsTicketInternalNote, 0, len(rows))
	for _, row := range rows {
		notes = append(notes, TicketsTicketInternalNote{
			ID:            row.ID,
			TicketID:      row.TicketID,
			ActorUserID:   row.ActorUserID,
			ActorUsername: row.ActorUsername,
			Content:       row.Content,
			CreatedAt:     isoTimestamp(row.CreatedAt),
		})
	}
	return notes, nil
}

// TicketsGetTicketObjectLinks mirrors the links query (createdAt desc).
func TicketsGetTicketObjectLinks(ctx context.Context, ticketID int32) ([]TicketsTicketObjectLink, error) {
	var rows []struct {
		ID                int32     `gorm:"column:id"`
		TicketID          int32     `gorm:"column:ticket_id"`
		ObjectType        string    `gorm:"column:object_type"`
		ObjectID          int32     `gorm:"column:object_id"`
		ObjectLabel       *string   `gorm:"column:object_label"`
		CreatedByUserID   int32     `gorm:"column:created_by_user_id"`
		CreatedByUsername string    `gorm:"column:created_by_username"`
		CreatedAt         time.Time `gorm:"column:created_at"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, ticket_id, object_type::text AS object_type, object_id, object_label,
		        created_by_user_id, created_by_username, created_at
		 FROM ticket_object_links WHERE ticket_id = ? ORDER BY created_at DESC`, ticketID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	links := make([]TicketsTicketObjectLink, 0, len(rows))
	for _, row := range rows {
		links = append(links, TicketsTicketObjectLink{
			ID:                row.ID,
			TicketID:          row.TicketID,
			ObjectType:        row.ObjectType,
			ObjectID:          row.ObjectID,
			ObjectLabel:       row.ObjectLabel,
			CreatedByUserID:   row.CreatedByUserID,
			CreatedByUsername: row.CreatedByUsername,
			CreatedAt:         isoTimestamp(row.CreatedAt),
		})
	}
	return links, nil
}

// ticketsValidateObjectLink mirrors validateTicketObjectLink().
func ticketsValidateObjectLink(ctx context.Context, ticket struct {
	UserID     int32
	HostID     *int32
	InstanceID *int32
}, objectType string, objectID int32) (*string, error) {
	switch objectType {
	case string(models.TicketObjectLinkTypeRechargeRecord):
		var rows []struct {
			OrderNo string `gorm:"column:order_no"`
			Status  string `gorm:"column:status"`
			Amount  string `gorm:"column:amount"`
		}
		if err := DB.WithContext(ctx).Raw(
			`SELECT order_no, status::text AS status, amount::text AS amount
			 FROM recharge_records WHERE id = ? AND user_id = ? LIMIT 1`, objectID, ticket.UserID,
		).Scan(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, nil
		}
		label := "充值 " + rows[0].OrderNo + " · " + rows[0].Status + " · ¥" + rows[0].Amount
		return &label, nil

	case string(models.TicketObjectLinkTypeOrderOperationCase):
		var rows []struct {
			OrderNo *string `gorm:"column:order_no"`
			Status  string  `gorm:"column:status"`
		}
		if err := DB.WithContext(ctx).Raw(
			`SELECT order_no, status::text AS status
			 FROM order_operation_cases WHERE id = ? AND user_id = ? LIMIT 1`, objectID, ticket.UserID,
		).Scan(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, nil
		}
		orderNo := "#" + strconv.FormatInt(int64(objectID), 10)
		if rows[0].OrderNo != nil && *rows[0].OrderNo != "" {
			orderNo = *rows[0].OrderNo
		}
		label := "订单处理 " + orderNo + " · " + rows[0].Status
		return &label, nil

	case string(models.TicketObjectLinkTypeInstance):
		var rows []struct {
			Name   string `gorm:"column:name"`
			Status string `gorm:"column:status"`
		}
		if err := DB.WithContext(ctx).Raw(
			`SELECT name, status::text AS status
			 FROM instances WHERE id = ? AND user_id = ? LIMIT 1`, objectID, ticket.UserID,
		).Scan(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, nil
		}
		label := "实例 " + rows[0].Name + " · " + rows[0].Status
		return &label, nil

	case string(models.TicketObjectLinkTypeHost):
		// OR: [ticket.hostId, {instances: {some: {userId}}}]
		conditions := []string{"h.id = ?"}
		args := []any{objectID}
		access := []string{"EXISTS (SELECT 1 FROM instances x WHERE x.host_id = h.id AND x.user_id = ?)"}
		accessArgs := []any{ticket.UserID}
		if ticket.HostID != nil {
			access = append([]string{"h.id = ?"}, access...)
			accessArgs = append([]any{*ticket.HostID}, accessArgs...)
		}
		conditions = append(conditions, "("+strings.Join(access, " OR ")+")")
		args = append(args, accessArgs...)

		var rows []struct {
			Name   string `gorm:"column:name"`
			Status string `gorm:"column:status"`
		}
		if err := DB.WithContext(ctx).Raw(
			`SELECT h.name, h.status::text AS status FROM hosts h WHERE `+
				strings.Join(conditions, " AND ")+` LIMIT 1`, args...,
		).Scan(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, nil
		}
		label := "节点 " + rows[0].Name + " · " + rows[0].Status
		return &label, nil

	default:
		return nil, nil
	}
}

// TicketsCreateTicketObjectLink mirrors createTicketObjectLink().
func TicketsCreateTicketObjectLink(ctx context.Context, ticketID int32, objectType string, objectID int32, createdByUserID int32, createdByUsername string) (*TicketsTicketObjectLink, error) {
	var tickets []struct {
		UserID     int32  `gorm:"column:user_id"`
		HostID     *int32 `gorm:"column:host_id"`
		InstanceID *int32 `gorm:"column:instance_id"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT user_id, host_id, instance_id FROM tickets WHERE id = ? LIMIT 1", ticketID,
	).Scan(&tickets).Error; err != nil {
		return nil, err
	}
	if len(tickets) == 0 {
		return nil, nil
	}

	objectLabel, err := ticketsValidateObjectLink(ctx, struct {
		UserID     int32
		HostID     *int32
		InstanceID *int32
	}{UserID: tickets[0].UserID, HostID: tickets[0].HostID, InstanceID: tickets[0].InstanceID}, objectType, objectID)
	if err != nil {
		return nil, err
	}
	if objectLabel == nil {
		return nil, nil
	}

	var row struct {
		ID                int32     `gorm:"column:id"`
		TicketID          int32     `gorm:"column:ticket_id"`
		ObjectType        string    `gorm:"column:object_type"`
		ObjectID          int32     `gorm:"column:object_id"`
		ObjectLabel       *string   `gorm:"column:object_label"`
		CreatedByUserID   int32     `gorm:"column:created_by_user_id"`
		CreatedByUsername string    `gorm:"column:created_by_username"`
		CreatedAt         time.Time `gorm:"column:created_at"`
	}
	if err := DB.WithContext(ctx).Raw(
		`INSERT INTO ticket_object_links
			(ticket_id, object_type, object_id, object_label, created_by_user_id, created_by_username, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (ticket_id, object_type, object_id)
		 DO UPDATE SET object_label = EXCLUDED.object_label
		 RETURNING id, ticket_id, object_type::text AS object_type, object_id, object_label,
		           created_by_user_id, created_by_username, created_at`,
		ticketID, objectType, objectID, *objectLabel, createdByUserID, createdByUsername, time.Now(),
	).Scan(&row).Error; err != nil {
		return nil, err
	}

	return &TicketsTicketObjectLink{
		ID:                row.ID,
		TicketID:          row.TicketID,
		ObjectType:        row.ObjectType,
		ObjectID:          row.ObjectID,
		ObjectLabel:       row.ObjectLabel,
		CreatedByUserID:   row.CreatedByUserID,
		CreatedByUsername: row.CreatedByUsername,
		CreatedAt:         isoTimestamp(row.CreatedAt),
	}, nil
}

// ==================== small helpers ====================

func nilableString(value string) *string {
	result := value
	return &result
}

// sortSliceStable is a thin wrapper so the comparator style stays close to the
// TypeScript source.
func sortSliceStable[T any](items []T, less func(a, b T) bool) {
	if len(items) < 2 {
		return
	}
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
