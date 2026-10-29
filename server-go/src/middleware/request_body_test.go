package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newBodyRouter mounts ValidateFastifyBody behind a trivial handler so the tests
// observe only the parser rules.
func newBodyRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if status, message := ValidateFastifyBody(c); status != 0 {
			AbortFastifyBody(c, status, message)
			return
		}
		c.Next()
	})
	router.Handle(http.MethodGet, "/x", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.Handle(http.MethodHead, "/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.Handle(http.MethodOptions, "/x", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		router.Handle(method, "/x", func(c *gin.Context) {
			payload, _ := c.GetRawData()
			c.JSON(http.StatusOK, gin.H{"ok": true, "len": len(payload)})
		})
	}
	return router
}

// TestValidateFastifyBodyJSONRules pins the JSON content-type rows measured
// against fastify@5.
func TestValidateFastifyBodyJSONRules(t *testing.T) {
	router := newBodyRouter()

	cases := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
		wantMessage string
	}{
		{"empty json body is 400", http.MethodPost, "application/json", "", http.StatusBadRequest, FastifyEmptyBodyMessage},
		{"whitespace-only json body is 400", http.MethodPost, "application/json", "  \n\t ", http.StatusBadRequest, FastifyEmptyBodyMessage},
		{"malformed json is 400", http.MethodPost, "application/json", `{oops`, http.StatusBadRequest, FastifyInvalidBodyMessage},
		{"json null is accepted", http.MethodPost, "application/json", "null", http.StatusOK, ""},
		{"json 0 is accepted", http.MethodPost, "application/json", "0", http.StatusOK, ""},
		{"json empty string is accepted", http.MethodPost, "application/json", `""`, http.StatusOK, ""},
		{"json empty array is accepted", http.MethodPost, "application/json", "[]", http.StatusOK, ""},
		{"json empty object is accepted", http.MethodPost, "application/json", "{}", http.StatusOK, ""},
		{"json with charset param", http.MethodPost, "application/json; charset=utf-8", "", http.StatusBadRequest, FastifyEmptyBodyMessage},
		{"content type is case-insensitive", http.MethodPost, "APPLICATION/JSON", "", http.StatusBadRequest, FastifyEmptyBodyMessage},
		{"proto poisoning is rejected", http.MethodPost, "application/json", `{"__proto__":{"x":1}}`, http.StatusBadRequest, FastifyInvalidBodyMessage},
		{"constructor key is rejected", http.MethodPost, "application/json", `{"constructor":{"x":1}}`, http.StatusBadRequest, FastifyInvalidBodyMessage},
		{"nested proto poisoning is rejected", http.MethodPost, "application/json", `{"a":[{"__proto__":1}]}`, http.StatusBadRequest, FastifyInvalidBodyMessage},

		{"delete with empty json is 400", http.MethodDelete, "application/json", "", http.StatusBadRequest, FastifyEmptyBodyMessage},
		{"patch with empty json is 400", http.MethodPatch, "application/json", "", http.StatusBadRequest, FastifyEmptyBodyMessage},
		{"put with empty json is 400", http.MethodPut, "application/json", "", http.StatusBadRequest, FastifyEmptyBodyMessage},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reader *strings.Reader
			if tc.body == "" {
				reader = strings.NewReader("")
			} else {
				reader = strings.NewReader(tc.body)
			}
			request := httptest.NewRequest(tc.method, "/x", reader)
			request.Header.Set("Content-Type", tc.contentType)
			// httptest sets ContentLength from the reader; an empty reader yields 0,
			// which is exactly the "no body" case Fastify distinguishes.

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantMessage != "" && !strings.Contains(recorder.Body.String(), tc.wantMessage) {
				t.Fatalf("body = %s, want message %q", recorder.Body.String(), tc.wantMessage)
			}
		})
	}
}

// TestValidateFastifyBodyMediaTypes pins the 415 and pass-through rows.
func TestValidateFastifyBodyMediaTypes(t *testing.T) {
	router := newBodyRouter()

	cases := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantMessage string
	}{
		{"text/plain empty passes", "text/plain", "", http.StatusOK, ""},
		{"text/plain with body passes", "text/plain", "not json", http.StatusOK, ""},
		{"multipart passes", "multipart/form-data; boundary=xyz", "", http.StatusOK, ""},
		{"xml is 415", "application/xml", "<a/>", http.StatusUnsupportedMediaType, "Unsupported Media Type: application/xml"},
		{"octet-stream is 415", "application/octet-stream", "", http.StatusUnsupportedMediaType, "Unsupported Media Type: application/octet-stream"},
		{"merge-patch is 415", "application/merge-patch+json", "", http.StatusUnsupportedMediaType, "Unsupported Media Type: application/merge-patch+json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.contentType)

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantMessage != "" && !strings.Contains(recorder.Body.String(), tc.wantMessage) {
				t.Fatalf("body = %s, want %q", recorder.Body.String(), tc.wantMessage)
			}
		})
	}
}

// TestValidateFastifyBodyMissingContentType pins the undefined-media-type row:
// a body without a Content-Type is 415, but no body at all is fine.
func TestValidateFastifyBodyMissingContentType(t *testing.T) {
	router := newBodyRouter()

	// No Content-Type, no body → pass.
	request := httptest.NewRequest(http.MethodPost, "/x", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("bodyless request with no content type = %d, want 200", recorder.Code)
	}

	// No Content-Type but a body → 415 "undefined".
	request = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("payload"))
	request.Header.Del("Content-Type")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), FastifyUndefinedMediaMessage) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

// TestValidateFastifyBodyBodylessMethods pins that GET/HEAD/OPTIONS never
// trigger the parser, even with a JSON content type.
func TestValidateFastifyBodyBodylessMethods(t *testing.T) {
	router := newBodyRouter()

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			request := httptest.NewRequest(method, "/x", nil)
			request.Header.Set("Content-Type", "application/json")
			// Deliberately malformed: a bodyless method must not parse it.
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code >= 400 {
				t.Fatalf("%s with json content type = %d, want <400", method, recorder.Code)
			}
		})
	}
}

// TestValidateFastifyBodyBodyLimit pins the 413 row.
func TestValidateFastifyBodyBodyLimit(t *testing.T) {
	t.Setenv("BODY_LIMIT", "16")
	router := newBodyRouter()

	oversized := `{"aaaa":"` + strings.Repeat("x", 64) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(oversized))
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), FastifyTooLargeMessage) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

// TestValidateFastifyBodyReplaysBody pins that a valid JSON body is still
// readable downstream — the middleware must not swallow it.
func TestValidateFastifyBodyReplaysBody(t *testing.T) {
	router := newBodyRouter()

	payload := `{"name":"vm-1"}`
	request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"len":15`) {
		t.Fatalf("downstream handler did not see the body: %s", recorder.Body.String())
	}
}

// TestMediaTypeMatches pins the parameter and case handling.
func TestMediaTypeMatches(t *testing.T) {
	cases := []struct {
		contentType string
		expected    string
		want        bool
	}{
		{"application/json", "application/json", true},
		{"application/json; charset=utf-8", "application/json", true},
		{"APPLICATION/JSON", "application/json", true},
		{"application/json ; charset=utf-8", "application/json", true},
		{"application/jsonx", "application/json", false},
		{"text/plain", "application/json", false},
		{"", "application/json", false},
	}

	for _, tc := range cases {
		if got := mediaTypeMatches(tc.contentType, tc.expected); got != tc.want {
			t.Errorf("mediaTypeMatches(%q, %q) = %v, want %v", tc.contentType, tc.expected, got, tc.want)
		}
	}
}
