// Port of server/src/routes/custom-init-commands.ts.
//
// Every package-level identifier is prefixed with `customInit` because the routes
// package is shared by many parallel ports.
//
// Fastify validates bodies/querystrings with its default AJV options
// (allErrors:false, coerceTypes:'array', removeAdditional:true) *before* the
// handler runs, and app.ts' setErrorHandler maps the first keyword to a message.
// The replica below reproduces that ordering and message text; the union-typed
// `description` (string|null) and the boolean `enabled` reuse the helpers in
// storage_configs.go (ajvKindNullableString / ajvKindBoolean).
//
// Behavioural quirks preserved on purpose:
//
//   - The `Invalid distros` responses carry no `code` field, unlike the apiError
//     envelope used everywhere else in this module.
//   - `GET /available` rejects anything outside VALID_QUERY_DISTROS (which adds
//     "other" to the write-side list) with 400 INVALID_PARAMS "Invalid distro".
//   - `GET /:id` returns 404 when the row is missing and 403 when it belongs to
//     another user, in that order.
package routes

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

const (
	// customInitNameMaxLength mirrors the `name` schema bound.
	customInitNameMaxLength = 100
	// customInitDescriptionMaxLength mirrors the `description` schema bound.
	customInitDescriptionMaxLength = 500
	// customInitCommandPreviewLength mirrors truncateText(cmd.command, 100).
	customInitCommandPreviewLength = 100
	// customInitLogModule mirrors the literal 'custom_init_command' module.
	customInitLogModule = "custom_init_command"
)

// customInitValidDistros mirrors VALID_DISTROS.
var customInitValidDistros = []string{"ubuntu", "debian", "rhel", "alpine", "arch", "suse", "all"}

// customInitValidQueryDistros mirrors VALID_QUERY_DISTROS (`[...VALID_DISTROS,
// 'other']`).
var customInitValidQueryDistros = []string{"ubuntu", "debian", "rhel", "alpine", "arch", "suse", "all", "other"}

// customInitDistroOption mirrors one entry of the static /distros response.
type customInitDistroOption struct {
	ID   string `json:"id"`
	Icon string `json:"icon"`
}

// customInitDistroOptions mirrors the literal list returned by GET /distros.
var customInitDistroOptions = []customInitDistroOption{
	{ID: "ubuntu", Icon: "ubuntu"},
	{ID: "debian", Icon: "debian"},
	{ID: "rhel", Icon: "almalinux"},
	{ID: "alpine", Icon: "alpine"},
	{ID: "arch", Icon: "arch"},
	{ID: "suse", Icon: "opensuse"},
	{ID: "all", Icon: "linux"},
}

// ==================== response shapes ====================

// customInitCommandSummary mirrors the GET / projection (command truncated).
type customInitCommandSummary struct {
	ID               int32    `json:"id"`
	Name             string   `json:"name"`
	CommandPreview   string   `json:"commandPreview"`
	CommandLineCount int      `json:"commandLineCount"`
	Distros          []string `json:"distros"`
	Description      *string  `json:"description"`
	Enabled          bool     `json:"enabled"`
	CreatedAt        string   `json:"createdAt"`
	UpdatedAt        string   `json:"updatedAt"`
}

// customInitAvailableCommand mirrors the GET /available projection.
type customInitAvailableCommand struct {
	ID               int32    `json:"id"`
	Name             string   `json:"name"`
	CommandLineCount int      `json:"commandLineCount"`
	Distros          []string `json:"distros"`
	Description      *string  `json:"description"`
}

// customInitCommandDetail mirrors the GET /:id projection.
type customInitCommandDetail struct {
	ID          int32    `json:"id"`
	Name        string   `json:"name"`
	Command     string   `json:"command"`
	Distros     []string `json:"distros"`
	Description *string  `json:"description"`
	Enabled     bool     `json:"enabled"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

// ==================== text helpers ====================

// customInitTruncateText mirrors truncateText(): the length and the cut are both
// counted in UTF-16 code units, like String#length / String#substring.
func customInitTruncateText(text string, maxLength int) string {
	if text == "" || customInitUTF16Length(text) <= maxLength {
		return text
	}
	return customInitSliceUTF16(text, maxLength) + "..."
}

// customInitUTF16Length mirrors String.prototype.length.
func customInitUTF16Length(value string) int {
	length := 0
	for _, char := range value {
		if char > 0xFFFF {
			length += 2
			continue
		}
		length++
	}
	return length
}

// customInitSliceUTF16 mirrors String#substring(0, end) over UTF-16 code units.
func customInitSliceUTF16(value string, end int) string {
	units := 0
	for index, runeValue := range value {
		if units >= end {
			return value[:index]
		}
		if runeValue > 0xFFFF {
			if units+2 > end {
				// A lone surrogate: JavaScript would cut mid-pair. Dropping the
				// character is the closest well-formed approximation.
				return value[:index]
			}
			units += 2
			continue
		}
		units++
	}
	return value
}

// customInitCountCommandLines mirrors
// `cmd.command.split('\n').filter(line => line.trim()).length`.
func customInitCountCommandLines(command string) int {
	count := 0
	for _, line := range strings.Split(command, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// customInitDistroAllowed reports membership in the write-side distro list.
func customInitDistroAllowed(distro string) bool {
	for _, candidate := range customInitValidDistros {
		if candidate == distro {
			return true
		}
	}
	return false
}

// customInitQueryDistroAllowed reports membership in VALID_QUERY_DISTROS.
func customInitQueryDistroAllowed(distro string) bool {
	for _, candidate := range customInitValidQueryDistros {
		if candidate == distro {
			return true
		}
	}
	return false
}

// customInitInvalidDistros mirrors `distros.filter(d => !VALID_DISTROS.includes(d))`.
func customInitInvalidDistros(distros []string) []string {
	invalid := make([]string, 0)
	for _, distro := range distros {
		if !customInitDistroAllowed(distro) {
			invalid = append(invalid, distro)
		}
	}
	return invalid
}

// ==================== schema replica ====================

// customInitBody is the validated POST/PUT body. Presence flags distinguish
// "key absent" from an explicit null/empty value, which the TypeScript relies on
// (`data.x !== undefined`).
type customInitBody struct {
	Name        *string
	Command     *string
	Distros     []string
	DistrosSet  bool
	Description *string
	DescSet     bool
	Enabled     *bool
}

// customInitCoerceStringArray mirrors AJV's array coercion for
// `{ type: 'array', items: { type: 'string' } }`: a scalar becomes a
// one-element array, then each item goes through ajvCoerceString.
func customInitCoerceStringArray(value any) ([]string, string) {
	var items []any
	switch typed := value.(type) {
	case []any:
		items = typed
	case string, float64, bool, nil:
		// `coerceTypes: 'array'`: string / number / boolean / null are wrapped.
		items = []any{typed}
	default:
		return nil, "must be array"
	}

	out := make([]string, 0, len(items))
	for _, item := range items {
		coerced, message := ajvCoerceString(item)
		if message != "" {
			return nil, message
		}
		out = append(out, coerced)
	}
	return out, ""
}

// customInitAJVLength mirrors AJV's `ucs2length` runtime helper, which counts
// Unicode *code points* (a surrogate pair counts as one). This is notably NOT
// String#length (UTF-16 units) — e.g. 101 emoji pass maxLength:100. The shared
// jsStringLength used by storage-configs counts UTF-16 units, so this module
// carries its own counter to stay byte-for-byte with Fastify.
func customInitAJVLength(value string) int {
	return utf8.RuneCountInString(value)
}

// customInitCoerceField is ajvCoerceField with AJV's ucs2length semantics for
// minLength/maxLength. The type coercion itself is identical, so it delegates.
func customInitCoerceField(field ajvFieldSpec, value any) (any, string) {
	var text string
	switch field.Kind {
	case ajvKindString:
		coerced, message := ajvCoerceString(value)
		if message != "" {
			return nil, message
		}
		text = coerced
		if field.MaxLength >= 0 && customInitAJVLength(text) > field.MaxLength {
			return nil, field.Name + " is too long"
		}
		if field.MinLength >= 0 && customInitAJVLength(text) < field.MinLength {
			return nil, field.Name + " is too short"
		}
		return text, ""

	case ajvKindNullableString:
		coerced, message := ajvCoerceNullableString(value)
		if message != "" {
			return nil, message
		}
		text, isString := coerced.(string)
		if !isString {
			return coerced, ""
		}
		if field.MaxLength >= 0 && customInitAJVLength(text) > field.MaxLength {
			return nil, field.Name + " is too long"
		}
		if field.MinLength >= 0 && customInitAJVLength(text) < field.MinLength {
			return nil, field.Name + " is too short"
		}
		return coerced, ""

	default:
		return ajvCoerceField(field, value)
	}
}

// customInitValidateBody mirrors the Fastify body schema. AJV runs `required`
// first (in schema order), then the properties in schema order; `distros`
// (minItems before items) sits between `command` and `description`.
func customInitValidateBody(decoded any, required []string, update bool) (customInitBody, string) {
	var out customInitBody

	object, ok := decoded.(map[string]any)
	if !ok {
		return out, "must be object"
	}

	for _, name := range required {
		if _, present := object[name]; !present {
			return out, name + " is required"
		}
	}

	// Property order is the schema's declaration order, not the document's: AJV
	// walks `properties` in order and stops at the first error (allErrors:false).
	for _, field := range []ajvFieldSpec{
		{Name: "name", Kind: ajvKindString, MinLength: 1, MaxLength: customInitNameMaxLength},
		{Name: "command", Kind: ajvKindString, MinLength: 1, MaxLength: -1},
	} {
		value, present := object[field.Name]
		if !present {
			continue
		}
		coerced, message := customInitCoerceField(field, value)
		if message != "" {
			return out, message
		}
		text, _ := coerced.(string)
		if field.Name == "name" {
			out.Name = &text
		} else {
			out.Command = &text
		}
	}

	// `distros` is validated between `command` and `description`.
	if value, present := object["distros"]; present {
		distros, message := customInitCoerceStringArray(value)
		if message != "" {
			return customInitBody{}, message
		}
		if len(distros) < 1 {
			return customInitBody{}, "must NOT have fewer than 1 items"
		}
		out.Distros = distros
		out.DistrosSet = true
	}

	trailingFields := []ajvFieldSpec{
		{Name: "description", Kind: ajvKindString, MinLength: -1, MaxLength: customInitDescriptionMaxLength},
	}
	if update {
		trailingFields = []ajvFieldSpec{
			{Name: "description", Kind: ajvKindNullableString, MinLength: -1, MaxLength: customInitDescriptionMaxLength},
			{Name: "enabled", Kind: ajvKindBoolean},
		}
	}

	for _, field := range trailingFields {
		value, present := object[field.Name]
		if !present {
			continue
		}
		coerced, message := customInitCoerceField(field, value)
		if message != "" {
			return out, message
		}
		switch field.Name {
		case "description":
			out.DescSet = true
			if text, ok := coerced.(string); ok {
				out.Description = &text
			}
		case "enabled":
			flag, _ := coerced.(bool)
			out.Enabled = &flag
		}
	}

	return out, ""
}

// customInitReadBody decodes and validates the request body. It writes the error
// response itself and reports ok=false on failure.
func customInitReadBody(c *gin.Context, update bool) (customInitBody, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return customInitBody{}, false
	}

	required := []string{"name", "command", "distros"}
	if update {
		required = nil
	}
	body, validationMessage := customInitValidateBody(decoded.Value, required, update)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return customInitBody{}, false
	}
	return body, true
}

// ==================== error mapping ====================

// customInitErrorTarget mirrors one errorMap entry.
type customInitErrorTarget struct {
	Status  int
	Message string
}

var customInitCreateErrorMap = map[string]customInitErrorTarget{
	"COMMAND_TOO_LARGE":    {Status: http.StatusBadRequest, Message: "Command content too large (max 64KB)"},
	"MAX_COMMANDS_REACHED": {Status: http.StatusBadRequest, Message: "Maximum command templates reached (50)"},
	"INVALID_DISTROS":      {Status: http.StatusBadRequest, Message: "Invalid distros"},
}

var customInitUpdateErrorMap = map[string]customInitErrorTarget{
	"NOT_FOUND":         {Status: http.StatusNotFound, Message: "Command not found"},
	"COMMAND_TOO_LARGE": {Status: http.StatusBadRequest, Message: "Command content too large (max 64KB)"},
	"INVALID_DISTROS":   {Status: http.StatusBadRequest, Message: "Invalid distros"},
}

// customInitRespondError mirrors `reply.code(errInfo.code).send({ error:
// errInfo.message })` including the `|| { code: 400, message: code }` fallback.
// Note that these responses carry no `code` field.
func customInitRespondError(c *gin.Context, targets map[string]customInitErrorTarget, code string) {
	target, ok := targets[code]
	if !ok {
		target = customInitErrorTarget{Status: http.StatusBadRequest, Message: code}
	}
	c.JSON(target.Status, gin.H{"error": target.Message})
}

// customInitRespondInvalidDistros mirrors the raw `{ error, details }` payload
// used by the distro pre-check.
func customInitRespondInvalidDistros(c *gin.Context, invalid []string) {
	c.JSON(http.StatusBadRequest, gin.H{
		"error": "Invalid distros",
		"details": gin.H{
			"invalidDistros": invalid,
			"validDistros":   customInitValidDistros,
		},
	})
}

// ==================== handlers ====================

// customInitLogID renders a numeric id the way a JS template literal does.
func customInitLogID(id int64) string {
	return strconv.FormatInt(id, 10)
}

// RegisterCustomInitCommands mounts /api/init-commands. Every route uses
// authenticateUser (admins are rejected with 403 USER_ONLY).
func RegisterCustomInitCommands(r *gin.Engine) {
	group := r.Group("/api/init-commands", middleware.AuthenticateUser())

	// GET / — the caller's templates, newest first, command bodies truncated.
	//
	// Fastify registers both "/api/init-commands" and "/api/init-commands/" for a
	// plugin root, so both spellings are mounted.
	customInitListHandler := func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		commands, err := db.GetCustomInitCommandsByUserId(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		items := make([]customInitCommandSummary, 0, len(commands))
		for _, command := range commands {
			items = append(items, customInitCommandSummary{
				ID:               command.ID,
				Name:             command.Name,
				CommandPreview:   customInitTruncateText(command.Command, customInitCommandPreviewLength),
				CommandLineCount: customInitCountCommandLines(command.Command),
				Distros:          command.Distros,
				Description:      command.Description,
				Enabled:          command.Enabled,
				CreatedAt:        command.CreatedAt,
				UpdatedAt:        command.UpdatedAt,
			})
		}
		c.JSON(http.StatusOK, gin.H{"commands": items})
	}
	group.GET("", customInitListHandler)
	group.GET("/", customInitListHandler)

	// GET /available — enabled templates matching a distro (plus the "all" ones).
	group.GET("/available", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		// Fastify validates the querystring first: `required: ['distro']`, and a
		// repeated key fails the string type check.
		values, present := c.Request.URL.Query()["distro"]
		if !present {
			respondAJVValidation(c, "distro is required")
			return
		}
		if len(values) != 1 {
			respondAJVValidation(c, "must be string")
			return
		}
		distro := values[0]

		if !customInitQueryDistroAllowed(distro) {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid distro")
			return
		}

		commands, err := db.GetEnabledCommandsByDistro(c.Request.Context(), user.ID, distro)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		items := make([]customInitAvailableCommand, 0, len(commands))
		for _, command := range commands {
			items = append(items, customInitAvailableCommand{
				ID:               command.ID,
				Name:             command.Name,
				CommandLineCount: customInitCountCommandLines(command.Command),
				Distros:          command.Distros,
				Description:      command.Description,
			})
		}
		c.JSON(http.StatusOK, gin.H{"commands": items})
	})

	// GET /distros — static list for the front end.
	//
	// Registered before /:id; Gin prefers the static segment, matching
	// find-my-way's behaviour.
	group.GET("/distros", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"distros": customInitDistroOptions})
	})

	// GET /:id — detail, owner only.
	group.GET("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		commandID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		command, err := db.GetCustomInitCommandById(c.Request.Context(), int32(commandID))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if command == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
			return
		}
		// 只能查看自己的命令
		if command.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		c.JSON(http.StatusOK, gin.H{"command": customInitCommandDetail{
			ID:          command.ID,
			Name:        command.Name,
			Command:     command.Command,
			Distros:     command.Distros,
			Description: command.Description,
			Enabled:     command.Enabled,
			CreatedAt:   command.CreatedAt,
			UpdatedAt:   command.UpdatedAt,
		}})
	})

	// POST / — create a template.
	customInitCreateHandler := func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		body, ok := customInitReadBody(c, false)
		if !ok {
			return
		}

		if invalid := customInitInvalidDistros(body.Distros); len(invalid) > 0 {
			customInitRespondInvalidDistros(c, invalid)
			return
		}

		result, err := db.CreateCustomInitCommand(ctx, db.CustomInitCommandCreateData{
			UserID:      user.ID,
			Name:        derefCustomInitString(body.Name),
			Command:     derefCustomInitString(body.Command),
			Distros:     body.Distros,
			Description: body.Description,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !result.Success {
			customInitRespondError(c, customInitCreateErrorMap, result.Error)
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, customInitLogModule, "custom_init_command.create",
			`Created init command template "`+derefCustomInitString(body.Name)+`"`,
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusCreated, gin.H{
			"message": "Init command created",
			"id":      result.ID,
		})
	}
	group.POST("", customInitCreateHandler)
	group.POST("/", customInitCreateHandler)

	// PUT /:id — partial update.
	group.PUT("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		// Fastify parses and validates the body before the handler runs, so the
		// schema check precedes the route-id check.
		body, ok := customInitReadBody(c, true)
		if !ok {
			return
		}

		commandID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		if body.DistrosSet {
			if invalid := customInitInvalidDistros(body.Distros); len(invalid) > 0 {
				customInitRespondInvalidDistros(c, invalid)
				return
			}
		}

		result, err := db.UpdateCustomInitCommand(ctx, int32(commandID), user.ID, db.CustomInitCommandUpdateData{
			Name:           body.Name,
			Command:        body.Command,
			Distros:        body.Distros,
			DistrosSet:     body.DistrosSet,
			Description:    body.Description,
			DescriptionSet: body.DescSet,
			Enabled:        body.Enabled,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !result.Success {
			customInitRespondError(c, customInitUpdateErrorMap, result.Error)
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, customInitLogModule, "custom_init_command.update",
			"Updated init command template ID "+customInitLogID(commandID),
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Init command updated"})
	})

	// DELETE /:id
	group.DELETE("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		// DELETE is a bodywith method in Fastify: a request declaring
		// Content-Type: application/json with an empty body is rejected before the
		// handler runs.
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		commandID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		deleted, err := db.DeleteCustomInitCommand(ctx, int32(commandID), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !deleted {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, customInitLogModule, "custom_init_command.delete",
			"Deleted init command template ID "+customInitLogID(commandID),
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Init command deleted"})
	})
}

// derefCustomInitString mirrors `value ?? undefined` for the required fields
// (which validation guarantees are set).
func derefCustomInitString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func init() {
	Register(RegisterCustomInitCommands)
}
