package routes

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// ==================== pagination ====================

// TestHelpParsePositiveIntegerQuery pins the query-string pagination contract the
// public help routes inherit from lib.ParsePositiveIntegerQuery: a malformed value
// falls back, a valid one is clamped to max, and 0 / -1 / 01 are malformed.
func TestHelpParsePositiveIntegerQuery(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		fallback int
		max      int
		want     int
	}{
		{"absent", "", 1, 100000, 1},
		{"pageSize default", "", 20, 100, 20},
		{"valid", "7", 1, 100000, 7},
		{"clamped to max", "500", 20, 100, 100},
		{"exactly max", "100", 20, 100, 100},
		{"zero falls back", "0", 20, 100, 20},
		{"negative falls back", "-5", 20, 100, 20},
		{"leading zero falls back", "01", 20, 100, 20},
		{"float falls back", "1.5", 20, 100, 20},
		{"non numeric falls back", "abc", 20, 100, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := helpTestParseQuery(tc.raw, tc.fallback, tc.max); got != tc.want {
				t.Fatalf("parseQuery(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// helpTestParseQuery isolates the library call so the expectation stays readable.
func helpTestParseQuery(raw string, fallback, max int) int {
	return lib.ParsePositiveIntegerQuery(raw, fallback, max)
}

// ==================== slug / category normalization ====================

func TestHelpNormalizeSlug(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
		ok    bool
	}{
		{"simple", "getting-started", "getting-started", true},
		{"digits", "faq-2", "faq-2", true},
		{"trimmed", "  faq  ", "faq", true},
		{"empty", "", "", false},
		{"whitespace only", "   ", "", false},
		{"uppercase rejected", "FAQ", "", false},
		{"underscore rejected", "faq_page", "", false},
		{"slash rejected", "faq/page", "", false},
		{"non string", 42.0, "", false},
		{"too long", string(make([]byte, maxHelpSlugLength+1)), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := helpNormalizeSlug(tc.value)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if tc.ok && got != tc.want {
				t.Fatalf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHelpNormalizeCategoryIDRejectsUnsafeValues(t *testing.T) {
	// The category id feeds both the JSON config and (indirectly) the SQL category
	// filter, so the [a-z0-9-]+ guard is a security boundary.
	// "-" is accepted: HELP_SLUG_PATTERN (/^[a-z0-9-]+$/) allows it in the TypeScript.
	for _, unsafe := range []string{"../../etc", "category'or'1", "cat egory", "CAT", "faq_page", ""} {
		if _, ok := helpNormalizeCategoryID(unsafe); ok {
			t.Fatalf("category id %q must be rejected", unsafe)
		}
	}
	if got, ok := helpNormalizeCategoryID("-"); !ok || got != "-" {
		t.Fatalf("dash id: got %q ok=%v, want the TS behaviour (accepted)", got, ok)
	}
	if got, ok := helpNormalizeCategoryID("  billing  "); !ok || got != "billing" {
		t.Fatalf("billing: got %q ok=%v", got, ok)
	}
}

// ==================== sort order ====================

// TestHelpNormalizeSortOrder pins the integer range: absent → 0, but NaN (a
// non-integer or out-of-range value) is a validation failure, not a silent zero.
func TestHelpNormalizeSortOrder(t *testing.T) {
	cases := []struct {
		name    string
		value   any
		present bool
		want    int32
		ok      bool
	}{
		{"absent", nil, false, 0, true},
		{"zero", 0.0, true, 0, true},
		{"max", float64(maxHelpSortOrder), true, maxHelpSortOrder, true},
		{"over max", float64(maxHelpSortOrder + 1), true, 0, false},
		{"negative", -1.0, true, 0, false},
		{"fraction", 1.5, true, 0, false},
		{"string", "3", true, 0, false},
		{"null literal", nil, true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := helpNormalizeSortOrder(tc.value, tc.present)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if tc.ok && got != tc.want {
				t.Fatalf("value = %d, want %d", got, tc.want)
			}
		})
	}
}

// ==================== AJV replica ====================

// TestHelpCreateSchemaOrderOfErrors pins AJV's allErrors:false contract: `required`
// is reported before any property constraint, and only the first failure surfaces.
func TestHelpCreateSchemaOrderOfErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not an object", `[]`, "must be object"},
		{"missing title", `{"slug":"a","content":"c"}`, "title is required"},
		{"missing slug", `{"title":"t","content":"c"}`, "slug is required"},
		{"missing content", `{"title":"t","slug":"a"}`, "content is required"},
		{"slug pattern", `{"title":"t","slug":"A B","content":"c"}`, "slug format is invalid"},
		{"slug too long", `{"title":"t","slug":"` + longHelpSlug(maxHelpSlugLength+1) + `","content":"c"}`, "slug is too long"},
		{"title too long", `{"title":"` + repeatHelpRune('x', maxHelpTitleLength+1) + `","slug":"a","content":"c"}`, "title is too long"},
		{"sort order negative", `{"title":"t","slug":"a","content":"c","sortOrder":-1}`, "must be >= 0"},
		{"sort order fraction", `{"title":"t","slug":"a","content":"c","sortOrder":1.5}`, "must be integer"},
		{"published wrong type", `{"title":"t","slug":"a","content":"c","published":"yes"}`, "must be boolean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var decoded any
			if err := json.Unmarshal([]byte(tc.body), &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			_, ajvErr := helpValidateAJV(decoded, helpArticleCreateSchema)
			if ajvErr == nil {
				t.Fatalf("expected a validation error")
			}
			if got := helpRenderAJVError(ajvErr); got != tc.want {
				t.Fatalf("message = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHelpValidateAJVCoercion pins coerceTypes:'array': scalars are stringified for
// string properties, single-element arrays unwrap, and "true"/"false" become bools.
func TestHelpValidateAJVCoercion(t *testing.T) {
	var decoded any
	body := `{"title":123,"slug":["abc"],"content":"c","published":"true","sortOrder":"5"}`
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	coerced, ajvErr := helpValidateAJV(decoded, helpArticleCreateSchema)
	if ajvErr != nil {
		t.Fatalf("unexpected error: %s", helpRenderAJVError(ajvErr))
	}
	object := coerced.(map[string]any)
	if object["title"] != "123" {
		t.Fatalf("title = %#v, want \"123\"", object["title"])
	}
	if object["slug"] != "abc" {
		t.Fatalf("slug = %#v, want \"abc\"", object["slug"])
	}
	if object["published"] != true {
		t.Fatalf("published = %#v, want true", object["published"])
	}
	if object["sortOrder"] != 5.0 {
		t.Fatalf("sortOrder = %#v, want 5", object["sortOrder"])
	}
}

// TestHelpCategoryConfigSchema pins the nested array validation (maxItems, unique
// ids, color pattern) used by PUT /admin/category-config.
func TestHelpCategoryConfigSchema(t *testing.T) {
	valid := `{"categories":[{"id":"a","name":"A","color":"#ffffff"}]}`
	var decoded any
	if err := json.Unmarshal([]byte(valid), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	coerced, ajvErr := helpValidateAJV(decoded, helpCategoryConfigSchema)
	if ajvErr != nil {
		t.Fatalf("unexpected error: %s", helpRenderAJVError(ajvErr))
	}
	categories, ok := helpNormalizeCategoryConfigInput(coerced)
	if !ok || len(categories) != 1 || categories[0].ID != "a" {
		t.Fatalf("categories = %#v ok=%v", categories, ok)
	}

	// Duplicate ids pass the schema but fail the handler's normalizer.
	duplicate := `{"categories":[{"id":"a","name":"A","color":"#ffffff"},{"id":"a","name":"B","color":"#000000"}]}`
	if err := json.Unmarshal([]byte(duplicate), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	coerced, ajvErr = helpValidateAJV(decoded, helpCategoryConfigSchema)
	if ajvErr != nil {
		t.Fatalf("unexpected schema error: %s", helpRenderAJVError(ajvErr))
	}
	if _, ok := helpNormalizeCategoryConfigInput(coerced); ok {
		t.Fatalf("duplicate category ids must be rejected")
	}

	// A bad color is a schema-level 400.
	badColor := `{"categories":[{"id":"a","name":"A","color":"red"}]}`
	if err := json.Unmarshal([]byte(badColor), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ajvErr := helpValidateAJV(decoded, helpCategoryConfigSchema); ajvErr == nil {
		t.Fatalf("bad color must fail validation")
	}
}

// TestHelpDefaultArticlesShape keeps the built-in fallback article consistent: it
// is the only published+pinned row and its timestamps are the pinned constant.
func TestHelpDefaultArticlesShape(t *testing.T) {
	if len(helpDefaultArticles) != 1 {
		t.Fatalf("expected exactly 1 default article, got %d", len(helpDefaultArticles))
	}
	article := helpDefaultArticles[0]
	if article.Published != 1 || article.Pinned != 1 {
		t.Fatalf("default article must be published and pinned")
	}
	if article.CreatedAt != defaultHelpArticleTimestamp || article.UpdatedAt != defaultHelpArticleTimestamp {
		t.Fatalf("default article timestamps = %q / %q", article.CreatedAt, article.UpdatedAt)
	}
	if !helpSlugPattern.MatchString(article.Slug) {
		t.Fatalf("default slug %q is not slug-shaped", article.Slug)
	}
	summary := helpPublicSummaryFromDefault(article)
	if summary.CreatedAtCamel != summary.CreatedAt {
		t.Fatalf("camelCase timestamp must mirror the snake_case one")
	}
	want := len(helpDefaultCategories)
	if want != 6 {
		t.Fatalf("expected 6 default categories, got %d", want)
	}
	for _, category := range helpDefaultCategories {
		if !helpColorPattern.MatchString(category.Color) {
			t.Fatalf("default category %q has invalid color %q", category.ID, category.Color)
		}
		if !helpSlugPattern.MatchString(category.ID) {
			t.Fatalf("default category id %q is not slug-shaped", category.ID)
		}
	}
	if !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(helpDefaultCategories[0].Color) {
		t.Fatalf("color pattern drift")
	}
}

// ==================== timestamps ====================

// TestHelpTimestampsAreISO8601 pins the wire format: the help payload reuses the
// database layer's Date#toISOString rendering (UTC, milliseconds, trailing Z).
func TestHelpTimestampsAreISO8601(t *testing.T) {
	isoPattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	if !isoPattern.MatchString(defaultHelpArticleTimestamp) {
		t.Fatalf("default timestamp %q is not ISO-8601", defaultHelpArticleTimestamp)
	}
	article := helpDefaultArticles[0]
	if article.CreatedAt != article.UpdatedAt {
		t.Fatalf("default article created/updated must match")
	}
}

// ==================== helpers under test ====================

func longHelpSlug(length int) string {
	return repeatHelpRune('a', length)
}

func repeatHelpRune(value rune, count int) string {
	out := make([]rune, count)
	for index := range out {
		out[index] = value
	}
	return string(out)
}
