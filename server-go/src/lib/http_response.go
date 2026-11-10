// Port of server/src/lib/http-response.ts — bounded response reads for outbound
// HTTP calls (the OAuth provider token/userinfo fetches are the live caller).
//
// The cap exists so a hostile or misconfigured upstream cannot make the backend
// buffer an unbounded body. Node checks Content-Length first and then enforces the
// limit while streaming; this port does the same, so a lying Content-Length is
// still caught.
package lib

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// DefaultMaxTextResponseBytes mirrors DEFAULT_MAX_TEXT_RESPONSE_BYTES (1 MiB).
const DefaultMaxTextResponseBytes = 1024 * 1024

// ReadLimitedTextResponse mirrors readLimitedTextResponse(response, label, maxBytes).
//
// `maxBytes <= 0` selects DefaultMaxTextResponseBytes. The error text is
// byte-identical to the TypeScript: "<label> is too large".
func ReadLimitedTextResponse(response *http.Response, label string, maxBytes int64) (string, error) {
	if label == "" {
		label = "HTTP response"
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxTextResponseBytes
	}

	// Number(content-length || '0'): a missing or non-numeric header is ignored
	// (Number('') is 0, Number('abc') is NaN and fails the isFinite check).
	if header := response.Header.Get("Content-Length"); header != "" {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(header), 64)
		if err == nil && parsed > float64(maxBytes) {
			return "", errors.New(label + " is too large")
		}
	}

	if response.Body == nil {
		return "", nil
	}

	// Read one byte past the limit so an oversized body is detected even when the
	// upstream understated Content-Length.
	limited := io.LimitReader(response.Body, maxBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return "", err
	}
	if int64(len(payload)) > maxBytes {
		return "", errors.New(label + " is too large")
	}
	return string(payload), nil
}
