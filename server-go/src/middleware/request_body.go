// Fastify request-body lifecycle parity.
//
// Fastify parses the request body before the route handler runs, which the Go
// handlers cannot see: they only touch the body when they happen to need it. The
// dual-run conformance harness caught this — Node returns 400 for a DELETE that
// carries `Content-Type: application/json` with an empty body, while a handler
// that never reads the body continues happily.
//
// Measured against fastify@5 on the live Node backend. Ordering first, because
// it decides where this middleware may be mounted:
//
//  1. Route matching   — an unmatched route still parses the body (see below)
//  2. onRequest hooks  — authentication. A request that fails auth never reaches
//     the parser: every unauthenticated POST/DELETE to an
//     auth-protected route returns 401 even when the JSON
//     body is empty or malformed, and even for a wrong
//     Content-Type.
//  3. Body parsing     — the failures below
//  4. preValidation / preHandler / handler / schema validation
//
// So the body checks must run AFTER authentication. Mounting them globally before
// auth would turn a 401 into a 400 and leak "this route exists and wants a body"
// to unauthenticated callers.
//
// Body-parser behaviour for POST/PUT/PATCH/DELETE (GET/HEAD/OPTIONS are bodyless
// and none of this applies):
//
//	Content-Type: application/json, body empty        → 400 "Body cannot be empty…"
//	Content-Type: application/json, body not JSON     → 400 "Body is not valid JSON…"
//	Content-Type: application/json, body valid JSON   → continue (null/0/"" are fine)
//	Content-Type: text/plain                          → continue (kept as a string)
//	Content-Type: multipart/form-data                 → continue (@fastify/multipart)
//	Content-Type: anything else                       → 415 "Unsupported Media Type: <ct>"
//	no Content-Type + a body                          → 415 "Unsupported Media Type: undefined"
//	no Content-Type + no body                         → continue
//	body larger than BODY_LIMIT (10 MiB default)      → 413 "Request body is too large"
//	unmatched route + JSON content type               → the same 400s as above
//	unmatched route + any other content type          → 404 (not 415)
//
// The unmatched-route row is why the no-route handler must also run the JSON
// checks; see app.go's NoRoute.
package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Fastify body-parse failure messages, byte-identical to fastify@5.
const (
	FastifyEmptyBodyMessage      = "Body cannot be empty when content-type is set to 'application/json'"
	FastifyInvalidBodyMessage    = "Body is not valid JSON but content-type is set to 'application/json'"
	FastifyUndefinedMediaMessage = "Unsupported Media Type: undefined"
	FastifyTooLargeMessage       = "Request body is too large"
)

// defaultBodyLimit mirrors `bodyLimit: parseInt(process.env.BODY_LIMIT || '10485760')`.
const defaultBodyLimit = 10485760

// bodyParsingMethods mirrors @fastify/body's `bodyless` rule: GET, HEAD, OPTIONS
// and TRACE never have their body parsed.
var bodyParsingMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// bodyLimitBytes resolves the configured limit per request so an operator can
// change BODY_LIMIT without a restart.
func bodyLimitBytes() int64 {
	if raw := os.Getenv("BODY_LIMIT"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultBodyLimit
}

// mediaTypeMatches compares the declared media type ignoring parameters
// (`; charset=utf-8`) and case, like Fastify's media-type lookup.
func mediaTypeMatches(contentType, expected string) bool {
	if semicolon := strings.IndexByte(contentType, ';'); semicolon >= 0 {
		contentType = contentType[:semicolon]
	}
	return strings.EqualFold(strings.TrimSpace(contentType), expected)
}

// ValidateFastifyBody applies the body-parser rules and returns (status, message)
// when the request must be rejected. status 0 means "continue".
//
// It buffers a valid JSON body back onto the request so downstream handlers can
// decode it again.
func ValidateFastifyBody(c *gin.Context) (int, string) {
	if !bodyParsingMethods[c.Request.Method] {
		return 0, ""
	}

	contentType := c.GetHeader("Content-Type")
	hasBody := c.Request.ContentLength > 0 || len(c.Request.TransferEncoding) > 0

	if contentType == "" {
		if hasBody {
			return http.StatusUnsupportedMediaType, FastifyUndefinedMediaMessage
		}
		return 0, ""
	}

	switch {
	case mediaTypeMatches(contentType, "application/json"):
		if !hasBody {
			return http.StatusBadRequest, FastifyEmptyBodyMessage
		}

		limited := http.MaxBytesReader(c.Writer, c.Request.Body, bodyLimitBytes())
		payload, err := io.ReadAll(limited)
		if err != nil {
			// MaxBytesReader reports the limit by failing the read. A length above
			// the cap is Fastify's 413; any other read error is treated the same,
			// because Node would also have aborted the parse.
			return http.StatusRequestEntityTooLarge, FastifyTooLargeMessage
		}
		if len(bytes.TrimSpace(payload)) == 0 {
			return http.StatusBadRequest, FastifyEmptyBodyMessage
		}

		var decoded any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return http.StatusBadRequest, FastifyInvalidBodyMessage
		}
		if hasForbiddenJSONKey(decoded) {
			// secure-json-parse runs with protoAction/constructorAction 'error'.
			return http.StatusBadRequest, FastifyInvalidBodyMessage
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(payload))
		c.Request.ContentLength = int64(len(payload))
		return 0, ""

	case mediaTypeMatches(contentType, "text/plain"):
		return 0, ""

	case mediaTypeMatches(contentType, "multipart/form-data"):
		return 0, ""

	default:
		return http.StatusUnsupportedMediaType, "Unsupported Media Type: " + contentType
	}
}

// AbortFastifyBody writes the body-parser error envelope. These come from the
// parser rather than apiError(), so they carry only `error` and no `code`.
func AbortFastifyBody(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": message})
}

// RequestBody applies ValidateFastifyBody as middleware. Mount it AFTER the auth
// middleware on protected groups (see the ordering note at the top of the file).
func RequestBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		if status, message := ValidateFastifyBody(c); status != 0 {
			AbortFastifyBody(c, status, message)
			return
		}
		c.Next()
	}
}

// hasForbiddenJSONKey mirrors secure-json-parse's default proto/constructor
// rejection for a decoded document.
func hasForbiddenJSONKey(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key == "__proto__" || key == "constructor" {
				return true
			}
			if hasForbiddenJSONKey(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if hasForbiddenJSONKey(item) {
				return true
			}
		}
	}
	return false
}
