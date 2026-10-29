// Port of server/src/routes/terminal-saved-commands.ts.
//
// The TypeScript routes declare Fastify JSON body schemas. Fastify's AJV defaults
// are load-bearing for the wire format (see @fastify/ajv-compiler's
// default-ajv-options.js): allErrors:false → a single message, removeAdditional:true
// → unknown keys are silently dropped instead of rejected, coerceTypes:'array' →
// numbers/booleans are stringified and single-element arrays unwrap. The
// validation below reproduces that behaviour plus the message mapping done by
// setErrorHandler in app.ts, because those messages are part of the contract.
package routes

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// Body schema bounds, mirroring the POST/PUT schema in the route module.
const (
	terminalCommandNameMaxLength        = 80
	terminalCommandContentMaxLength     = 16384
	terminalCommandDescriptionMaxLength = 300
)

// Fastify's own body-parser errors, surfaced verbatim by the generic branch of
// setErrorHandler.
const (
	terminalCommandEmptyBodyMessage   = "Body cannot be empty when content-type is set to 'application/json'"
	terminalCommandInvalidJSONMessage = "Body is not valid JSON but content-type is set to 'application/json'"
)

// terminalCommandBody is the validated body: a nil pointer means "key absent",
// HasDescription distinguishes an absent description from an explicit null.
type terminalCommandBody struct {
	Name           *string
	Command        *string
	Description    *string
	HasDescription bool
}

// readTerminalCommandBody decodes the raw JSON body. The second return value is a
// Fastify body-parser message when the body could not be read at all.
func readTerminalCommandBody(c *gin.Context) (any, string) {
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, terminalCommandInvalidJSONMessage
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, terminalCommandEmptyBodyMessage
	}

	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, terminalCommandInvalidJSONMessage
	}
	return decoded, ""
}

// coerceTerminalCommandString mirrors AJV's `coerceTypes: 'array'` coercion for a
// string-typed property.
func coerceTerminalCommandString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case nil:
		// AJV coerces null to "" for type:'string'; minLength then rejects it.
		return "", true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		return formatTerminalCommandNumber(typed), true
	case []any:
		if len(typed) == 1 {
			return coerceTerminalCommandString(typed[0])
		}
		return "", false
	default:
		return "", false
	}
}

// formatTerminalCommandNumber mirrors JavaScript's String(number) coercion.
func formatTerminalCommandNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// validateTerminalCommandBody mirrors the route schemas. It returns the first
// (and, because AJV runs with allErrors:false, the only) validation message.
func validateTerminalCommandBody(decoded any, requireNameAndCommand bool) (terminalCommandBody, string) {
	var out terminalCommandBody

	object, ok := decoded.(map[string]any)
	if !ok {
		return out, "must be object"
	}

	// `required` is checked before the properties, in schema order.
	if requireNameAndCommand {
		if _, present := object["name"]; !present {
			return out, "name is required"
		}
		if _, present := object["command"]; !present {
			return out, "command is required"
		}
	}

	if value, present := object["name"]; present {
		coerced, ok := coerceTerminalCommandString(value)
		if !ok {
			return out, "must be string"
		}
		if utf8.RuneCountInString(coerced) > terminalCommandNameMaxLength {
			return out, "name is too long"
		}
		if utf8.RuneCountInString(coerced) < 1 {
			return out, "name is too short"
		}
		out.Name = &coerced
	}

	if value, present := object["command"]; present {
		coerced, ok := coerceTerminalCommandString(value)
		if !ok {
			return out, "must be string"
		}
		if utf8.RuneCountInString(coerced) > terminalCommandContentMaxLength {
			return out, "command is too long"
		}
		if utf8.RuneCountInString(coerced) < 1 {
			return out, "command is too short"
		}
		out.Command = &coerced
	}

	if value, present := object["description"]; present {
		out.HasDescription = true
		if value != nil {
			coerced, ok := coerceTerminalCommandString(value)
			if !ok {
				return out, "must be string,null"
			}
			if utf8.RuneCountInString(coerced) > terminalCommandDescriptionMaxLength {
				return out, "description is too long"
			}
			out.Description = &coerced
		}
	}

	return out, ""
}

// respondTerminalCommandValidation mirrors the validation branch of
// setErrorHandler: a single message (allErrors is off), so `details` is never set.
func respondTerminalCommandValidation(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": "VALIDATION_ERROR"})
}

// terminalCommandErrorTarget mirrors the errorMap entries of the route module.
type terminalCommandErrorTarget struct {
	Status  int
	Message string
}

var terminalCommandErrorMap = map[string]terminalCommandErrorTarget{
	"INVALID_NAME":         {Status: http.StatusBadRequest, Message: "Command name is required"},
	"INVALID_COMMAND":      {Status: http.StatusBadRequest, Message: "Command content is required"},
	"COMMAND_TOO_LARGE":    {Status: http.StatusBadRequest, Message: "Command content too large (max 16KB)"},
	"MAX_COMMANDS_REACHED": {Status: http.StatusBadRequest, Message: "Maximum saved terminal commands reached (100)"},
	"NOT_FOUND":            {Status: http.StatusNotFound, Message: "Saved command not found"},
}

// respondTerminalCommandError mirrors `reply.code(status).send({ error, code })`
// including the `errorMap[code] || { status: 400, message: code }` fallback.
func respondTerminalCommandError(c *gin.Context, code string) {
	target, ok := terminalCommandErrorMap[code]
	if !ok {
		target = terminalCommandErrorTarget{Status: http.StatusBadRequest, Message: code}
	}
	c.JSON(target.Status, gin.H{"error": target.Message, "code": code})
}

func derefTerminalCommandString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// RegisterTerminalSavedCommands mounts /api/terminal-saved-commands. Every route
// requires authentication, mirroring the plugin-level onRequest hook.
func RegisterTerminalSavedCommands(r *gin.Engine) {
	group := r.Group("/api/terminal-saved-commands", middleware.Authenticate())

	group.GET("", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		commands, err := db.GetTerminalSavedCommandsByUserId(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"commands": commands})
	})

	group.POST("", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		decoded, decodeError := readTerminalCommandBody(c)
		if decodeError != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": decodeError})
			return
		}

		body, message := validateTerminalCommandBody(decoded, true)
		if message != "" {
			respondTerminalCommandValidation(c, message)
			return
		}

		result, err := db.CreateTerminalSavedCommand(c.Request.Context(), db.TerminalSavedCommandCreateData{
			UserID:      user.ID,
			Name:        derefTerminalCommandString(body.Name),
			Command:     derefTerminalCommandString(body.Command),
			Description: body.Description,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !result.Success {
			respondTerminalCommandError(c, result.Error)
			return
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, "terminal", "terminal.saved_command.create",
			"Created terminal saved command \""+result.Command.Name+"\"",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusCreated, gin.H{
			"message": "Saved terminal command created",
			"command": result.Command,
		})
	})

	group.PUT("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		// Fastify validates the body before the handler runs, so the schema
		// check precedes the route-id check here as well.
		decoded, decodeError := readTerminalCommandBody(c)
		if decodeError != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": decodeError})
			return
		}

		body, message := validateTerminalCommandBody(decoded, false)
		if message != "" {
			respondTerminalCommandValidation(c, message)
			return
		}

		commandID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		result, err := db.UpdateTerminalSavedCommand(c.Request.Context(), int32(commandID), user.ID, db.TerminalSavedCommandUpdateData{
			NameProvided:        body.Name != nil,
			Name:                derefTerminalCommandString(body.Name),
			CommandProvided:     body.Command != nil,
			Command:             derefTerminalCommandString(body.Command),
			DescriptionProvided: body.HasDescription,
			Description:         body.Description,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !result.Success {
			respondTerminalCommandError(c, result.Error)
			return
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, "terminal", "terminal.saved_command.update",
			"Updated terminal saved command \""+result.Command.Name+"\" (#"+strconv.FormatInt(commandID, 10)+")",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{
			"message": "Saved terminal command updated",
			"command": result.Command,
		})
	})

	group.DELETE("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		commandID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		deleted, err := db.DeleteTerminalSavedCommand(c.Request.Context(), int32(commandID), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !deleted {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
			return
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, "terminal", "terminal.saved_command.delete",
			"Deleted terminal saved command #"+strconv.FormatInt(commandID, 10),
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Saved terminal command deleted"})
	})
}

func init() {
	Register(RegisterTerminalSavedCommands)
}
