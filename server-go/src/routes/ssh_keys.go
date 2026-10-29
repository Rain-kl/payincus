// Port of server/src/routes/ssh-keys.ts.
//
// Behavioural notes that are easy to get wrong:
//
//   - Fastify validates the POST body before the handler runs, with the default
//     AJV options (allErrors:false → a single message, coerceTypes:'array' →
//     scalars stringified / single-element arrays unwrapped). `removeAdditional`
//     is on but has no effect here because the schema does not set
//     `additionalProperties:false`, so unknown keys are accepted and simply
//     ignored by the handler (verified against fastify@5.6.2). The replica lives
//     in the helpers below plus decodeJSONRequestBody / respondAJVValidation in
//     storage_configs.go.
//   - The list/detail projections deliberately return only a public-key preview
//     (first 30 / last 20 chars), never the stored key in full.
//   - The private key produced by /generate is returned once and never persisted.
package routes

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

const (
	// sshKeyMaxNameLength mirrors MAX_SSH_KEY_NAME_LENGTH.
	sshKeyMaxNameLength = 50
	// sshKeyMinPublicKeyLength mirrors MIN_SSH_PUBLIC_KEY_LENGTH.
	sshKeyMinPublicKeyLength = 20
	// sshKeyMaxPublicKeyLength mirrors MAX_SSH_PUBLIC_KEY_LENGTH.
	sshKeyMaxPublicKeyLength = 8192
	// sshKeyMaxKeysPerUser mirrors MAX_SSH_KEYS in the generate handler.
	sshKeyMaxKeysPerUser = 20
	// sshKeyRSABits mirrors the 4096-bit modulus requested from
	// crypto.generateKeyPairSync('rsa', …).
	sshKeyRSABits = 4096
)

var (
	// sshKeyLineBreakPattern mirrors /\r?\n/g.
	sshKeyLineBreakPattern = regexp.MustCompile(`\r?\n`)
	// sshKeyWhitespaceRunPattern mirrors JavaScript's /\s+/ (ECMA-262 WhiteSpace +
	// LineTerminator). Go's RE2 \s is only [\t\n\f\r ], so the class is spelled out.
	sshKeyWhitespaceRunPattern = regexp.MustCompile(jsWhitespaceClass + "+")
	// sshKeyControlCharPattern mirrors /[\u0000-\u001F\u007F]/.
	sshKeyControlCharPattern = regexp.MustCompile(`[\x00-\x1F\x7F]`)
	// sshKeyPublicKeyPattern mirrors
	// /^(ssh-rsa|ssh-ed25519|ecdsa-sha2-\S+)\s+([A-Za-z0-9+/=]+)(\s+.*)?$/, with
	// \s replaced by the JavaScript whitespace class.
	sshKeyPublicKeyPattern = regexp.MustCompile(
		"^(ssh-rsa|ssh-ed25519|ecdsa-sha2-" + jsNonWhitespaceClass + "+)" +
			jsWhitespaceClass + "+([A-Za-z0-9+/=]+)(" + jsWhitespaceClass + "+.*)?$")
)

// jsNonWhitespaceClass is the RE2 spelling of JavaScript's [^\s].
const jsNonWhitespaceClass = "[^\\t\\n\\v\\f\\r\\x20\\x{00a0}\\x{1680}\\x{2000}-\\x{200a}\\x{2028}\\x{2029}\\x{202f}\\x{205f}\\x{3000}\\x{feff}]"

// jsWhitespaceClass is the RE2 spelling of JavaScript's \s.
const jsWhitespaceClass = "[\\t\\n\\v\\f\\r\\x20\\x{00a0}\\x{1680}\\x{2000}-\\x{200a}\\x{2028}\\x{2029}\\x{202f}\\x{205f}\\x{3000}\\x{feff}]"

// sshKeyPreview is the redacted projection returned by the list/detail routes.
type sshKeyPreview struct {
	ID               int32  `json:"id"`
	Name             string `json:"name"`
	Fingerprint      string `json:"fingerprint"`
	PublicKeyPreview string `json:"publicKeyPreview"`
	CreatedAt        string `json:"createdAt"`
}

// sshKeyCreateResponse omits createdAt, exactly like the POST handler.
type sshKeyCreateResponse struct {
	ID               int32  `json:"id"`
	Name             string `json:"name"`
	Fingerprint      string `json:"fingerprint"`
	PublicKeyPreview string `json:"publicKeyPreview"`
}

// sshKeyCreateBody is the normalized POST body.
type sshKeyCreateBody struct {
	Name      string
	PublicKey string
}

// normalizeSshKeyCreateInput mirrors normalizeSshKeyCreateInput(). It returns the
// apiError envelope to send back when the input is not acceptable.
func normalizeSshKeyCreateInput(input any) (sshKeyCreateBody, *lib.APIError) {
	object, ok := input.(map[string]any)
	if !ok {
		err := lib.APIErrorFor(lib.ErrInvalidParams, "Invalid request body")
		return sshKeyCreateBody{}, &err
	}
	name, nameIsString := object["name"].(string)
	publicKey, publicKeyIsString := object["publicKey"].(string)
	if !nameIsString || !publicKeyIsString {
		err := lib.APIErrorFor(lib.ErrInvalidParams, "Invalid SSH key request")
		return sshKeyCreateBody{}, &err
	}

	nameValidation := lib.ValidateName(name, "Key name", 1, sshKeyMaxNameLength)
	if !nameValidation.Valid || nameValidation.Sanitized == nil {
		err := lib.APIErrorFor(lib.ErrInvalidParams, nameValidation.Message)
		return sshKeyCreateBody{}, &err
	}

	normalized := sshKeyWhitespaceRunPattern.ReplaceAllString(
		sshKeyLineBreakPattern.ReplaceAllString(publicKey, " "), " ")
	normalized = strings.TrimSpace(normalized)
	if jsStringLength(normalized) < sshKeyMinPublicKeyLength ||
		jsStringLength(normalized) > sshKeyMaxPublicKeyLength ||
		sshKeyControlCharPattern.MatchString(normalized) {
		err := lib.APIErrorFor(lib.ErrInvalidSshKey, "")
		return sshKeyCreateBody{}, &err
	}

	return sshKeyCreateBody{Name: *nameValidation.Sanitized, PublicKey: normalized}, nil
}

// validateSshKeyCreateBody mirrors the Fastify body schema of the POST route
// (required name+publicKey; name 1..50; publicKey minLength 20).
func validateSshKeyCreateBody(decoded any) (sshKeyCreateBody, string) {
	var out sshKeyCreateBody

	object, ok := decoded.(map[string]any)
	if !ok {
		return out, "must be object"
	}

	// `required` runs before the property checks, in schema order.
	if _, present := object["name"]; !present {
		return out, "name is required"
	}
	if _, present := object["publicKey"]; !present {
		return out, "publicKey is required"
	}

	name, message := ajvCoerceString(object["name"])
	if message != "" {
		return out, message
	}
	if jsStringLength(name) > sshKeyMaxNameLength {
		return out, "name is too long"
	}
	if jsStringLength(name) < 1 {
		return out, "name is too short"
	}
	out.Name = name

	publicKey, message := ajvCoerceString(object["publicKey"])
	if message != "" {
		return out, message
	}
	if jsStringLength(publicKey) < sshKeyMinPublicKeyLength {
		return out, "publicKey is too short"
	}
	out.PublicKey = publicKey

	return out, ""
}

// parseSshPublicKey mirrors parseSSHPublicKey().
func parseSshPublicKey(key string) (keyType, keyData, comment string, ok bool) {
	cleanKey := strings.TrimSpace(sshKeyLineBreakPattern.ReplaceAllString(key, " "))
	match := sshKeyPublicKeyPattern.FindStringSubmatch(cleanKey)
	if match == nil {
		return "", "", "", false
	}
	return match[1], match[2], strings.TrimSpace(match[3]), true
}

// nodeBase64Decode reproduces Buffer.from(value, 'base64'): invalid characters are
// skipped and decoding stops at the first '=' padding character, so the result
// (and therefore the SSH fingerprint) matches the Node implementation for the
// same key material.
func nodeBase64Decode(value string) []byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

	out := make([]byte, 0, len(value)*3/4+3)
	var accumulator uint32
	bits := 0
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch == '=' {
			break
		}
		index := strings.IndexByte(alphabet, ch)
		if index < 0 {
			continue
		}
		accumulator = accumulator<<6 | uint32(index)
		bits += 6
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(accumulator>>uint(bits)))
		}
	}
	return out
}

// calculateFingerprint mirrors calculateFingerprint(): SHA-256 over the decoded
// key blob, base64-encoded with the padding stripped, prefixed with "SHA256:".
func calculateFingerprint(base64KeyData string) string {
	digest := sha256.Sum256(nodeBase64Decode(base64KeyData))
	encoded := base64.StdEncoding.EncodeToString(digest[:])
	return "SHA256:" + strings.TrimRight(encoded, "=")
}

// truncateSshKey mirrors truncateKey(): keys shorter than 60 characters are
// returned as-is, otherwise "first30...last20".
func truncateSshKey(publicKey string) string {
	runes := []rune(publicKey)
	if len(runes) < 60 {
		return publicKey
	}
	return string(runes[:30]) + "..." + string(runes[len(runes)-20:])
}

// sshKeyPreviewOf builds the redacted list/detail projection.
func sshKeyPreviewOf(key db.SshKeyData) sshKeyPreview {
	return sshKeyPreview{
		ID:               key.ID,
		Name:             key.Name,
		Fingerprint:      key.Fingerprint,
		PublicKeyPreview: truncateSshKey(key.PublicKey),
		CreatedAt:        key.CreatedAt,
	}
}

// opensshRsaPublicKey mirrors convertRSAToOpenSSHFormat(): the ssh-rsa wire
// encoding is uint32-length-prefixed "ssh-rsa", exponent and modulus, with a 0x00
// prefix whenever the leading bit would otherwise look negative.
func opensshRsaPublicKey(publicKey *rsa.PublicKey) string {
	exponent := bigEndianUnsigned(publicKey.E)
	modulus := publicKey.N.Bytes()
	if len(modulus) > 0 && modulus[0]&0x80 != 0 {
		modulus = append([]byte{0x00}, modulus...)
	}

	keyType := []byte("ssh-rsa")
	blob := make([]byte, 0, 4+len(keyType)+4+len(exponent)+4+len(modulus))
	blob = appendUint32BE(blob, uint32(len(keyType)))
	blob = append(blob, keyType...)
	blob = appendUint32BE(blob, uint32(len(exponent)))
	blob = append(blob, exponent...)
	blob = appendUint32BE(blob, uint32(len(modulus)))
	blob = append(blob, modulus...)

	return "ssh-rsa " + base64.StdEncoding.EncodeToString(blob)
}

// bigEndianUnsigned mirrors the minimal big-endian encoding of a small integer
// (Node gets this for free from the JWK "e" field).
func bigEndianUnsigned(value int) []byte {
	buffer := make([]byte, 8)
	binary.BigEndian.PutUint64(buffer, uint64(value))
	start := 0
	for start < len(buffer) && buffer[start] == 0 {
		start++
	}
	return buffer[start:]
}

func appendUint32BE(target []byte, value uint32) []byte {
	var buffer [4]byte
	binary.BigEndian.PutUint32(buffer[:], value)
	return append(target, buffer[:]...)
}

// sshKeysListHandler mirrors `fastify.get('/', { onRequest: [fastify.authenticateUser] })`.
func sshKeysListHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	keys, err := db.GetSSHKeysByUserID(c.Request.Context(), user.ID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	items := make([]sshKeyPreview, 0, len(keys))
	for _, key := range keys {
		items = append(items, sshKeyPreviewOf(key))
	}
	c.JSON(http.StatusOK, gin.H{"keys": items})
}

// sshKeysCreateHandler mirrors `fastify.post('/', { onRequest: [fastify.authenticateUser], schema })`.
func sshKeysCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	// Fastify parses and validates the body before the handler: body-parser errors
	// and schema errors both precede any business logic.
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	_, validationMessage := validateSshKeyCreateBody(decoded.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	body, apiError := normalizeSshKeyCreateInput(decoded.Value)
	if apiError != nil {
		c.JSON(http.StatusBadRequest, apiError)
		return
	}

	parsedType, keyData, _, parsed := parseSshPublicKey(body.PublicKey)
	if !parsed || parsedType == "" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidSshKey, "")
		return
	}
	fingerprint := calculateFingerprint(keyData)

	existing, err := db.GetSSHKeysByUserID(ctx, user.ID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	for _, key := range existing {
		if key.Fingerprint == fingerprint {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrSshKeyExists, "")
			return
		}
	}

	keyID, err := db.CreateSSHKey(ctx, db.SshKeyCreateData{
		UserID:      user.ID,
		Name:        body.Name,
		PublicKey:   body.PublicKey,
		Fingerprint: fingerprint,
	})
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleSSHKey, db.LogActionSSHKeyAdd,
		"Added SSH key \""+body.Name+"\" [fingerprint: "+fingerprint+"]",
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusCreated, gin.H{
		"message": "SSH key added",
		"key": sshKeyCreateResponse{
			ID:               keyID,
			Name:             body.Name,
			Fingerprint:      fingerprint,
			PublicKeyPreview: truncateSshKey(body.PublicKey),
		},
	})
}

// RegisterSSHKeys mounts /api/ssh-keys. Every route uses authenticateUser, which
// rejects admin accounts (SSH keys belong to regular users).
func RegisterSSHKeys(r *gin.Engine) {
	group := r.Group("/api/ssh-keys", middleware.AuthenticateUser())

	// GET / — the user's keys, newest first, public keys truncated.
	//
	// Fastify's find-my-way registers both "/api/ssh-keys" and "/api/ssh-keys/" for a
	// plugin root, so the trailing-slash form is mounted too.
	group.GET("", sshKeysListHandler)
	group.GET("/", sshKeysListHandler)

	// POST / — add a public key.
	group.POST("", sshKeysCreateHandler)
	group.POST("/", sshKeysCreateHandler)

	// POST /generate — server-side RSA-4096 key pair; the private key is returned
	// once and never stored.
	group.POST("/generate", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		existing, err := db.GetSSHKeysByUserID(ctx, user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if len(existing) >= sshKeyMaxKeysPerUser {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("Maximum %d SSH keys allowed per user", sshKeyMaxKeysPerUser),
			})
			return
		}

		privateKey, err := rsa.GenerateKey(rand.Reader, sshKeyRSABits)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate valid SSH key"})
			return
		}

		sshPublicKey := opensshRsaPublicKey(&privateKey.PublicKey)
		sshPrivateKey := string(pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
		}))

		_, keyData, _, parsed := parseSshPublicKey(sshPublicKey)
		if !parsed {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate valid SSH key"})
			return
		}
		fingerprint := calculateFingerprint(keyData)

		for _, key := range existing {
			if key.Fingerprint == fingerprint {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Key already exists"})
				return
			}
		}

		// crypto.randomBytes(4).toString('base64url').slice(0, 5)
		suffixBytes := make([]byte, 4)
		if _, err := rand.Read(suffixBytes); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate valid SSH key"})
			return
		}
		suffix := base64.RawURLEncoding.EncodeToString(suffixBytes)
		if len(suffix) > 5 {
			suffix = suffix[:5]
		}
		keyName := "Incudal-" + suffix

		keyID, err := db.CreateSSHKey(ctx, db.SshKeyCreateData{
			UserID:      user.ID,
			Name:        keyName,
			PublicKey:   sshPublicKey,
			Fingerprint: fingerprint,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, db.LogModuleSSHKey, "ssh_key.generate",
			"Generated SSH key \""+keyName+"\" [fingerprint: "+fingerprint+"]",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{
			"message":    "SSH key generated",
			"privateKey": sshPrivateKey,
			"key": sshKeyCreateResponse{
				ID:               keyID,
				Name:             keyName,
				Fingerprint:      fingerprint,
				PublicKeyPreview: truncateSshKey(sshPublicKey),
			},
		})
	})

	// GET /:id — single key, owner only.
	group.GET("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		keyID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		key, err := db.GetSSHKeyByID(c.Request.Context(), int32(keyID))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if key == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
			return
		}
		// 管理员只管理系统层面，不参与用户SSH密钥操作
		if key.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		c.JSON(http.StatusOK, gin.H{"key": sshKeyPreviewOf(*key)})
	})

	// DELETE /:id — owner only.
	group.DELETE("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		// DELETE is a bodywith method in Fastify, so a request that declares
		// Content-Type: application/json with an empty body is rejected (400
		// FST_ERR_CTP_EMPTY_JSON_BODY) before the handler runs.
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		keyID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		key, err := db.GetSSHKeyByID(ctx, int32(keyID))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if key == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
			return
		}
		if key.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		if err := db.DeleteSSHKey(ctx, int32(keyID)); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, db.LogModuleSSHKey, db.LogActionSSHKeyDelete,
			"Deleted SSH key \""+key.Name+"\" [fingerprint: "+key.Fingerprint+"]",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "SSH key deleted"})
	})
}

func init() {
	Register(RegisterSSHKeys)
}
