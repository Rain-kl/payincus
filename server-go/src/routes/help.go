// Port of server/src/routes/help.ts.
//
// Public routes (list / pinned / category-config / categories / article by slug)
// fall back to a built-in article set when the database has no published rows, so
// a fresh install still renders a help centre. The admin routes are CRUD over
// help_articles plus the `help_categories` system config.
//
// Fastify runs the JSON-schema validation (AJV with allErrors:false,
// coerceTypes:'array', removeAdditional only where declared) *before* the handler,
// so body validation errors (400 VALIDATION_ERROR, first message only) precede the
// route-id checks. validateHelpAJV reproduces that ordering and the message
// mapping done by setErrorHandler in app.ts.
package routes

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// Schema bounds, mirroring the constants in the route module.
const (
	maxHelpTitleLength          = 200
	maxHelpSlugLength           = 120
	maxHelpCategoryLength       = 80
	maxHelpContentLength        = 100000
	maxHelpSortOrder            = 1000000
	maxHelpCategoryConfigItems  = 50
	maxHelpCategoryNameLength   = 80
	defaultHelpArticleTimestamp = "2026-07-01T00:00:00.000Z"
)

var (
	helpSlugPattern  = regexp.MustCompile(`^[a-z0-9-]+$`)
	helpColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// helpCategoryConfigItem is one { id, name, color } entry of the category config.
type helpCategoryConfigItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// helpDefaultCategories mirrors DEFAULT_CATEGORIES.
var helpDefaultCategories = []helpCategoryConfigItem{
	{ID: "general", Name: "常规", Color: "#6b7280"},
	{ID: "getting-started", Name: "快速开始", Color: "#22c55e"},
	{ID: "instances", Name: "实例管理", Color: "#3b82f6"},
	{ID: "networking", Name: "网络配置", Color: "#8b5cf6"},
	{ID: "billing", Name: "计费相关", Color: "#f59e0b"},
	{ID: "faq", Name: "常见问题", Color: "#ef4444"},
}

// helpDefaultArticle mirrors one entry of DEFAULT_HELP_ARTICLES.
type helpDefaultArticle struct {
	ID        int32
	Title     string
	Slug      string
	Content   string
	Category  string
	SortOrder int32
	Published int32
	Pinned    int32
	CreatedAt string
	UpdatedAt string
}

// helpDefaultArticles mirrors DEFAULT_HELP_ARTICLES.
var helpDefaultArticles = []helpDefaultArticle{
	{
		ID:    0,
		Title: "快速开始使用 Incudal",
		Slug:  "getting-started",
		Content: strings.Join([]string{
			"# 快速开始使用 Incudal",
			"",
			"欢迎使用 Incudal。你可以先从套餐列表选择合适的地区、套餐和方案，再进入创建实例流程。",
			"",
			"## 创建实例",
			"",
			"1. 打开套餐列表或控制台里的创建实例页面。",
			"2. 选择地区、套餐、方案、宿主机、系统镜像和 SSH 密钥。",
			"3. 提交后在实例列表查看交付和运行状态。",
			"",
			"## 账户与工单",
			"",
			"如遇到支付、实例交付、网络或资源问题，请在工单中心提交问题并附上实例编号或订单编号。",
		}, "\n"),
		Category:  "getting-started",
		SortOrder: 0,
		Published: 1,
		Pinned:    1,
		CreatedAt: defaultHelpArticleTimestamp,
		UpdatedAt: defaultHelpArticleTimestamp,
	},
}

// ==================== response shapes ====================

// helpPublicArticleSummary mirrors serializePublicHelpArticleSummary: both the
// snake_case and camelCase timestamps are emitted.
type helpPublicArticleSummary struct {
	ID             int32  `json:"id"`
	Title          string `json:"title"`
	Slug           string `json:"slug"`
	Category       string `json:"category"`
	SortOrder      int32  `json:"sort_order"`
	Published      int32  `json:"published"`
	Pinned         int32  `json:"pinned"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	CreatedAtCamel string `json:"createdAt"`
	UpdatedAtCamel string `json:"updatedAt"`
}

// helpPublicArticleDetail mirrors serializePublicHelpArticleDetail.
type helpPublicArticleDetail struct {
	helpPublicArticleSummary
	Content string `json:"content"`
}

// helpAdminArticleListItem is the admin list row.
type helpAdminArticleListItem struct {
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

// helpAdminArticleDetail is the admin detail row.
type helpAdminArticleDetail struct {
	ID        int32  `json:"id"`
	Title     string `json:"title"`
	Slug      string `json:"slug"`
	Content   string `json:"content"`
	Category  string `json:"category"`
	SortOrder int32  `json:"sort_order"`
	Published int32  `json:"published"`
	Pinned    int32  `json:"pinned"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func helpPublicSummaryFromDefault(article helpDefaultArticle) helpPublicArticleSummary {
	return helpPublicArticleSummary{
		ID:             article.ID,
		Title:          article.Title,
		Slug:           article.Slug,
		Category:       article.Category,
		SortOrder:      article.SortOrder,
		Published:      article.Published,
		Pinned:         article.Pinned,
		CreatedAt:      article.CreatedAt,
		UpdatedAt:      article.UpdatedAt,
		CreatedAtCamel: article.CreatedAt,
		UpdatedAtCamel: article.UpdatedAt,
	}
}

func helpPublicSummaryFromRow(article db.HelpArticleListItem) helpPublicArticleSummary {
	return helpPublicArticleSummary{
		ID:             article.ID,
		Title:          article.Title,
		Slug:           article.Slug,
		Category:       article.Category,
		SortOrder:      article.SortOrder,
		Published:      article.Published,
		Pinned:         article.Pinned,
		CreatedAt:      article.CreatedAt,
		UpdatedAt:      article.UpdatedAt,
		CreatedAtCamel: article.CreatedAt,
		UpdatedAtCamel: article.UpdatedAt,
	}
}

// ==================== normalization helpers ====================

// helpNormalizeRequiredText mirrors normalizeRequiredText: a non-string, an empty
// (after trim) or an over-long value yields false.
func helpNormalizeRequiredText(value any, maxLength int) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || utf16Length(trimmed) > maxLength {
		return "", false
	}
	return trimmed, true
}

// helpNormalizeOptionalText mirrors normalizeOptionalText: an absent key is
// "not provided" (nil, true); an invalid value is "null" (nil, false).
func helpNormalizeOptionalText(value any, present bool, maxLength int) (*string, bool) {
	if !present {
		return nil, true
	}
	text, ok := helpNormalizeRequiredText(value, maxLength)
	if !ok {
		return nil, false
	}
	return &text, true
}

// helpNormalizeSlug mirrors normalizeSlug.
func helpNormalizeSlug(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	slug := strings.TrimSpace(text)
	if slug == "" || utf16Length(slug) > maxHelpSlugLength || !helpSlugPattern.MatchString(slug) {
		return "", false
	}
	return slug, true
}

// helpNormalizeOptionalSlug mirrors normalizeOptionalSlug.
func helpNormalizeOptionalSlug(value any, present bool) (*string, bool) {
	if !present {
		return nil, true
	}
	slug, ok := helpNormalizeSlug(value)
	if !ok {
		return nil, false
	}
	return &slug, true
}

// helpNormalizeCategoryID mirrors normalizeCategoryId.
func helpNormalizeCategoryID(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	id := strings.TrimSpace(text)
	if id == "" || utf16Length(id) > maxHelpCategoryLength || !helpSlugPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

// helpNormalizeSortOrder mirrors normalizeSortOrder: absent is 0, an out-of-range
// or non-integer value is NaN (ok=false).
func helpNormalizeSortOrder(value any, present bool) (int32, bool) {
	if !present {
		return 0, true
	}
	number, ok := value.(float64)
	if !ok || !ajvIsInteger(number) || number < 0 || number > maxHelpSortOrder {
		return 0, false
	}
	return int32(number), true
}

// helpNormalizeOptionalBoolean mirrors normalizeOptionalBoolean: absent is
// "not provided", a non-boolean is "null".
func helpNormalizeOptionalBoolean(value any, present bool) (*bool, bool) {
	if !present {
		return nil, true
	}
	flag, ok := value.(bool)
	if !ok {
		return nil, false
	}
	return &flag, true
}

// helpNormalizeCategoryConfigInput mirrors normalizeHelpCategoryConfigInput.
func helpNormalizeCategoryConfigInput(body any) ([]helpCategoryConfigItem, bool) {
	record, ok := body.(map[string]any)
	if !ok {
		return nil, false
	}
	rawItems, ok := record["categories"].([]any)
	if !ok || len(rawItems) > maxHelpCategoryConfigItems {
		return nil, false
	}

	categories := make([]helpCategoryConfigItem, 0, len(rawItems))
	seen := map[string]bool{}
	for _, raw := range rawItems {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		id, idOK := helpNormalizeCategoryID(item["id"])
		name, nameOK := helpNormalizeRequiredText(item["name"], maxHelpCategoryNameLength)
		color := ""
		if text, ok := item["color"].(string); ok {
			color = strings.TrimSpace(text)
		}
		if !idOK || !nameOK || !helpColorPattern.MatchString(color) || seen[id] {
			return nil, false
		}
		seen[id] = true
		categories = append(categories, helpCategoryConfigItem{ID: id, Name: name, Color: color})
	}
	return categories, true
}

// helpJSONString mirrors JSON.stringify for the stored category config.
func helpJSONString(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

// ==================== AJV replica ====================

// helpAJVKind is the declared type of a schema property.
type helpAJVKind int

const (
	helpAJVString helpAJVKind = iota
	helpAJVInteger
	helpAJVBoolean
	helpAJVObject
	helpAJVArray
)

// helpAJVSpec is the subset of JSON Schema these routes use.
type helpAJVSpec struct {
	Name       string
	Kind       helpAJVKind
	Required   []string
	Properties []helpAJVSpec
	Items      *helpAJVSpec
	MinLength  int // 0 = unset
	MaxLength  int // 0 = unset
	MaxItems   int // 0 = unset
	HasMinimum bool
	Minimum    float64
	HasMaximum bool
	Maximum    float64
	Pattern    *regexp.Regexp
}

// helpAJVError carries the pieces setErrorHandler needs to render a message.
type helpAJVError struct {
	Keyword string
	Message string
	Path    string
	Missing string
}

// helpRenderAJVError mirrors the keyword branches of setErrorHandler.
func helpRenderAJVError(err *helpAJVError) string {
	switch err.Keyword {
	case "required":
		if field := helpAJVFieldName(err.Path); field != "field" {
			return field + " is required"
		}
		return err.Missing + " is required"
	case "minLength":
		return helpAJVFieldName(err.Path) + " is too short"
	case "maxLength":
		return helpAJVFieldName(err.Path) + " is too long"
	case "pattern":
		return helpAJVFieldName(err.Path) + " format is invalid"
	}
	return err.Message
}

// helpAJVFieldName mirrors `err.instancePath?.replace('/', ”) || ... || 'field'`:
// only the first slash is stripped (String#replace without /g).
func helpAJVFieldName(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "field"
	}
	return trimmed
}

// helpValidateAJV validates a body against spec, returning the coerced value and
// the first AJV error (allErrors:false).
func helpValidateAJV(value any, spec helpAJVSpec) (any, *helpAJVError) {
	return helpValidateAJVAt(value, spec, "")
}

func helpValidateAJVAt(value any, spec helpAJVSpec, path string) (any, *helpAJVError) {
	switch spec.Kind {
	case helpAJVString:
		text, message := ajvCoerceString(value)
		if message != "" {
			return nil, &helpAJVError{Keyword: "type", Message: message, Path: path}
		}
		// AJV checks maxLength before minLength, then pattern.
		if spec.MaxLength > 0 && utf8.RuneCountInString(text) > spec.MaxLength {
			return nil, &helpAJVError{Keyword: "maxLength", Message: "is too long", Path: path}
		}
		if spec.MinLength > 0 && utf8.RuneCountInString(text) < spec.MinLength {
			return nil, &helpAJVError{Keyword: "minLength", Message: "is too short", Path: path}
		}
		if spec.Pattern != nil && !spec.Pattern.MatchString(text) {
			return nil, &helpAJVError{Keyword: "pattern", Message: "format is invalid", Path: path}
		}
		return text, nil

	case helpAJVInteger:
		number, ok := ajvCoerceInteger(value)
		if !ok {
			return nil, &helpAJVError{Keyword: "type", Message: "must be integer", Path: path}
		}
		if !math.IsInf(number, 0) && !math.IsNaN(number) {
			if spec.HasMaximum && number > spec.Maximum {
				return nil, &helpAJVError{
					Keyword: "maximum",
					Message: "must be <= " + formatTerminalCommandNumber(spec.Maximum),
					Path:    path,
				}
			}
			if spec.HasMinimum && number < spec.Minimum {
				return nil, &helpAJVError{
					Keyword: "minimum",
					Message: "must be >= " + formatTerminalCommandNumber(spec.Minimum),
					Path:    path,
				}
			}
		}
		return number, nil

	case helpAJVBoolean:
		flag, ok := ajvCoerceBoolean(value)
		if !ok {
			return nil, &helpAJVError{Keyword: "type", Message: "must be boolean", Path: path}
		}
		return flag, nil

	case helpAJVArray:
		items, ok := helpCoerceAJVArray(value)
		if !ok {
			return nil, &helpAJVError{Keyword: "type", Message: "must be array", Path: path}
		}
		if spec.MaxItems > 0 && len(items) > spec.MaxItems {
			return nil, &helpAJVError{
				Keyword: "maxItems",
				Message: "must NOT have more than " + formatTerminalCommandNumber(float64(spec.MaxItems)) + " items",
				Path:    path,
			}
		}
		out := make([]any, 0, len(items))
		for index, item := range items {
			itemPath := path + "/" + formatTerminalCommandNumber(float64(index))
			if spec.Items != nil {
				coerced, err := helpValidateAJVAt(item, *spec.Items, itemPath)
				if err != nil {
					return nil, err
				}
				out = append(out, coerced)
				continue
			}
			out = append(out, item)
		}
		return out, nil

	case helpAJVObject:
		object, ok := value.(map[string]any)
		if !ok {
			return nil, &helpAJVError{Keyword: "type", Message: "must be object", Path: path}
		}
		// `required` runs before the properties, in schema order.
		for _, name := range spec.Required {
			if _, present := object[name]; !present {
				return nil, &helpAJVError{
					Keyword: "required",
					Message: name + " is required",
					Path:    path,
					Missing: name,
				}
			}
		}
		out := make(map[string]any, len(object))
		for key, item := range object {
			out[key] = item
		}
		for _, child := range spec.Properties {
			value, present := object[child.Name]
			if !present {
				continue
			}
			coerced, err := helpValidateAJVAt(value, child, path+"/"+child.Name)
			if err != nil {
				return nil, err
			}
			out[child.Name] = coerced
		}
		return out, nil
	}
	return value, nil
}

// helpCoerceAJVArray mirrors coerceTypes:'array' for an array-typed property: a
// scalar (or null) is wrapped into a one-element array.
func helpCoerceAJVArray(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case string:
		return []any{typed}, true
	case float64:
		return []any{typed}, true
	case bool:
		return []any{typed}, true
	case nil:
		return []any{nil}, true
	}
	return nil, false
}

// ==================== schemas ====================

var helpAdminArticleFields = []helpAJVSpec{
	{Name: "title", Kind: helpAJVString, MinLength: 1, MaxLength: maxHelpTitleLength},
	{Name: "slug", Kind: helpAJVString, MinLength: 1, MaxLength: maxHelpSlugLength, Pattern: helpSlugPattern},
	{Name: "content", Kind: helpAJVString, MaxLength: maxHelpContentLength},
	{Name: "category", Kind: helpAJVString, MinLength: 1, MaxLength: maxHelpCategoryLength},
	{
		Name:       "sortOrder",
		Kind:       helpAJVInteger,
		HasMinimum: true,
		Minimum:    0,
		HasMaximum: true,
		Maximum:    maxHelpSortOrder,
	},
	{Name: "published", Kind: helpAJVBoolean},
	{Name: "pinned", Kind: helpAJVBoolean},
}

var helpArticleCreateSchema = helpAJVSpec{
	Kind:       helpAJVObject,
	Required:   []string{"title", "slug", "content"},
	Properties: helpAdminArticleFields,
}

var helpArticleUpdateSchema = helpAJVSpec{
	Kind:       helpAJVObject,
	Properties: helpAdminArticleFields,
}

var helpCategoryConfigSchema = helpAJVSpec{
	Kind:     helpAJVObject,
	Required: []string{"categories"},
	Properties: []helpAJVSpec{
		{
			Name:     "categories",
			Kind:     helpAJVArray,
			MaxItems: maxHelpCategoryConfigItems,
			Items: &helpAJVSpec{
				Kind:     helpAJVObject,
				Required: []string{"id", "name", "color"},
				Properties: []helpAJVSpec{
					{
						Name:      "id",
						Kind:      helpAJVString,
						MinLength: 1,
						MaxLength: maxHelpCategoryLength,
						Pattern:   helpSlugPattern,
					},
					{
						Name:      "name",
						Kind:      helpAJVString,
						MinLength: 1,
						MaxLength: maxHelpCategoryNameLength,
					},
					{Name: "color", Kind: helpAJVString, Pattern: helpColorPattern},
				},
			},
		},
	},
}

// ==================== route helpers ====================

// helpBodyHas mirrors the `value === undefined` checks of the normalizers: Go
// maps distinguish "key absent" from "key present with a JSON null", and so does
// JavaScript, so presence (not nil-ness) is what the callers must pass on.
func helpBodyHas(object map[string]any, key string) bool {
	_, present := object[key]
	return present
}

// helpReadValidatedBody decodes the request body and runs the AJV replica. It
// writes the error response itself and reports ok=false on failure.
func helpReadValidatedBody(c *gin.Context, schema helpAJVSpec) (map[string]any, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	coerced, ajvError := helpValidateAJV(decoded.Value, schema)
	if ajvError != nil {
		respondAJVValidation(c, helpRenderAJVError(ajvError))
		return nil, false
	}
	object, ok := coerced.(map[string]any)
	if !ok {
		respondAJVValidation(c, "must be object")
		return nil, false
	}
	return object, true
}

func helpRespondInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// helpPublishedOnly builds the `options.publishedOnly` value. The public routes
// pass `publishedOnly: true` explicitly; the admin list passes `false`.
func helpPublishedOnly(value bool) *bool {
	return &value
}

// helpHasPublishedArticles mirrors hasPublishedHelpArticles. A query failure is
// reported to the caller because the TypeScript version would throw (→ 500).
func helpHasPublishedArticles(c *gin.Context) (bool, bool) {
	result, err := db.GetHelpArticles(c.Request.Context(), db.HelpArticleListOptions{
		Page:          1,
		PageSize:      1,
		PublishedOnly: helpPublishedOnly(true),
	})
	if err != nil {
		return false, false
	}
	return result.Total > 0, true
}

// RegisterHelp mounts /api/help. Public routes are unauthenticated; the /admin
// subtree carries fastify.authenticateAdmin.
func RegisterHelp(r *gin.Engine) {
	group := r.Group("/api/help")

	// GET / — published articles, with the built-in article as a fresh-install
	// fallback. Fastify registers both "/api/help" and "/api/help/" for a plugin
	// root (prefixTrailingSlash 'both').
	helpListHandler := func(c *gin.Context) {
		query := c.Request.URL.Query()
		var category *string
		if raw := query.Get("category"); raw != "" {
			category = &raw
		}
		var search *string
		if raw := query.Get("search"); raw != "" {
			search = &raw
		}

		result, err := db.GetHelpArticles(c.Request.Context(), db.HelpArticleListOptions{
			Page:          lib.ParsePositiveIntegerQuery(query.Get("page"), 1, 100000),
			PageSize:      lib.ParsePositiveIntegerQuery(query.Get("pageSize"), 20, 100),
			PublishedOnly: helpPublishedOnly(true),
			Category:      category,
			Search:        search,
		})
		if err != nil {
			helpRespondInternalError(c)
			return
		}

		if result.Total == 0 && result.Page == 1 && category == nil && search == nil {
			articles := make([]helpPublicArticleSummary, 0, len(helpDefaultArticles))
			for _, article := range helpDefaultArticles {
				articles = append(articles, helpPublicSummaryFromDefault(article))
			}
			c.JSON(http.StatusOK, gin.H{
				"articles":   articles,
				"total":      len(helpDefaultArticles),
				"page":       result.Page,
				"pageSize":   result.PageSize,
				"totalPages": 1,
			})
			return
		}

		articles := make([]helpPublicArticleSummary, 0, len(result.Items))
		for _, article := range result.Items {
			articles = append(articles, helpPublicSummaryFromRow(article))
		}
		c.JSON(http.StatusOK, gin.H{
			"articles":   articles,
			"total":      result.Total,
			"page":       result.Page,
			"pageSize":   result.PageSize,
			"totalPages": result.TotalPages,
		})
	}
	group.GET("", helpListHandler)
	group.GET("/", helpListHandler)

	// GET /pinned — pinned articles for the dashboard home page.
	group.GET("/pinned", func(c *gin.Context) {
		limit := lib.ParsePositiveIntegerQuery(c.Request.URL.Query().Get("limit"), 6, 20)

		articles, err := db.GetPinnedHelpArticles(c.Request.Context(), limit)
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if len(articles) == 0 {
			hasPublished, ok := helpHasPublishedArticles(c)
			if !ok {
				helpRespondInternalError(c)
				return
			}
			if hasPublished {
				if articles == nil {
					articles = []db.HelpPinnedArticle{}
				}
				c.JSON(http.StatusOK, gin.H{"articles": articles})
				return
			}
			fallback := make([]db.HelpPinnedArticle, 0, limit)
			for index, article := range helpDefaultArticles {
				if index >= limit {
					break
				}
				fallback = append(fallback, db.HelpPinnedArticle{
					ID:       article.ID,
					Title:    article.Title,
					Slug:     article.Slug,
					Category: article.Category,
				})
			}
			articles = fallback
		}
		if articles == nil {
			articles = []db.HelpPinnedArticle{}
		}
		c.JSON(http.StatusOK, gin.H{"articles": articles})
	})

	// GET /category-config — admin-configured categories, defaults otherwise.
	group.GET("/category-config", func(c *gin.Context) {
		configStr, err := db.GetSystemConfig(c.Request.Context(), db.HelpCategoryConfigKey, false)
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if configStr != nil && *configStr != "" {
			var parsed any
			if json.Unmarshal([]byte(*configStr), &parsed) == nil {
				c.JSON(http.StatusOK, gin.H{"categories": parsed})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"categories": helpDefaultCategories})
	})

	// GET /categories — published article counts per category.
	group.GET("/categories", func(c *gin.Context) {
		categories, err := db.GetHelpCategories(c.Request.Context())
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if len(categories) == 0 {
			hasPublished, ok := helpHasPublishedArticles(c)
			if !ok {
				helpRespondInternalError(c)
				return
			}
			if !hasPublished {
				categories = []db.HelpCategoryCount{{
					Category: helpDefaultArticles[0].Category,
					Count:    int64(len(helpDefaultArticles)),
				}}
			}
		}
		if categories == nil {
			categories = []db.HelpCategoryCount{}
		}
		c.JSON(http.StatusOK, gin.H{"categories": categories})
	})

	// GET /article/:slug — published article by slug.
	group.GET("/article/:slug", func(c *gin.Context) {
		article, err := db.GetHelpArticleBySlug(c.Request.Context(), c.Param("slug"))
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if article == nil {
			for _, candidate := range helpDefaultArticles {
				if candidate.Slug != c.Param("slug") {
					continue
				}
				hasPublished, ok := helpHasPublishedArticles(c)
				if !ok {
					helpRespondInternalError(c)
					return
				}
				if !hasPublished {
					c.JSON(http.StatusOK, gin.H{"article": helpPublicDetailFromDefault(candidate)})
					return
				}
			}
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrArticleNotFound, "")
			return
		}

		c.JSON(http.StatusOK, gin.H{"article": struct {
			ID        int32  `json:"id"`
			Title     string `json:"title"`
			Slug      string `json:"slug"`
			Content   string `json:"content"`
			Category  string `json:"category"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		}{
			ID:        article.ID,
			Title:     article.Title,
			Slug:      article.Slug,
			Content:   article.Content,
			Category:  article.Category,
			CreatedAt: article.CreatedAt,
			UpdatedAt: article.UpdatedAt,
		}})
	})

	admin := group.Group("/admin", middleware.AuthenticateAdmin())

	// PUT /admin/category-config — persist the category list.
	admin.PUT("/category-config", func(c *gin.Context) {
		body, ok := helpReadValidatedBody(c, helpCategoryConfigSchema)
		if !ok {
			return
		}
		categories, valid := helpNormalizeCategoryConfigInput(body)
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
			return
		}

		ctx := c.Request.Context()
		exists, err := db.HelpCategoryConfigExists(ctx, db.HelpCategoryConfigKey)
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		serialized := helpJSONString(categories)
		if exists {
			if err := db.UpdateSystemConfig(ctx, db.HelpCategoryConfigKey, serialized); err != nil {
				helpRespondInternalError(c)
				return
			}
		} else {
			if err := db.CreateHelpCategoryConfig(ctx, db.HelpCategoryConfigKey, serialized); err != nil {
				helpRespondInternalError(c)
				return
			}
			lib.InvalidateCachedConfig(db.HelpCategoryConfigKey)
		}

		userID := int64(middleware.CurrentUser(c).ID)
		db.CreateLog(ctx, &userID, "system", "help.category_config",
			"Updated help category config ("+formatTerminalCommandNumber(float64(len(categories)))+" categories)",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Category config saved"})
	})

	// GET /admin — every article, published or not.
	admin.GET("", func(c *gin.Context) {
		query := c.Request.URL.Query()
		var category *string
		if raw := query.Get("category"); raw != "" {
			category = &raw
		}
		var search *string
		if raw := query.Get("search"); raw != "" {
			search = &raw
		}

		result, err := db.GetHelpArticles(c.Request.Context(), db.HelpArticleListOptions{
			Page:          lib.ParsePositiveIntegerQuery(query.Get("page"), 1, 100000),
			PageSize:      lib.ParsePositiveIntegerQuery(query.Get("pageSize"), 20, 100),
			PublishedOnly: helpPublishedOnly(false),
			Category:      category,
			Search:        search,
		})
		if err != nil {
			helpRespondInternalError(c)
			return
		}

		articles := make([]helpAdminArticleListItem, 0, len(result.Items))
		for _, article := range result.Items {
			articles = append(articles, helpAdminArticleListItem{
				ID:        article.ID,
				Title:     article.Title,
				Slug:      article.Slug,
				Category:  article.Category,
				SortOrder: article.SortOrder,
				Published: article.Published,
				Pinned:    article.Pinned,
				CreatedAt: article.CreatedAt,
				UpdatedAt: article.UpdatedAt,
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"articles":   articles,
			"total":      result.Total,
			"page":       result.Page,
			"pageSize":   result.PageSize,
			"totalPages": result.TotalPages,
		})
	})

	// GET /admin/:id — article detail.
	admin.GET("/:id", func(c *gin.Context) {
		articleID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		article, err := db.GetHelpArticleByID(c.Request.Context(), int32(articleID))
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if article == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrArticleNotFound, "")
			return
		}
		c.JSON(http.StatusOK, gin.H{"article": helpAdminArticleFromModel(article)})
	})

	// POST /admin — create an article.
	admin.POST("", func(c *gin.Context) {
		body, ok := helpReadValidatedBody(c, helpArticleCreateSchema)
		if !ok {
			return
		}

		title, titleOK := helpNormalizeRequiredText(body["title"], maxHelpTitleLength)
		slug, slugOK := helpNormalizeSlug(body["slug"])
		content, contentOK := helpNormalizeRequiredText(body["content"], maxHelpContentLength)
		category := "general"
		if normalized, valid := helpNormalizeOptionalText(body["category"], helpBodyHas(body, "category"), maxHelpCategoryLength); !valid {
			// `normalizeOptionalText(...) ?? 'general'` swallows null, so an invalid
			// category falls back to "general" (the `=== null` guard below is dead
			// code in the TypeScript, reproduced faithfully here).
			category = "general"
		} else if normalized != nil {
			category = *normalized
		}
		sortOrder, sortOK := helpNormalizeSortOrder(body["sortOrder"], helpBodyHas(body, "sortOrder"))
		published, publishedOK := helpNormalizeOptionalBoolean(body["published"], helpBodyHas(body, "published"))
		pinned, pinnedOK := helpNormalizeOptionalBoolean(body["pinned"], helpBodyHas(body, "pinned"))

		if !titleOK || !slugOK || !contentOK || !sortOK || !publishedOK || !pinnedOK {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
			return
		}

		ctx := c.Request.Context()
		existing, err := db.GetAnyHelpArticleBySlug(ctx, slug)
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if existing != nil {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrSlugExists, "")
			return
		}

		publishedValue := true
		if published != nil {
			publishedValue = *published
		}
		pinnedValue := pinned != nil && *pinned

		userID := middleware.CurrentUser(c).ID
		articleID, err := db.CreateHelpArticle(ctx, db.HelpArticleCreateData{
			Title:     title,
			Slug:      slug,
			Content:   content,
			Category:  &category,
			SortOrder: &sortOrder,
			Published: &publishedValue,
			Pinned:    &pinnedValue,
			CreatedBy: &userID,
		})
		if err != nil {
			helpRespondInternalError(c)
			return
		}

		actorID := int64(userID)
		db.CreateLog(ctx, &actorID, "system", "help.create",
			"Created help article \""+title+"\" (ID: "+formatTerminalCommandNumber(float64(articleID))+")",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{
			"message": "Article created",
			"article": gin.H{
				"id":    articleID,
				"title": title,
				"slug":  slug,
			},
		})
	})

	// PATCH /admin/:id — partial update.
	admin.PATCH("/:id", func(c *gin.Context) {
		// Fastify validates the body before the handler runs.
		body, ok := helpReadValidatedBody(c, helpArticleUpdateSchema)
		if !ok {
			return
		}

		articleID, idOK := lib.ParsePositiveRouteID(c.Param("id"))
		if !idOK {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		var title *string
		{
			value, valid := helpNormalizeOptionalText(body["title"], helpBodyHas(body, "title"), maxHelpTitleLength)
			if !valid {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
				return
			}
			title = value
		}
		slug, slugValid := helpNormalizeOptionalSlug(body["slug"], helpBodyHas(body, "slug"))
		if !slugValid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
			return
		}
		content, contentValid := helpNormalizeOptionalText(body["content"], helpBodyHas(body, "content"), maxHelpContentLength)
		if !contentValid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
			return
		}
		category, categoryValid := helpNormalizeOptionalText(body["category"], helpBodyHas(body, "category"), maxHelpCategoryLength)
		if !categoryValid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
			return
		}
		var sortOrder *int32
		if helpBodyHas(body, "sortOrder") {
			value, sortOK := helpNormalizeSortOrder(body["sortOrder"], true)
			if !sortOK {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
				return
			}
			sortOrder = &value
		}
		published, publishedValid := helpNormalizeOptionalBoolean(body["published"], helpBodyHas(body, "published"))
		if !publishedValid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
			return
		}
		pinned, pinnedValid := helpNormalizeOptionalBoolean(body["pinned"], helpBodyHas(body, "pinned"))
		if !pinnedValid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
			return
		}

		ctx := c.Request.Context()
		article, err := db.GetHelpArticleByID(ctx, int32(articleID))
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if article == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrArticleNotFound, "")
			return
		}

		if slug != nil && *slug != article.Slug {
			existing, err := db.GetAnyHelpArticleBySlug(ctx, *slug)
			if err != nil {
				helpRespondInternalError(c)
				return
			}
			if existing != nil {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrSlugExists, "")
				return
			}
		}

		// The TypeScript returns early when nothing was provided, so `updatedAt` is
		// left untouched for an empty body — reproduce that instead of bumping it.
		hasUpdates := title != nil || slug != nil || content != nil || category != nil ||
			sortOrder != nil || published != nil || pinned != nil
		if hasUpdates {
			if err := db.UpdateHelpArticle(ctx, int32(articleID), db.HelpArticleUpdateData{
				Title:     title,
				Slug:      slug,
				Content:   content,
				Category:  category,
				SortOrder: sortOrder,
				Published: published,
				Pinned:    pinned,
			}); err != nil {
				helpRespondInternalError(c)
				return
			}
		}

		userID := int64(middleware.CurrentUser(c).ID)
		db.CreateLog(ctx, &userID, "system", "help.update",
			"Updated help article \""+article.Title+"\" (ID: "+formatTerminalCommandNumber(float64(articleID))+")",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Article updated"})
	})

	// DELETE /admin/:id
	admin.DELETE("/:id", func(c *gin.Context) {
		articleID, idOK := lib.ParsePositiveRouteID(c.Param("id"))
		if !idOK {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		ctx := c.Request.Context()
		article, err := db.GetHelpArticleByID(ctx, int32(articleID))
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if article == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrArticleNotFound, "")
			return
		}

		if err := db.DeleteHelpArticle(ctx, int32(articleID)); err != nil {
			helpRespondInternalError(c)
			return
		}

		userID := int64(middleware.CurrentUser(c).ID)
		db.CreateLog(ctx, &userID, "system", "help.delete",
			"Deleted help article \""+article.Title+"\" (ID: "+formatTerminalCommandNumber(float64(articleID))+")",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Article deleted"})
	})
}

func helpAdminArticleFromModel(article *db.HelpArticle) helpAdminArticleDetail {
	return helpAdminArticleDetail{
		ID:        article.ID,
		Title:     article.Title,
		Slug:      article.Slug,
		Content:   article.Content,
		Category:  article.Category,
		SortOrder: article.SortOrder,
		Published: article.Published,
		Pinned:    article.Pinned,
		CreatedAt: article.CreatedAt,
		UpdatedAt: article.UpdatedAt,
	}
}

func helpPublicDetailFromDefault(article helpDefaultArticle) helpPublicArticleDetail {
	return helpPublicArticleDetail{
		helpPublicArticleSummary: helpPublicSummaryFromDefault(article),
		Content:                  article.Content,
	}
}

func init() {
	Register(RegisterHelp)
}
