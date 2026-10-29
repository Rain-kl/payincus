// Port of server/src/routes/verification.ts.
//
// Sensitive-operation second-factor verification. app.ts registers this module as
// `fastify.register(verificationRoutes, { prefix: '/api/verification' })`, so the
// paths below are relative to that prefix. Every route requires authentication
// (`onRequest: [fastify.authenticate]`).
//
// The verification state machine itself lives in lib/operation_verification.go
// (RequestOperationVerification / VerifyOperationCode / IsOperationVerified /
// IsResourceVerificationRequired) and is reused verbatim. This file only
// reproduces the Fastify request pipeline:
//
//   - JSON-schema validation with @fastify/ajv-compiler's defaults
//     (coerceTypes:'array', useDefaults:true, allErrors:false). Only the first
//     error is reported and the message comes from the setErrorHandler branches
//     in app.ts (`X is required`, `X is too short`, `must be integer`, ...).
//   - the module-local error payloads, which are plain `{error, code}` objects
//     rather than lib.ErrXxx entries (so no `details` is ever attached):
//     INVALID_RESOURCE_ID and the VERIFICATION_FAILED / INVALID_CODE fallbacks.
//     On failure the `code` is whatever lib returned in result.errorCode, which
//     is exactly what the TypeScript sends.
package routes

import (
	"math"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// verificationRouteErrorCode values are literals in the TypeScript (they are not
// part of lib/errors.ts), so they stay plain strings to keep byte-identical
// payloads without polluting the generated catalog.
const (
	verificationInvalidResourceIDCode = "INVALID_RESOURCE_ID"
	verificationFailedCode            = "VERIFICATION_FAILED"
	verificationInvalidCodeCode       = "INVALID_CODE"

	verificationResourceRequiredMessage   = "Resource ID is required for this operation"
	verificationResourceNotAllowedMessage = "Resource ID is not allowed for this operation"
	verificationNotRequiredMessage        = "No notification channel configured, verification not required"
	verificationAlreadySentMessage        = "Verification code already sent"
	verificationFailedMessage             = "Verification failed"
	verificationSuccessMessage            = "Verification successful"
)

// validOperationTypes mirrors VALID_OPERATION_TYPES, in order: both the schema
// enum and the /operation-types payload depend on this ordering.
var validOperationTypes = []models.OperationType{
	models.OperationTypeChangePassword,
	models.OperationTypeDisable2fa,
	models.OperationTypeChangeEmail,
	models.OperationTypeDeleteAccount,
	models.OperationTypeDeleteInstance,
	models.OperationTypeReinstallInstance,
	models.OperationTypeRecreateInstance,
	models.OperationTypeTransferInstance,
	models.OperationTypeDeleteSnapshot,
	models.OperationTypeDeleteBackup,
}

var (
	validOperationTypeNames = func() []string {
		names := make([]string, 0, len(validOperationTypes))
		for _, operationType := range validOperationTypes {
			names = append(names, string(operationType))
		}
		return names
	}()

	validOperationTypeSet = func() map[string]models.OperationType {
		set := make(map[string]models.OperationType, len(validOperationTypes))
		for _, operationType := range validOperationTypes {
			set[string(operationType)] = operationType
		}
		return set
	}()

	// verificationRequestBodyFields is the POST /request schema.
	verificationRequestBodyFields = []ajvFieldSpec{
		{Name: "operationType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN(), Enum: validOperationTypeNames},
		{Name: "resourceId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
		{Name: "resourceType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	}

	// verificationVerifyBodyFields is the POST /verify schema.
	verificationVerifyBodyFields = []ajvFieldSpec{
		{Name: "operationType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN(), Enum: validOperationTypeNames},
		{Name: "code", Kind: ajvKindString, MinLength: 6, MaxLength: 6, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "resourceId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
	}
)

// verificationQueryValue reproduces Fastify's querystring shape (fast-querystring):
// a repeated key becomes an array of strings, a single key stays a string, and an
// absent key is undefined. AJV's single-element array unwrapping then decides
// whether the value is usable.
func verificationQueryValue(query url.Values, key string) (any, bool) {
	values, present := query[key]
	if !present {
		return nil, false
	}
	if len(values) == 1 {
		return values[0], true
	}
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	return items, true
}

// normalizeVerificationResourceID mirrors normalizeResourceIdForOperation: for
// resource operations the id is mandatory and must be a strict positive safe
// integer (a string arrives from the querystring, a number from the JSON body);
// for account operations it must be absent.
func normalizeVerificationResourceID(operationType models.OperationType, value any, present bool) (int64, bool) {
	if lib.IsResourceOperation(operationType) {
		if !present {
			return 0, false
		}
		switch typed := value.(type) {
		case string:
			return lib.ParsePositiveRouteID(typed)
		case float64:
			return verificationPositiveSafeInteger(typed)
		}
		return 0, false
	}

	if present {
		return 0, false
	}
	return 0, true
}

// verificationPositiveSafeInteger mirrors parsePositiveResourceIdNumber.
func verificationPositiveSafeInteger(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value <= 0 || value > 1<<53-1 {
		return 0, false
	}
	return int64(value), true
}

// respondVerificationInvalidResource mirrors the INVALID_RESOURCE_ID payloads.
func respondVerificationInvalidResource(c *gin.Context, operationType models.OperationType) {
	message := verificationResourceNotAllowedMessage
	if lib.IsResourceOperation(operationType) {
		message = verificationResourceRequiredMessage
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": verificationInvalidResourceIDCode})
}

// respondVerificationInternal is the generic 500 of setErrorHandler.
func respondVerificationInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// verificationOperationTypeItem mirrors one entry of accountOperations /
// resourceOperations.
type verificationOperationTypeItem struct {
	Type    models.OperationType `json:"type"`
	Name    string               `json:"name"`
	NameEn  string               `json:"nameEn"`
	Channel string               `json:"channel"`
}

// RegisterVerification mounts /api/verification.
func RegisterVerification(r *gin.Engine) {
	group := r.Group("/api/verification", middleware.Authenticate())

	// 请求二次验证码
	group.POST("/request", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		decoded, status, parseMessage := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": parseMessage})
			return
		}
		body, validationMessage := validateAJVObject(unwrapAJVRootArray(decoded.Value), []string{"operationType"}, verificationRequestBodyFields)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		rawOperationType, _ := body.stringValue("operationType")
		operationType := validOperationTypeSet[rawOperationType]
		resourceType, _ := body.stringValue("resourceType")

		resourceID, valid := normalizeVerificationResourceID(operationType, body.Values["resourceId"], body.Present["resourceId"])
		if !valid {
			respondVerificationInvalidResource(c, operationType)
			return
		}

		// 资源操作：用户没有绑定通知渠道时不需要二次验证
		if lib.IsResourceOperation(operationType) {
			required, err := lib.IsResourceVerificationRequired(ctx, int64(user.ID))
			if err != nil {
				respondVerificationInternal(c)
				return
			}
			if !required {
				c.JSON(http.StatusOK, gin.H{
					"success":  true,
					"required": false,
					"message":  verificationNotRequiredMessage,
				})
				return
			}
		}

		result, err := lib.RequestOperationVerification(ctx, int64(user.ID), operationType, resourceID, resourceType)
		if err != nil {
			respondVerificationInternal(c)
			return
		}

		if !result.Success {
			// 特殊处理：已发送过验证码。lib 在 already-sent 时仍返回 success=true，
			// 因此该分支与 TypeScript 一样实际不可达，保留以逐行对齐。
			if result.ExpiresIn != 0 {
				c.JSON(http.StatusOK, gin.H{
					"success":      true,
					"channel":      result.Channel,
					"maskedTarget": result.MaskedTarget,
					"expiresIn":    result.ExpiresIn,
					"message":      verificationAlreadySentMessage,
				})
				return
			}

			code := result.ErrorCode
			if code == "" {
				code = verificationFailedCode
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": result.Error, "code": code})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"required":      true,
			"channel":       result.Channel,
			"maskedTarget":  result.MaskedTarget,
			"expiresIn":     result.ExpiresIn,
			"operationName": lib.GetOperationName(operationType, "zh"),
		})
	})

	// 验证二次验证码
	group.POST("/verify", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		decoded, status, parseMessage := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": parseMessage})
			return
		}
		body, validationMessage := validateAJVObject(unwrapAJVRootArray(decoded.Value), []string{"operationType", "code"}, verificationVerifyBodyFields)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		rawOperationType, _ := body.stringValue("operationType")
		operationType := validOperationTypeSet[rawOperationType]
		code, _ := body.stringValue("code")

		resourceID, valid := normalizeVerificationResourceID(operationType, body.Values["resourceId"], body.Present["resourceId"])
		if !valid {
			respondVerificationInvalidResource(c, operationType)
			return
		}

		result, err := lib.VerifyOperationCode(ctx, int64(user.ID), operationType, code, resourceID)
		if err != nil {
			respondVerificationInternal(c)
			return
		}

		if !result.Success || !result.Verified {
			message := result.Error
			if message == "" {
				message = verificationFailedMessage
			}
			resultCode := result.ErrorCode
			if resultCode == "" {
				resultCode = verificationInvalidCodeCode
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": resultCode})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"success":  true,
			"verified": true,
			"message":  verificationSuccessMessage,
		})
	})

	// 检查操作是否已通过验证
	group.GET("/check", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()
		query := c.Request.URL.Query()

		// querystring schema: required operationType (enum), optional resourceId (string).
		rawOperationType, present := verificationQueryValue(query, "operationType")
		if !present {
			respondAJVValidation(c, "operationType is required")
			return
		}
		operationTypeName, message := ajvCoerceString(rawOperationType)
		if message != "" {
			respondAJVValidation(c, message)
			return
		}
		operationType, known := validOperationTypeSet[operationTypeName]
		if !known {
			respondAJVValidation(c, "must be equal to one of the allowed values")
			return
		}

		var resourceIDValue any
		resourceIDPresent := false
		if rawResourceID, present := verificationQueryValue(query, "resourceId"); present {
			text, message := ajvCoerceString(rawResourceID)
			if message != "" {
				respondAJVValidation(c, message)
				return
			}
			resourceIDValue = text
			resourceIDPresent = true
		}

		resourceID, valid := normalizeVerificationResourceID(operationType, resourceIDValue, resourceIDPresent)
		if !valid {
			respondVerificationInvalidResource(c, operationType)
			return
		}

		// 资源操作：没有绑定通知渠道时视为已验证且不需要验证
		required := false
		if lib.IsResourceOperation(operationType) {
			resourceRequired, err := lib.IsResourceVerificationRequired(ctx, int64(user.ID))
			if err != nil {
				respondVerificationInternal(c)
				return
			}
			if !resourceRequired {
				c.JSON(http.StatusOK, gin.H{"required": false, "verified": true})
				return
			}
			required = true
		}

		verified, err := lib.IsOperationVerified(ctx, int64(user.ID), operationType, resourceID)
		if err != nil {
			respondVerificationInternal(c)
			return
		}

		// `isAccountOperation(op) || await isResourceVerificationRequired(userId)`:
		// the short-circuit means an account operation never queries again.
		if !required {
			if lib.IsAccountOperation(operationType) {
				required = true
			} else {
				resourceRequired, err := lib.IsResourceVerificationRequired(ctx, int64(user.ID))
				if err != nil {
					respondVerificationInternal(c)
					return
				}
				required = resourceRequired
			}
		}

		c.JSON(http.StatusOK, gin.H{"required": required, "verified": verified})
	})

	// 获取操作类型信息
	group.GET("/operation-types", func(c *gin.Context) {
		accountOperations := make([]verificationOperationTypeItem, 0, len(validOperationTypes))
		resourceOperations := make([]verificationOperationTypeItem, 0, len(validOperationTypes))
		for _, operationType := range validOperationTypes {
			item := verificationOperationTypeItem{
				Type:   operationType,
				Name:   lib.GetOperationName(operationType, "zh"),
				NameEn: lib.GetOperationName(operationType, "en"),
			}
			switch {
			case lib.IsAccountOperation(operationType):
				item.Channel = "email"
				accountOperations = append(accountOperations, item)
			case lib.IsResourceOperation(operationType):
				item.Channel = "notification"
				resourceOperations = append(resourceOperations, item)
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"accountOperations":  accountOperations,
			"resourceOperations": resourceOperations,
		})
	})
}

func init() {
	Register(RegisterVerification)
}
