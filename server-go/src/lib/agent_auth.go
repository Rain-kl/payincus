// Port of server/src/lib/agent-auth.ts.
//
// This is the host-agent authentication surface (WAVE_D_SPEC §0.2, third of the
// three mutually exclusive credential planes). It is deliberately NOT a JWT and
// NOT a public-API token:
//
//	X-Incus-Agent-Id      agt_…
//	X-Incus-Timestamp     13-digit epoch milliseconds
//	X-Incus-Nonce         caller-chosen, 8..128 chars, unique per agent
//	X-Incus-Body-Sha256   sha256 hex of the stable-stringified JSON body
//	X-Incus-Signature     hex HMAC-SHA256(secret, signing payload)
//
// The signing payload is built byte-for-byte like the TypeScript:
//
//	METHOD \n PATH \n TIMESTAMP \n NONCE \n lowercase(bodyHash)
//
// METHOD is uppercased and PATH is the request URL without its query string.
// There is no newline at the end, and the fields are never re-ordered.
//
// Web JWTs and pat_ tokens can never satisfy this: without the shared agent
// secret an attacker cannot produce a valid signature, and no route below ever
// consults the JWT verifier (routes/agent.go_test asserts the reverse).
package lib

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Agent timestamp/nonce lifetimes mirror the TypeScript constants.
const (
	// AgentTimestampWindowMs is the accepted clock skew (±5 minutes).
	AgentTimestampWindowMs int64 = 5 * 60 * 1000
	// AgentNonceTTLMs is how long a used nonce blocks a replay (10 minutes).
	AgentNonceTTLMs int64 = 10 * 60 * 1000
)

var (
	agentIDPattern     = regexp.MustCompile(`^agt_[A-Za-z0-9_-]{24,64}$`)
	agentSecretPattern = regexp.MustCompile(`^ias_[A-Za-z0-9_-]{32,96}$`)
	agentNoncePattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	agentSHA256Pattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	agentTimestampForm = regexp.MustCompile(`^\d{13}$`)
)

// AgentAuthHeaders mirrors the AgentAuthHeaders interface.
type AgentAuthHeaders struct {
	AgentID   string
	Timestamp string
	Nonce     string
	BodyHash  string
	Signature string
}

// AgentSigningPayload mirrors AgentSigningPayload.
type AgentSigningPayload struct {
	Method    string
	Path      string
	Timestamp string
	Nonce     string
	BodyHash  string
}

// GenerateAgentID mirrors generateAgentId: `agt_` + base64url(24 random bytes).
func GenerateAgentID() string {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return "agt_" + base64.RawURLEncoding.EncodeToString(buffer)
}

// GenerateAgentSecret mirrors generateAgentSecret: `ias_` + base64url(32 bytes).
func GenerateAgentSecret() string {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return "ias_" + base64.RawURLEncoding.EncodeToString(buffer)
}

// IsValidAgentID mirrors isValidAgentId.
func IsValidAgentID(value string) bool { return agentIDPattern.MatchString(value) }

// IsValidAgentSecret mirrors isValidAgentSecret.
func IsValidAgentSecret(value string) bool { return agentSecretPattern.MatchString(value) }

// HashAgentSecret mirrors hashAgentSecret (sha256 hex, lowercase).
func HashAgentSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// agentIsArrayIndex reports whether key is a JavaScript array-index property
// name: a canonical decimal string in [0, 2^32-2] ("0", "42", not "01", not
// "-1"). JSON.stringify emits these keys first, in ascending numeric order,
// before every other key, because that is how Object.keys orders them.
func agentIsArrayIndex(key string) (uint64, bool) {
	if key == "" || len(key) > 10 {
		return 0, false
	}
	if key != "0" && key[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < '0' || key[i] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(key, 10, 64)
	if err != nil || value > 4294967294 {
		return 0, false
	}
	return value, true
}

// agentOrderedKeys mirrors the ordering JSON.stringify gives to an object whose
// keys were inserted in sorted order: array indices ascending first, then the
// remaining keys in their (already sorted) insertion order.
func agentOrderedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	indexes := make([]struct {
		key   string
		value uint64
	}, 0, len(object))
	others := make([]string, 0, len(object))
	for key := range object {
		if value, ok := agentIsArrayIndex(key); ok {
			indexes = append(indexes, struct {
				key   string
				value uint64
			}{key: key, value: value})
			continue
		}
		others = append(others, key)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].value < indexes[j].value })
	sort.Strings(others)
	for _, entry := range indexes {
		keys = append(keys, entry.key)
	}
	keys = append(keys, others...)
	return keys
}

// AgentAppendJSONString appends value as JSON.stringify would spell it.
//
// encoding/json cannot be used here: it escapes `<`, `>`, `&` and U+2028/U+2029
// by default, and its number formatting differs from ECMAScript (1e+06 vs
// 1000000). Byte equality with the TypeScript body hash depends on both.
func agentAppendJSONString(dst []byte, value string) []byte {
	dst = append(dst, '"')
	for _, runeValue := range value {
		switch runeValue {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\r':
			dst = append(dst, '\\', 'r')
		default:
			if runeValue < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0',
					"0123456789abcdef"[runeValue>>4], "0123456789abcdef"[runeValue&0xf])
				continue
			}
			dst = append(dst, string(runeValue)...)
		}
	}
	return append(dst, '"')
}

// agentAppendStableJSON mirrors stableStringifyAgentBody's recursive walk:
// object keys are sorted (array indices first, see agentOrderedKeys), arrays keep
// their order, and JS `undefined` never appears because decoded JSON has no such
// value.
func agentAppendStableJSON(dst []byte, value any) []byte {
	switch typed := value.(type) {
	case nil:
		return append(dst, "null"...)
	case bool:
		if typed {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case string:
		return agentAppendJSONString(dst, typed)
	case float64:
		// Number::toString is what JSON.stringify uses for numbers. NaN and ±Inf
		// cannot be decoded from JSON; they would become null, handled below.
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return append(dst, "null"...)
		}
		return append(dst, jsNumberToString(typed)...)
	case float32:
		return append(dst, jsNumberToString(float64(typed))...)
	case int:
		return append(dst, jsNumberToString(float64(typed))...)
	case int64:
		return append(dst, jsNumberToString(float64(typed))...)
	case int32:
		return append(dst, jsNumberToString(float64(typed))...)
	case uint64:
		return append(dst, jsNumberToString(float64(typed))...)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return append(dst, "null"...)
		}
		return append(dst, jsNumberToString(parsed)...)
	case []any:
		dst = append(dst, '[')
		for index, item := range typed {
			if index > 0 {
				dst = append(dst, ',')
			}
			dst = agentAppendStableJSON(dst, item)
		}
		return append(dst, ']')
	case map[string]any:
		dst = append(dst, '{')
		for index, key := range agentOrderedKeys(typed) {
			if index > 0 {
				dst = append(dst, ',')
			}
			dst = agentAppendJSONString(dst, key)
			dst = append(dst, ':')
			dst = agentAppendStableJSON(dst, typed[key])
		}
		return append(dst, '}')
	default:
		// A caller handed us a struct/slice that did not come from JSON decoding.
		// Round-trip it through encoding/json so the shape still matches the
		// TypeScript (every agent payload is decoded JSON, so this is defensive).
		encoded, err := json.Marshal(value)
		if err != nil {
			return append(dst, "null"...)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return append(dst, "null"...)
		}
		return agentAppendStableJSON(dst, decoded)
	}
}

// StableStringifyAgentBody mirrors stableStringifyAgentBody.
func StableStringifyAgentBody(body any) string {
	return string(agentAppendStableJSON(nil, body))
}

// CreateAgentBodyHash mirrors createAgentBodyHash.
func CreateAgentBodyHash(body any) string {
	sum := sha256.Sum256([]byte(StableStringifyAgentBody(body)))
	return hex.EncodeToString(sum[:])
}

// BuildAgentSigningPayload mirrors buildAgentSigningPayload.
func BuildAgentSigningPayload(payload AgentSigningPayload) string {
	return strings.ToUpper(payload.Method) + "\n" +
		payload.Path + "\n" +
		payload.Timestamp + "\n" +
		payload.Nonce + "\n" +
		strings.ToLower(payload.BodyHash)
}

// CreateAgentSignature mirrors createAgentSignature.
func CreateAgentSignature(secret string, payload AgentSigningPayload) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(BuildAgentSigningPayload(payload)))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyAgentSignature mirrors verifyAgentSignature. The TypeScript rejects a
// malformed signature before computing the HMAC (so a 63-char value costs no
// work) and then compares in constant time.
func VerifyAgentSignature(secret string, payload AgentSigningPayload, signature string) bool {
	if !agentSHA256Pattern.MatchString(signature) {
		return false
	}
	expected := CreateAgentSignature(secret, payload)
	actual := strings.ToLower(signature)
	if len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

// agentHeaderFirstValue mirrors getHeaderValue: the first value, or nil.
func agentHeaderFirstValue(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// ReadAgentAuthHeaders mirrors readAgentAuthHeaders. A missing header yields
// nil, which the route renders as AGENT_AUTH_REQUIRED.
func ReadAgentAuthHeaders(header http.Header) *AgentAuthHeaders {
	agentID, ok1 := agentHeaderFirstValue(header, "X-Incus-Agent-Id")
	timestamp, ok2 := agentHeaderFirstValue(header, "X-Incus-Timestamp")
	nonce, ok3 := agentHeaderFirstValue(header, "X-Incus-Nonce")
	bodyHash, ok4 := agentHeaderFirstValue(header, "X-Incus-Body-Sha256")
	signature, ok5 := agentHeaderFirstValue(header, "X-Incus-Signature")
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return nil
	}
	return &AgentAuthHeaders{
		AgentID:   agentID,
		Timestamp: timestamp,
		Nonce:     nonce,
		BodyHash:  bodyHash,
		Signature: signature,
	}
}

// ValidateAgentHeaders mirrors validateAgentHeaders: "" means valid; otherwise
// the exact `details` string the TypeScript puts in the 401 body.
func ValidateAgentHeaders(headers *AgentAuthHeaders) string {
	if headers == nil {
		return "invalid agent id"
	}
	if !IsValidAgentID(headers.AgentID) {
		return "invalid agent id"
	}
	if !agentTimestampForm.MatchString(headers.Timestamp) {
		return "invalid timestamp"
	}
	if !agentNoncePattern.MatchString(headers.Nonce) {
		return "invalid nonce"
	}
	if !agentSHA256Pattern.MatchString(headers.BodyHash) {
		return "invalid body hash"
	}
	if !agentSHA256Pattern.MatchString(headers.Signature) {
		return "invalid signature"
	}
	return ""
}

// IsAgentTimestampFresh mirrors isAgentTimestampFresh.
func IsAgentTimestampFresh(timestamp string, nowMs int64) bool {
	parsed, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	// Number.isSafeInteger bound: 13 digits always fits, but keep the guard.
	if parsed > 9007199254740991 || parsed < -9007199254740991 {
		return false
	}
	delta := nowMs - parsed
	if delta < 0 {
		delta = -delta
	}
	return delta <= AgentTimestampWindowMs
}

// AgentBuildRequestPath mirrors buildRequestPath: the URL without the query.
func AgentBuildRequestPath(requestURL string) string {
	if index := strings.IndexByte(requestURL, '?'); index >= 0 {
		path := requestURL[:index]
		if path != "" {
			return path
		}
	}
	if requestURL == "" {
		return "/"
	}
	return requestURL
}

// AgentBodyHashForRequest mirrors the route's body-hash rule:
//
//	(request.method === 'GET' && !request.body)
//	  ? sha256('')
//	  : createAgentBodyHash(request.body ?? {})
func AgentBodyHashForRequest(method string, hasBody bool, body any) string {
	if strings.EqualFold(method, "GET") && !hasBody {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:])
	}
	if body == nil {
		body = map[string]any{}
	}
	return CreateAgentBodyHash(body)
}

// agentNonceExpiry is the SQL-facing expiry for a newly consumed nonce.
func agentNonceExpiry(now time.Time) time.Time {
	return now.UTC().Add(time.Duration(AgentNonceTTLMs) * time.Millisecond)
}

// ErrAgentNonceReplay is returned by the nonce writer when the unique
// (agent_id, nonce) constraint rejects a replay.
var ErrAgentNonceReplay = errors.New("agent nonce replay")
