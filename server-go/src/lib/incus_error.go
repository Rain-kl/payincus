// Port of server/src/lib/incus/incus-error.ts.
//
// Every Incus/certificate pin error that reaches a client is scrubbed twice:
// first the original request body is redacted (a secret echoed back by the node
// must not leak into a log or a 4xx detail), then the string is truncated. The
// order matters and is why redactNodeText exists separately from the truncation.
package lib

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// IncusApiError mirrors the TypeScript class. StatusCode is always in [400,599];
// anything else collapses to 502 so a malformed node response cannot masquerade
// as a success-adjacent code.
type IncusApiError struct {
	StatusCode int
	Message    string
}

// Error implements error.
func (e *IncusApiError) Error() string { return e.Message }

// NewIncusAPIError mirrors `new IncusApiError(message, statusCode, body)`.
func NewIncusAPIError(message string, statusCode int, body any) *IncusApiError {
	redacted := RedactNodeText(message, body)
	if runes := []rune(redacted); len(runes) > 4096 {
		redacted = string(runes[:4096])
	}
	if statusCode < 400 || statusCode > 599 {
		statusCode = 502
	}
	return &IncusApiError{StatusCode: statusCode, Message: redacted}
}

// incusSecretKeyPattern mirrors /key|password|secret|token|credential/i.
var incusSecretKeyPattern = regexp.MustCompile(`(?i)key|password|secret|token|credential`)

// RedactNodeText mirrors redactNodeText: walk the original request body (up to
// depth 5) and replace every occurrence of any field whose NAME looks secret
// with [REDACTED]. Candidates include the raw value, its percent-encoding and its
// JSON-string body form, matching the TypeScript exactly.
func RedactNodeText(message string, body any) string {
	detail := message
	redactNodeValue(&detail, body, 0)
	return detail
}

func redactNodeValue(detail *string, value any, depth int) {
	if value == nil || depth > 5 {
		return
	}

	switch typed := value.(type) {
	case map[string]any:
		for name, item := range typed {
			if incusSecretKeyPattern.MatchString(name) {
				if text, ok := item.(string); ok && text != "" {
					replaceCandidates(detail, text)
				}
				continue
			}
			redactNodeValue(detail, item, depth+1)
		}
	case []any:
		for _, item := range typed {
			redactNodeValue(detail, item, depth+1)
		}
	}
}

// replaceCandidates mirrors the inner loop:
//
//	for (const candidate of [item, encodeURIComponent(item), JSON.stringify(item).slice(1, -1)])
//	  detail = detail.split(candidate).join('[REDACTED]')
func replaceCandidates(detail *string, value string) {
	candidates := []string{value, url.QueryEscape(value), jsonSlicesOffQuotes(value)}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		*detail = strings.ReplaceAll(*detail, candidate, "[REDACTED]")
	}
}

// jsonSlicesOffQuotes mirrors `JSON.stringify(item).slice(1, -1)`: the encoded
// string with its surrounding quotes removed. JSON.stringify escapes by the
// JSON rules, which encoding/json reproduces for a plain string.
func jsonSlicesOffQuotes(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	text := string(encoded)
	if len(text) < 2 {
		return text
	}
	return text[1 : len(text)-1]
}

// NodeErrorDetail mirrors nodeErrorDetail(error, fallback, body): the message of
// the error, redacted and truncated, falling back when empty.
func NodeErrorDetail(err error, fallback string, body any) string {
	message := ""
	if err != nil {
		message = err.Error()
	}
	if message == "" {
		message = fallback
	}
	return NewIncusAPIError(message, 500, body).Message
}

// NodeOperationAborted is the Go equivalent of the TS AbortError / TimeoutError
// DOMException pair. Callers distinguish cancellation from a node failure, so the
// kind is explicit rather than parsed back out of a string.
type NodeOperationAborted struct {
	Kind    string // "AbortError" or "TimeoutError"
	Message string
}

// Error implements error.
func (e *NodeOperationAborted) Error() string { return e.Message }

// IsNodeOperationAborted reports whether err is a cancellation/timeout.
func IsNodeOperationAborted(err error) (*NodeOperationAborted, bool) {
	var aborted *NodeOperationAborted
	if errors.As(err, &aborted) {
		return aborted, true
	}
	return nil, false
}

// OperationError mirrors operationError(error, body): cancellation is preserved
// (kind and message), everything else becomes an IncusApiError that keeps the
// original status when it already was one.
func OperationError(err error, body any) error {
	if aborted, ok := IsNodeOperationAborted(err); ok {
		redacted := RedactNodeText(aborted.Message, body)
		if runes := []rune(redacted); len(runes) > 4096 {
			redacted = string(runes[:4096])
		}
		return &NodeOperationAborted{Kind: aborted.Kind, Message: redacted}
	}

	message := "节点操作失败"
	var apiErr *IncusApiError
	statusCode := 502
	if err != nil {
		if err.Error() != "" {
			message = err.Error()
		}
		if errors.As(err, &apiErr) {
			statusCode = apiErr.StatusCode
		}
	}
	return NewIncusAPIError(message, statusCode, body)
}
