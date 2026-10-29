// Port of server/src/lib/errors.ts (apiError). The ErrorCode constants and
// ErrorMessages map live in errors_gen.go, generated from the TypeScript source.
package lib

// APIError is the canonical JSON error envelope returned to clients.
// Field names and the 256-byte details cap match the Node implementation.
type APIError struct {
	Error   string    `json:"error"`
	Code    ErrorCode `json:"code"`
	Details string    `json:"details,omitempty"`
}

var internalErrorCodes = map[ErrorCode]bool{
	ErrInternalError: true,
}

// APIErrorFor mirrors apiError(code, details): internal errors never leak details,
// and any non-empty details are token-sanitized and truncated to 256 runes.
func APIErrorFor(code ErrorCode, details string) APIError {
	message := ErrorMessages[code]
	if internalErrorCodes[code] || details == "" {
		return APIError{Error: message, Code: code}
	}

	sanitized := SanitizeTokensInString(details)
	if runes := []rune(sanitized); len(runes) > 256 {
		sanitized = string(runes[:256])
	}

	return APIError{Error: message, Code: code, Details: sanitized}
}
