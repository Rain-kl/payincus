package routes

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// egressTestUUID mirrors the vless/vmess credential used throughout the Node
// guard script (scripts/test-egress.ts).
const egressTestUUID = "00000000-0000-4000-8000-000000000001"

func egressTestOutboundUsersPass(node egressImportedProxy) any {
	settings, _ := node.Outbound["settings"].(map[string]any)
	servers, _ := settings["servers"].([]any)
	if len(servers) == 0 {
		return nil
	}
	entry, _ := servers[0].(map[string]any)
	users, _ := entry["users"].([]any)
	if len(users) == 0 {
		return nil
	}
	user, _ := users[0].(map[string]any)
	return user["pass"]
}

func egressTestStreamSettings(node egressImportedProxy, field, key string) any {
	stream, _ := node.Outbound["streamSettings"].(map[string]any)
	settings, _ := stream[field].(map[string]any)
	return settings[key]
}

func TestEgressParseProxyImportURIs(t *testing.T) {
	input := "socks5://user:password@example.com:1080#Socks\n" +
		"vless://" + egressTestUUID + "@example.com:443?security=tls&type=ws&host=example.com&path=%2Fproxy#Vless"

	parsed, err := egressParseProxyImport(input)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Nodes) != 2 || len(parsed.Errors) != 0 {
		t.Fatalf("nodes=%d errors=%d, want 2/0", len(parsed.Nodes), len(parsed.Errors))
	}
	if got := egressTestOutboundUsersPass(parsed.Nodes[0]); got != "password" {
		t.Fatalf("socks password = %v", got)
	}
	if got := egressTestStreamSettings(parsed.Nodes[1], "wsSettings", "path"); got != "/proxy" {
		t.Fatalf("vless ws path = %v", got)
	}
	if parsed.Nodes[1].Protocol != "vless" {
		t.Fatalf("protocol = %q", parsed.Nodes[1].Protocol)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(input))
	decoded, err := egressParseProxyImport(encoded)
	if err != nil {
		t.Fatalf("parse base64: %v", err)
	}
	if len(decoded.Nodes) != 2 {
		t.Fatalf("base64 nodes = %d, want 2", len(decoded.Nodes))
	}
}

func TestEgressParseProxyImportFingerprintIgnoresName(t *testing.T) {
	first, err := egressParseProxyImport("socks5://example.com:1080")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	second, err := egressParseProxyImport("socks5://example.com:1080#DifferentName")
	if err != nil {
		t.Fatalf("parse named: %v", err)
	}
	if first.Nodes[0].Fingerprint != second.Nodes[0].Fingerprint {
		t.Fatalf("fingerprint depends on the name: %s != %s",
			first.Nodes[0].Fingerprint, second.Nodes[0].Fingerprint)
	}
	if first.Nodes[0].Name != "socks example.com:1080" {
		t.Fatalf("default name = %q", first.Nodes[0].Name)
	}
}

func TestEgressParseProxyImportJSONOutbounds(t *testing.T) {
	raw := `{"outbounds":[
		{"protocol":"socks","tag":"native","settings":{"servers":[{"address":"example.com","port":1080}]}},
		{"protocol":"freedom"}]}`
	parsed, err := egressParseProxyImport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Nodes) != 1 || len(parsed.Errors) != 1 {
		t.Fatalf("nodes=%d errors=%d, want 1/1", len(parsed.Nodes), len(parsed.Errors))
	}
	if parsed.Errors[0].Index != 2 {
		t.Fatalf("error index = %d, want 2", parsed.Errors[0].Index)
	}
	if parsed.Nodes[0].Name != "native" {
		t.Fatalf("tag name = %q", parsed.Nodes[0].Name)
	}
}

func TestEgressParseProxyImportSingbox(t *testing.T) {
	raw := `{"outbounds":[
		{"type":"vless","tag":"converted","server":"example.com","server_port":443,
		 "uuid":"` + egressTestUUID + `","tls":{"enabled":true,"server_name":"example.com"},
		 "transport":{"type":"ws","path":"/ws"}},
		{"type":"hysteria2","server":"example.com"},
		{"type":"socks","server":"example.com","server_port":1080,"detour":"another"}]}`
	parsed, err := egressParseProxyImport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Nodes) != 1 || len(parsed.Errors) != 2 {
		t.Fatalf("nodes=%d errors=%d, want 1/2", len(parsed.Nodes), len(parsed.Errors))
	}
	stream, _ := parsed.Nodes[0].Outbound["streamSettings"].(map[string]any)
	if stream["security"] != "tls" {
		t.Fatalf("singbox security = %v", stream["security"])
	}
}

func TestEgressParseProxyImportShadowsocksAndReality(t *testing.T) {
	credential := base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:secret"))
	parsed, err := egressParseProxyImport("ss://" + credential + "@example.com:8388#SS")
	if err != nil {
		t.Fatalf("parse ss: %v", err)
	}
	if len(parsed.Nodes) != 1 || parsed.Nodes[0].Protocol != "shadowsocks" {
		t.Fatalf("ss nodes = %+v", parsed.Nodes)
	}

	reality, err := egressParseProxyImport(
		"vless://" + egressTestUUID + "@example.com:443?security=reality&pbk=public-key&sni=example.com")
	if err != nil {
		t.Fatalf("parse reality: %v", err)
	}
	if len(reality.Nodes) != 1 {
		t.Fatalf("reality nodes = %d", len(reality.Nodes))
	}
	if got := egressTestStreamSettings(reality.Nodes[0], "realitySettings", "publicKey"); got != "public-key" {
		t.Fatalf("reality publicKey = %v", got)
	}

	vision, err := egressParseProxyImport("vless://" + egressTestUUID +
		"@example.com:443?encryption=none&flow=xtls-rprx-vision&security=reality&sni=example.com&fp=chrome&pbk=public-key&type=tcp&headerType=none#Vision")
	if err != nil {
		t.Fatalf("parse vision: %v", err)
	}
	if len(vision.Errors) != 0 || len(vision.Nodes) != 1 {
		t.Fatalf("vision nodes=%d errors=%d", len(vision.Nodes), len(vision.Errors))
	}
	settings, _ := vision.Nodes[0].Outbound["settings"].(map[string]any)
	vnext, _ := settings["vnext"].([]any)
	entry, _ := vnext[0].(map[string]any)
	users, _ := entry["users"].([]any)
	user, _ := users[0].(map[string]any)
	if user["flow"] != "xtls-rprx-vision" {
		t.Fatalf("vision flow = %v", user["flow"])
	}
	stream, _ := vision.Nodes[0].Outbound["streamSettings"].(map[string]any)
	if _, present := stream["tcpSettings"]; present {
		t.Fatalf("vision must not emit tcpSettings")
	}
}

func TestEgressParseProxyImportCapabilityFlag(t *testing.T) {
	capability := "vless://" + egressTestUUID + "@example.com:443?security=reality&pbk=public-key&support-x25519mlkem768=true"
	withFlag, err := egressParseProxyImport(capability)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(withFlag.Errors) != 0 || len(withFlag.Nodes) != 1 {
		t.Fatalf("capability nodes=%d errors=%d", len(withFlag.Nodes), len(withFlag.Errors))
	}
	withoutFlag, err := egressParseProxyImport(strings.Replace(capability, "&support-x25519mlkem768=true", "", 1))
	if err != nil {
		t.Fatalf("parse without flag: %v", err)
	}
	if withFlag.Nodes[0].Fingerprint != withoutFlag.Nodes[0].Fingerprint {
		t.Fatalf("capability flag changed the fingerprint")
	}
	invalid, err := egressParseProxyImport(strings.Replace(capability, "=true", "=invalid", 1))
	if err != nil {
		t.Fatalf("parse invalid flag: %v", err)
	}
	if len(invalid.Nodes) != 0 {
		t.Fatalf("invalid capability accepted")
	}
}

func TestEgressParseProxyImportRejections(t *testing.T) {
	header, err := egressParseProxyImport("vless://" + egressTestUUID + "@example.com:443?headerType=http")
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if len(header.Nodes) != 0 {
		t.Fatalf("headerType=http accepted")
	}

	dangerous, err := egressParseProxyImport(`{"protocol":"socks","settings":{"servers":[{"address":"example.com","port":1080}]},"streamSettings":{"sockopt":{"dialerProxy":"direct"}}}`)
	if err != nil {
		t.Fatalf("parse dangerous: %v", err)
	}
	if len(dangerous.Nodes) != 0 {
		t.Fatalf("sockopt accepted")
	}

	secret, err := egressParseProxyImport("socks5://user:do-not-echo@example.com:99999")
	if err != nil {
		t.Fatalf("parse secret: %v", err)
	}
	if len(secret.Errors) != 1 || strings.Contains(secret.Errors[0].Error, "do-not-echo") {
		t.Fatalf("error leaked the credential: %+v", secret.Errors)
	}
}

func TestEgressParseProxyImportSizeLimits(t *testing.T) {
	if _, err := egressParseProxyImport(strings.Repeat("a", 524289)); err == nil {
		t.Fatalf("oversized import accepted")
	} else {
		var networkError *lib.NetworkInputError
		if !asNetworkInputError(err, &networkError) || networkError.Message != "Import exceeds 512 KiB" {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if _, err := egressParseProxyImport("   "); err == nil {
		t.Fatalf("empty import accepted")
	}
}

// asNetworkInputError is a tiny errors.As wrapper kept local so the test file
// does not need to import errors twice.
func asNetworkInputError(err error, target **lib.NetworkInputError) bool {
	for err != nil {
		if typed, ok := err.(*lib.NetworkInputError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func TestEgressJSONStringify(t *testing.T) {
	value := map[string]any{
		"b": float64(1),
		"a": "x\"y",
		"c": []any{float64(1), nil, true},
		"d": nil,
	}
	got := egressJSONStringify(value)
	want := `{"a":"x\"y","b":1,"c":[1,null,true]}`
	if got != want {
		t.Fatalf("stringify = %s, want %s", got, want)
	}
	if egressJSONStringify("\x01") != `"\u0001"` {
		t.Fatalf("control escaping = %s", egressJSONStringify("\x01"))
	}
	if egressJSONStringify(float64(1080)) != "1080" {
		t.Fatalf("integer formatting = %s", egressJSONStringify(float64(1080)))
	}
}

func TestEgressJSTrim(t *testing.T) {
	cases := map[string]string{
		"  hello  ":        "hello",
		"\u3000wide\u3000": "wide",
		"\uFEFFbom":        "bom",
		"\t\n\r x \r\n\t":  "x",
		"   ":              "",
	}
	for input, want := range cases {
		if got := egressJSTrim(input); got != want {
			t.Fatalf("egressJSTrim(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEgressPolicyProbeURL(t *testing.T) {
	if got, err := egressPolicyProbeURL("https://1.1.1.1/cdn-cgi/trace"); err != nil || got == "" {
		t.Fatalf("valid probe rejected: %v", err)
	}
	for _, invalid := range []any{
		"http://example.com",
		"https://user:pass@example.com",
		"https://",
		"ftp://example.com",
		float64(5),
		"https://example.com/" + strings.Repeat("a", 2048),
	} {
		if _, err := egressPolicyProbeURL(invalid); err == nil {
			t.Fatalf("probe %v accepted", invalid)
		}
	}
}

func TestEgressPolicyIDs(t *testing.T) {
	if ids, err := egressPolicyIDs([]any{float64(1), float64(2)}, 10); err != nil || len(ids) != 2 {
		t.Fatalf("ids = %v, %v", ids, err)
	}
	for _, invalid := range []any{
		[]any{float64(1), float64(1)},
		[]any{float64(0)},
		[]any{float64(2147483648)},
		[]any{"1"},
		[]any{float64(1.5)},
	} {
		if _, err := egressPolicyIDs(invalid, 10); err == nil {
			t.Fatalf("ids %v accepted", invalid)
		}
	}
	tooMany := make([]any, 0, 11)
	for index := 0; index < 11; index++ {
		tooMany = append(tooMany, float64(index+1))
	}
	if _, err := egressPolicyIDs(tooMany, 10); err == nil {
		t.Fatalf("11 ids accepted")
	}
}

func TestEgressNodeIDsValueMessages(t *testing.T) {
	body := map[string]any{"nodeIds": []any{float64(1), float64(1)}}
	if _, message := egressNodeIDsValue(body, "nodeIds", 1, 10); message != "must NOT have duplicate items (items ## 1 and 0 are identical)" {
		t.Fatalf("duplicate message = %q", message)
	}
	body = map[string]any{"nodeIds": []any{}}
	if _, message := egressNodeIDsValue(body, "nodeIds", 1, 10); message != "must NOT have fewer than 1 items" {
		t.Fatalf("minItems message = %q", message)
	}
	body = map[string]any{"nodeIds": []any{float64(0)}}
	if _, message := egressNodeIDsValue(body, "nodeIds", 1, 10); message != "must be >= 1" {
		t.Fatalf("minimum message = %q", message)
	}
	body = map[string]any{"nodeIds": "1"}
	ids, message := egressNodeIDsValue(body, "nodeIds", 1, 10)
	if message != "" || len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("scalar coercion = %v, %q", ids, message)
	}
}

func TestEgressSanitizeNetworkStatus(t *testing.T) {
	report := map[string]any{
		"appliedRevision": float64(3),
		"groups": []any{
			map[string]any{"key": "p1", "state": "ready", "error": "none"},
			map[string]any{"key": "bad", "state": "ready"},
			map[string]any{"key": "p2", "state": "nope"},
			map[string]any{"key": "p3", "state": "error", "error": "weird"},
		},
		"bindings": []any{
			map[string]any{"instanceName": "box", "generation": float64(2)},
			map[string]any{"instanceName": strings.Repeat("x", 101), "generation": float64(1)},
			map[string]any{"instanceName": "zero", "generation": float64(0)},
		},
		"state":           "applied",
		"error":           "none",
		"errorStage":      "health",
		"errorDetail":     "timeout",
		"xrayAvailable":   true,
		"activeNodeId":    float64(7),
		"managedNetworks": []any{"iceg0", "bad network"},
		"routedFallback":  true,
		"healthyNodeIds":  []any{float64(7), float64(0), "8"},
	}
	status, err := egressSanitizeNetworkStatus(report)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	groups := status["groups"].([]gin.H)
	if len(groups) != 2 || groups[0]["key"] != "p1" || groups[1]["error"] != "network" {
		t.Fatalf("groups = %+v", groups)
	}
	bindings := status["bindings"].([]gin.H)
	if len(bindings) != 1 || bindings[0]["instanceName"] != "box" {
		t.Fatalf("bindings = %+v", bindings)
	}
	if status["state"] != "applied" || status["error"] != "none" || status["errorStage"] != "health" {
		t.Fatalf("status fields = %+v", status)
	}
	if status["xrayAvailable"] != true || status["routedFallback"] != true {
		t.Fatalf("boolean fields = %+v", status)
	}
	networks := status["managedNetworks"].([]any)
	if len(networks) != 1 || networks[0] != "iceg0" {
		t.Fatalf("managedNetworks = %+v", networks)
	}
	healthy := status["healthyNodeIds"].([]any)
	if len(healthy) != 1 || healthy[0] != float64(7) {
		t.Fatalf("healthyNodeIds = %+v", healthy)
	}

	if _, err := egressSanitizeNetworkStatus(map[string]any{}); err == nil {
		t.Fatalf("missing appliedRevision accepted")
	}
	if _, err := egressSanitizeNetworkStatus("nope"); err == nil {
		t.Fatalf("non-object report accepted")
	}
	// Unknown states fall back to their defaults.
	fallback, err := egressSanitizeNetworkStatus(map[string]any{"appliedRevision": float64(0), "state": "weird"})
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if fallback["state"] != "error" || fallback["error"] != "network" || fallback["errorStage"] != "" {
		t.Fatalf("fallback fields = %+v", fallback)
	}
}

func TestEgressSanitizeProbeResults(t *testing.T) {
	raw := json.RawMessage(`{"items":[
		{"nodeId":1,"success":true,"latencyMs":12.6},
		{"nodeId":2,"success":false,"error":"timeout","errorDetail":"command_failed"}]}`)
	items, err := egressSanitizeProbeResults(raw, []int32{1, 2})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0]["latencyMs"] != float64(13) || items[0]["error"] != "" {
		t.Fatalf("first item = %+v", items[0])
	}
	if items[1]["error"] != "timeout" || items[1]["errorDetail"] != "command_failed" || items[1]["latencyMs"] != nil {
		t.Fatalf("second item = %+v", items[1])
	}

	bad := json.RawMessage(`{"items":[{"nodeId":1,"success":true}]}`)
	if _, err := egressSanitizeProbeResults(bad, []int32{1, 2}); err == nil {
		t.Fatalf("short item list accepted")
	}
	duplicate := json.RawMessage(`{"items":[{"nodeId":1,"success":true},{"nodeId":1,"success":true}]}`)
	if _, err := egressSanitizeProbeResults(duplicate, []int32{1, 2}); err == nil {
		t.Fatalf("duplicate nodeIds accepted")
	}
}

func TestEgressControlFailure(t *testing.T) {
	cases := map[string]struct {
		status  int
		message string
	}{
		egressTunnelCodeOffline:         {503, "节点隧道未连接，请确认 Agent 在线后重试"},
		egressTunnelCodeUpgradeRequired: {409, "请升级 Agent 后使用实时配置与检测"},
		egressTunnelCodeTimeout:         {504, "等待节点响应超时，请检查节点状态"},
		egressTunnelCodeInvalidResponse: {502, "节点返回的数据无效"},
		egressTunnelCodeRemoteError:     {502, "节点执行失败，请查看 Agent 日志"},
	}
	for code, want := range cases {
		status, message := egressControlFailure(&egressTunnelControlError{Code: code})
		if status != want.status || message != want.message {
			t.Fatalf("%s = (%d, %q), want (%d, %q)", code, status, message, want.status, want.message)
		}
	}
	if status, _ := egressControlFailure(errTestPlain{}); status != 502 {
		t.Fatalf("unknown error status = %d", status)
	}
}

type errTestPlain struct{}

func (errTestPlain) Error() string { return "boom" }

func TestEgressAgentHasCapability(t *testing.T) {
	if !egressAgentHasCapability([]byte(`["network-control"]`), "network-control") {
		t.Fatalf("capability not detected")
	}
	for _, raw := range []string{``, `null`, `{}`, `["other"]`, `"network-control"`} {
		if egressAgentHasCapability([]byte(raw), "network-control") {
			t.Fatalf("capability %q accepted", raw)
		}
	}
}

func TestEgressJSStringAndNumber(t *testing.T) {
	if egressJSString(nil) != "null" || egressJSString(true) != "true" || egressJSString(float64(5)) != "5" {
		t.Fatalf("egressJSString primitives")
	}
	if egressJSString([]any{float64(1), float64(2)}) != "1,2" {
		t.Fatalf("array String()")
	}
	if egressJSString(map[string]any{}) != "[object Object]" {
		t.Fatalf("object String()")
	}
	if egressJSNumber(" 12 ") != 12 || egressJSNumber(true) != 1 || egressJSNumber("0x10") != 16 {
		t.Fatalf("egressJSNumber coercion")
	}
	if !egressIsSafeInteger(float64(2147483647)) || egressIsSafeInteger(1.5) {
		t.Fatalf("safe integer predicate")
	}
}

func TestEgressRequireNetworkControlDefaults(t *testing.T) {
	// The tunnel is not ported: the default must fail explicitly, never succeed.
	err := egressRequireNetworkControl(1, "network.apply")
	if err == nil {
		t.Fatalf("unported tunnel reported as available")
	}
	if status, _ := egressControlFailure(err); status != 503 && status != 409 {
		t.Fatalf("unexpected status %d", status)
	}
	if _, err := EgressTunnelRequestControl(nil, 1, "proxy.probe", nil, 0); err == nil {
		t.Fatalf("default requestControl succeeded")
	}
}
