// Demo-site safety: block mutations and redact sensitive JSON on demo hosts.
// Port of the onRequest/onSend hooks in server/src/app.ts plus lib/demo-safety.ts.
package middleware

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

func demoHosts(c *gin.Context) (forwarded, host string) {
	// Go's net/http moves the Host header into Request.Host, so read it from there.
	return c.GetHeader("X-Forwarded-Host"), c.Request.Host
}

// DemoReadOnly mirrors the onRequest hook that rejects demo mutations with 403.
func DemoReadOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		forwarded, host := demoHosts(c)
		if lib.ShouldBlockDemoMutation(forwarded, host, c.Request.Method, c.Request.URL.RequestURI()) {
			c.AbortWithStatusJSON(403, gin.H{
				"error": lib.DemoReadOnlyMessage,
				"code":  "DEMO_READ_ONLY",
			})
			return
		}
		c.Next()
	}
}

type captureWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (w *captureWriter) Write(data []byte) (int, error) {
	w.body.Write(data)
	return len(data), nil
}

// DemoRedact mirrors the onSend hook that scrubs demo responses.
func DemoRedact() gin.HandlerFunc {
	return func(c *gin.Context) {
		forwarded, host := demoHosts(c)
		if !lib.IsDemoRequest(forwarded, host) || !strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if path == "/api/auth/login" || path == "/api/auth/refresh" {
			c.Next()
			return
		}

		original := c.Writer
		capture := &captureWriter{ResponseWriter: original, body: &bytes.Buffer{}}
		c.Writer = capture
		c.Next()

		payload := capture.body.Bytes()
		contentType := original.Header().Get("Content-Type")
		if !strings.Contains(contentType, "application/json") || len(payload) == 0 {
			original.WriteHeader(capture.Status())
			_, _ = original.Write(payload)
			return
		}

		var decoded any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			original.WriteHeader(capture.Status())
			_, _ = original.Write(payload)
			return
		}

		redacted, err := json.Marshal(lib.RedactDemoJSONPayload(decoded))
		if err != nil {
			original.WriteHeader(capture.Status())
			_, _ = original.Write(payload)
			return
		}
		original.WriteHeader(capture.Status())
		_, _ = original.Write(redacted)
	}
}
