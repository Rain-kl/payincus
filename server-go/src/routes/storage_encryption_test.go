package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestStorageEncryptionParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		id       string
		pool     string
		status   int
		message  string
		hostID   int32
		wantPool string
	}{
		{"4", "data", 0, "", 4, "data"},
		{"0", "data", 400, "id format is invalid", 0, ""},
		{"0x4", "data", 400, "id format is invalid", 0, ""},
		{"1.5", "data", 400, "id format is invalid", 0, ""},
		{"04", "data", 400, "id format is invalid", 0, ""},
		{"9999999999", "data", 400, "节点 ID 无效", 0, ""},
		{"4", "-bad", 400, "poolName format is invalid", 0, ""},
		{"4", "bad space", 400, "poolName format is invalid", 0, ""},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest(http.MethodGet, "/api/hosts/4/storage-pools/data/encryption", nil)
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = request
		context.Params = gin.Params{{Key: "hostId", Value: testCase.id}, {Key: "poolName", Value: testCase.pool}}

		hostID, poolName, ok := storageEncryptionParams(context)
		if testCase.status != 0 {
			if ok || recorder.Code != testCase.status {
				t.Fatalf("%s/%s status = %d, ok=%v", testCase.id, testCase.pool, recorder.Code, ok)
			}
			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if payload["error"] != testCase.message {
				t.Fatalf("%s/%s error = %v, want %q", testCase.id, testCase.pool, payload["error"], testCase.message)
			}
			continue
		}
		if !ok || hostID != testCase.hostID || poolName != testCase.wantPool {
			t.Fatalf("%s/%s = (%d, %q, %v)", testCase.id, testCase.pool, hostID, poolName, ok)
		}
	}
}
