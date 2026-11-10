// Tests for the bounded response reader (the OAuth provider fetch guard).
package lib

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func newHTTPResponse(contentLength string, body io.Reader) *http.Response {
	header := http.Header{}
	if contentLength != "" {
		header.Set("Content-Length", contentLength)
	}
	return &http.Response{Header: header, Body: io.NopCloser(body)}
}

func TestReadLimitedTextResponse(t *testing.T) {
	t.Run("under the limit", func(t *testing.T) {
		response := newHTTPResponse("11", strings.NewReader("hello world"))
		body, err := ReadLimitedTextResponse(response, "provider response", 64)
		if err != nil || body != "hello world" {
			t.Fatalf("body=%q err=%v", body, err)
		}
	})

	t.Run("content-length over the limit", func(t *testing.T) {
		response := newHTTPResponse("100", strings.NewReader("hello"))
		_, err := ReadLimitedTextResponse(response, "provider response", 64)
		if err == nil || err.Error() != "provider response is too large" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("streamed body over the limit without a length", func(t *testing.T) {
		response := newHTTPResponse("", strings.NewReader(strings.Repeat("a", 65)))
		_, err := ReadLimitedTextResponse(response, "x", 64)
		if err == nil || err.Error() != "x is too large" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("lying content-length is still caught", func(t *testing.T) {
		response := newHTTPResponse("10", strings.NewReader(strings.Repeat("a", 65)))
		_, err := ReadLimitedTextResponse(response, "x", 64)
		if err == nil || err.Error() != "x is too large" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("non-numeric content-length is ignored like Number() NaN", func(t *testing.T) {
		response := newHTTPResponse("not-a-number", strings.NewReader("ok"))
		body, err := ReadLimitedTextResponse(response, "x", 64)
		if err != nil || body != "ok" {
			t.Fatalf("body=%q err=%v", body, err)
		}
	})

	t.Run("exactly at the limit passes", func(t *testing.T) {
		response := newHTTPResponse("64", strings.NewReader(strings.Repeat("a", 64)))
		body, err := ReadLimitedTextResponse(response, "x", 64)
		if err != nil || len(body) != 64 {
			t.Fatalf("len=%d err=%v", len(body), err)
		}
	})

	t.Run("default label", func(t *testing.T) {
		response := newHTTPResponse("", strings.NewReader(strings.Repeat("a", 16)))
		_, err := ReadLimitedTextResponse(response, "", 8)
		if err == nil || err.Error() != "HTTP response is too large" {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("default limit is one mebibyte", func(t *testing.T) {
		if DefaultMaxTextResponseBytes != 1024*1024 {
			t.Fatalf("default = %d", DefaultMaxTextResponseBytes)
		}
	})
}
