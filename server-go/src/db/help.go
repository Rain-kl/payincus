// Port of server/src/db/help.ts.
//
// The TypeScript layer returns plain objects whose timestamps are already ISO
// strings (`article.createdAt.toISOString()`), and whose booleans are exposed as
// 0/1 numbers (the HelpArticle type in types/database.ts). The Go structs below
// mirror that wire shape exactly; no time.Time leaks out of this package.
package db

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// helpCategoryMaxLength / helpSearchMaxLength mirror the slice(0, N) bounds in
// normalizeHelpCategory / normalizeHelpSearch.
const (
	helpCategoryMaxLength = 80
	helpSearchMaxLength   = 128
)

// HelpArticleListItem is the list projection (content is never selected).
type HelpArticleListItem struct {
	ID        int32  `json:"id"`
	Title     string `json:"title"`
	Slug      string `json:"slug"`
	Category  string `json:"category"`
	SortOrder int32  `json:"sort_order"`
	Published int32  `json:"published"`
	Pinned    int32  `json:"pinned"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// HelpArticle mirrors the HelpArticle type from types/database.ts.
type HelpArticle struct {
	ID        int32  `json:"id"`
	Title     string `json:"title"`
	Slug      string `json:"slug"`
	Content   string `json:"content"`
	Category  string `json:"category"`
	SortOrder int32  `json:"sort_order"`
	Published int32  `json:"published"`
	Pinned    int32  `json:"pinned"`
	CreatedBy *int32 `json:"created_by"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// HelpPinnedArticle is the pinned-article projection used by the public home page.
type HelpPinnedArticle struct {
	ID       int32  `json:"id"`
	Title    string `json:"title"`
	Slug     string `json:"slug"`
	Category string `json:"category"`
}

// HelpCategoryCount mirrors the getHelpCategories row shape.
type HelpCategoryCount struct {
	Category string `json:"category"`
	Count    int64  `json:"count"`
}

// HelpArticleListOptions mirrors the options object of getHelpArticles.
type HelpArticleListOptions struct {
	Page int
	// PageSize zero means "not provided".
	PageSize int
	// PublishedOnly is a pointer because TypeScript defaults it to true
	// (`options.publishedOnly ?? true`). A plain bool cannot distinguish
	// "not provided" (published only) from an explicit false (include drafts).
	PublishedOnly *bool
	Category      *string
	Search        *string
	PinnedOnly    bool
}

// helpDefaultPublishedOnly mirrors `options.publishedOnly ?? true`.
func helpDefaultPublishedOnly(value *bool) bool {
	if value == nil {
		return true
	}
	return *value
}

// HelpArticleListResult mirrors the getHelpArticles return shape.
type HelpArticleListResult struct {
	Items      []HelpArticleListItem `json:"items"`
	Total      int64                 `json:"total"`
	Page       int                   `json:"page"`
	PageSize   int                   `json:"pageSize"`
	TotalPages int64                 `json:"totalPages"`
}

// HelpArticleCreateData mirrors createHelpArticle's data argument. A nil pointer
// means the key was absent (the Prisma defaults apply).
type HelpArticleCreateData struct {
	Title     string
	Slug      string
	Content   string
	Category  *string
	SortOrder *int32
	Published *bool
	Pinned    *bool
	CreatedBy *int32
}

// HelpArticleUpdateData mirrors updateHelpArticle's data argument: nil means
// "not provided" and the column is left untouched.
type HelpArticleUpdateData struct {
	Title     *string
	Slug      *string
	Content   *string
	Category  *string
	SortOrder *int32
	Published *bool
	Pinned    *bool
}

type helpArticleRow struct {
	ID        int32     `gorm:"column:id"`
	Title     string    `gorm:"column:title"`
	Slug      string    `gorm:"column:slug"`
	Content   string    `gorm:"column:content"`
	Category  string    `gorm:"column:category"`
	SortOrder int32     `gorm:"column:sort_order"`
	Published bool      `gorm:"column:published"`
	Pinned    bool      `gorm:"column:pinned"`
	CreatedBy *int32    `gorm:"column:created_by"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// ==================== helpers ====================

// clampHelpPagination mirrors clampHelpPagination: a non-positive or absent page
// becomes 1 and pageSize is clamped to [1, 100] (default 20).
func clampHelpPagination(page, pageSize int) (int, int) {
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

// helpNormalizeFilter mirrors `value?.trim()` followed by `slice(0, max)`; an
// empty result is null. The slice is UTF-16 aware, matching String#slice.
func helpNormalizeFilter(value *string, maxLength int) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	trimmed = helpSliceJS(trimmed, maxLength)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// helpSliceJS mirrors String#slice(0, end) over UTF-16 code units.
func helpSliceJS(value string, end int) string {
	units := 0
	for index, runeValue := range value {
		width := 1
		if runeValue > 0xFFFF {
			width = 2
		}
		if units+width > end {
			return value[:index]
		}
		units += width
	}
	return value
}

// helpISOTime mirrors Date#toISOString (UTC, millisecond precision, trailing Z).
func helpISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func helpBoolToInt(value bool) int32 {
	if value {
		return 1
	}
	return 0
}

func helpListItem(row helpArticleRow) HelpArticleListItem {
	return HelpArticleListItem{
		ID:        row.ID,
		Title:     row.Title,
		Slug:      row.Slug,
		Category:  row.Category,
		SortOrder: row.SortOrder,
		Published: helpBoolToInt(row.Published),
		Pinned:    helpBoolToInt(row.Pinned),
		CreatedAt: helpISOTime(row.CreatedAt),
		UpdatedAt: helpISOTime(row.UpdatedAt),
	}
}

func helpArticle(row helpArticleRow) HelpArticle {
	return HelpArticle{
		ID:        row.ID,
		Title:     row.Title,
		Slug:      row.Slug,
		Content:   row.Content,
		Category:  row.Category,
		SortOrder: row.SortOrder,
		Published: helpBoolToInt(row.Published),
		Pinned:    helpBoolToInt(row.Pinned),
		CreatedBy: row.CreatedBy,
		CreatedAt: helpISOTime(row.CreatedAt),
		UpdatedAt: helpISOTime(row.UpdatedAt),
	}
}

// ==================== read ====================

// GetHelpArticles mirrors getHelpArticles: published-only by default, optional
// category filter, case-insensitive search over title/content/category, and
// pinned articles first (unless pinnedOnly is set).
func GetHelpArticles(ctx context.Context, options HelpArticleListOptions) (HelpArticleListResult, error) {
	page, pageSize := clampHelpPagination(options.Page, options.PageSize)
	category := helpNormalizeFilter(options.Category, helpCategoryMaxLength)
	search := helpNormalizeFilter(options.Search, helpSearchMaxLength)

	conditions := make([]string, 0, 5)
	args := make([]any, 0, 8)

	if helpDefaultPublishedOnly(options.PublishedOnly) {
		conditions = append(conditions, "published = true")
	}
	if category != nil {
		conditions = append(conditions, "category = ?")
		args = append(args, *category)
	}
	if search != nil {
		pattern := "%" + *search + "%"
		conditions = append(conditions, "(title ILIKE ? OR content ILIKE ? OR category ILIKE ?)")
		args = append(args, pattern, pattern, pattern)
	}
	if options.PinnedOnly {
		conditions = append(conditions, "pinned = true")
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM help_articles"+where, args...,
	).Scan(&total).Error; err != nil {
		return HelpArticleListResult{}, err
	}

	orderBy := " ORDER BY pinned DESC, sort_order ASC, created_at DESC"
	if options.PinnedOnly {
		orderBy = " ORDER BY sort_order ASC, created_at DESC"
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	var rows []helpArticleRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT id, title, slug, category, sort_order, published, pinned, created_at, updated_at"+
			" FROM help_articles"+where+orderBy+" LIMIT ? OFFSET ?", listArgs...,
	).Scan(&rows).Error; err != nil {
		return HelpArticleListResult{}, err
	}

	items := make([]HelpArticleListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, helpListItem(row))
	}

	return HelpArticleListResult{
		Items:      items,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages(total, pageSize),
	}, nil
}

// GetPinnedHelpArticles mirrors getPinnedHelpArticles (limit clamped to [1,20],
// default 6).
func GetPinnedHelpArticles(ctx context.Context, limit int) ([]HelpPinnedArticle, error) {
	safeLimit := 6
	if limit > 0 {
		safeLimit = limit
		if safeLimit > 20 {
			safeLimit = 20
		}
	}

	var rows []helpArticleRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT id, title, slug, category, sort_order, created_at FROM help_articles"+
			" WHERE published = true AND pinned = true"+
			" ORDER BY sort_order ASC, created_at DESC LIMIT ?", safeLimit,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	articles := make([]HelpPinnedArticle, 0, len(rows))
	for _, row := range rows {
		articles = append(articles, HelpPinnedArticle{
			ID:       row.ID,
			Title:    row.Title,
			Slug:     row.Slug,
			Category: row.Category,
		})
	}
	return articles, nil
}

// GetHelpArticleByID mirrors getHelpArticleById (includes unpublished rows).
func GetHelpArticleByID(ctx context.Context, id int32) (*HelpArticle, error) {
	article, found, err := findHelpArticle(ctx, "id = ?", id)
	if err != nil || !found {
		return nil, err
	}
	return article, nil
}

// GetHelpArticleBySlug mirrors getHelpArticleBySlug (published only).
func GetHelpArticleBySlug(ctx context.Context, slug string) (*HelpArticle, error) {
	article, found, err := findHelpArticle(ctx, "slug = ? AND published = true", slug)
	if err != nil || !found {
		return nil, err
	}
	return article, nil
}

// GetAnyHelpArticleBySlug mirrors getAnyHelpArticleBySlug (uniqueness check, so
// unpublished rows count too).
func GetAnyHelpArticleBySlug(ctx context.Context, slug string) (*HelpArticle, error) {
	article, found, err := findHelpArticle(ctx, "slug = ?", slug)
	if err != nil || !found {
		return nil, err
	}
	return article, nil
}

func findHelpArticle(ctx context.Context, where string, arg any) (*HelpArticle, bool, error) {
	var rows []helpArticleRow
	if err := DB.WithContext(ctx).Raw(
		"SELECT id, title, slug, content, category, sort_order, published, pinned, created_by,"+
			" created_at, updated_at FROM help_articles WHERE "+where+" LIMIT 1", arg,
	).Scan(&rows).Error; err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	article := helpArticle(rows[0])
	return &article, true, nil
}

// GetHelpCategories mirrors getHelpCategories: published articles grouped by
// category, ordered by category ascending.
func GetHelpCategories(ctx context.Context) ([]HelpCategoryCount, error) {
	var rows []struct {
		Category string `gorm:"column:category"`
		Count    int64  `gorm:"column:count"`
	}
	if err := DB.WithContext(ctx).Raw(
		"SELECT category, COUNT(id) AS count FROM help_articles WHERE published = true" +
			" GROUP BY category ORDER BY category ASC",
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	categories := make([]HelpCategoryCount, 0, len(rows))
	for _, row := range rows {
		categories = append(categories, HelpCategoryCount{Category: row.Category, Count: row.Count})
	}
	return categories, nil
}

// ==================== write ====================

// CreateHelpArticle mirrors createHelpArticle and returns the new row id.
func CreateHelpArticle(ctx context.Context, data HelpArticleCreateData) (int32, error) {
	category := "general"
	if data.Category != nil {
		category = *data.Category
	}
	sortOrder := int32(0)
	if data.SortOrder != nil {
		sortOrder = *data.SortOrder
	}
	published := true
	if data.Published != nil {
		published = *data.Published
	}
	pinned := false
	if data.Pinned != nil {
		pinned = *data.Pinned
	}

	now := time.Now().UTC()
	var id int32
	if err := DB.WithContext(ctx).Raw(
		"INSERT INTO help_articles (title, slug, content, category, sort_order, published, pinned,"+
			" created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		data.Title, data.Slug, data.Content, category, sortOrder, published, pinned,
		data.CreatedBy, now, now,
	).Scan(&id).Error; err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateHelpArticle mirrors updateHelpArticle: only the provided columns are
// written and @updatedAt is always bumped. A missing row is an error (P2025).
func UpdateHelpArticle(ctx context.Context, id int32, data HelpArticleUpdateData) error {
	updates := map[string]any{}
	if data.Title != nil {
		updates["title"] = *data.Title
	}
	if data.Slug != nil {
		updates["slug"] = *data.Slug
	}
	if data.Content != nil {
		updates["content"] = *data.Content
	}
	if data.Category != nil {
		updates["category"] = *data.Category
	}
	if data.SortOrder != nil {
		updates["sort_order"] = *data.SortOrder
	}
	if data.Published != nil {
		updates["published"] = *data.Published
	}
	if data.Pinned != nil {
		updates["pinned"] = *data.Pinned
	}

	// TypeScript returns early when no field is present, so it neither bumps
	// updated_at nor errors on a missing row. Mirror that exactly.
	if len(updates) == 0 {
		return nil
	}
	updates["updated_at"] = time.Now().UTC()

	result := DB.WithContext(ctx).Model(&models.HelpArticle{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// DeleteHelpArticle mirrors deleteHelpArticle. A missing row is an error (P2025).
func DeleteHelpArticle(ctx context.Context, id int32) error {
	result := DB.WithContext(ctx).Exec("DELETE FROM help_articles WHERE id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// HelpCategoryConfigKey is the system_configs key holding the help category list.
const HelpCategoryConfigKey = "help_categories"

// HelpCategoryConfigExists mirrors `prisma.systemConfig.findUnique({ where: { key } })`
// as a boolean: it reports row existence, not whether the value is non-empty.
func HelpCategoryConfigExists(ctx context.Context, key string) (bool, error) {
	var count int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM system_configs WHERE key = ?", key,
	).Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// CreateHelpCategoryConfig mirrors `prisma.systemConfig.create` for the
// help_categories row (type "json" plus the human-readable label/description).
func CreateHelpCategoryConfig(ctx context.Context, key, value string) error {
	now := time.Now().UTC()
	return DB.WithContext(ctx).Exec(
		"INSERT INTO system_configs (key, value, type, label, description, created_at, updated_at)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?)",
		key, value, "json", "帮助中心分类配置", "帮助文档的分类配置（ID、名称、颜色）", now, now,
	).Error
}
