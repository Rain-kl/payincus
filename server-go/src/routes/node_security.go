// Port of server/src/routes/node-security.ts.
//
// Admin-only node security surface for one host:
//
//	POST   /api/admin/hosts/:hostId/node-security/identity      pin the server cert
//	GET    /api/admin/hosts/:hostId/node-security               overview (live or stored)
//	POST   /api/admin/hosts/:hostId/node-security/policy        lock policy
//	POST   /api/admin/hosts/:hostId/node-security/recover       approve background recovery
//	POST   /api/admin/hosts/:hostId/node-security/lock          lock now
//	POST   /api/admin/hosts/:hostId/node-security/keys          enroll a storage key
//	POST   /api/admin/hosts/:hostId/node-security/keys/reveal   reveal a stored key
//	POST   /api/admin/hosts/:hostId/node-security/keys/unlock   push a key to the node
//
// Security contract, all reproduced from the TypeScript:
//
//   - Every response is Cache-Control: no-store + Pragma: no-cache, including a
//     revealed storage key.
//   - The node client checks the node advertises `payincus_security_v1` (and
//     `payincus_security_resume_v1` before a recover) and refuses to send anything
//     else. A node without the extension fails with
//     `节点不支持此功能，请升级集成包` — it never proceeds on a guess.
//   - The certificate pin comes from the operator-verified fingerprint stored in
//     node_security_states; a host without one is refused.
//   - The storage key is only ever returned by the explicit reveal route, and only
//     after the audit row is durably written.
//
// Gin allows one wildcard name per path position tree-wide, and /api/hosts already
// owns `:hostId`, so this module uses `:hostId` too (the TypeScript's `:hostId`
// name already matches).
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Route constants.
const (
	// nodeSecurityBase is the admin route prefix. It is absolute because gin only
	// allows one wildcard name at a path position and `:hostId` is shared with the
	// /api/hosts tree.
	nodeSecurityBase = "/api/admin/hosts/:hostId/node-security"

	// nodeSecuritySecurityV1 is the required Incus API extension.
	nodeSecuritySecurityV1 = "payincus_security_v1"
	// nodeSecuritySecurityResumeV1 gates the recover path only.
	nodeSecuritySecurityResumeV1 = "payincus_security_resume_v1"

	// nodeSecurityEventsTake mirrors `take: 100`.
	nodeSecurityEventsTake = 100

	// nodeSecurityHostIDMaximum mirrors the params schema maximum.
	nodeSecurityHostIDMaximum = 2147483647

	// nodeSecurityRevisionMaximum mirrors Number.MAX_SAFE_INTEGER.
	nodeSecurityRevisionMaximum = 9007199254740991

	// nodeSecurityDatasetMinimum / Maximum mirror the dataset schema.
	nodeSecurityDatasetMinimum = 3
	nodeSecurityDatasetMaximum = 200

	// nodeSecurityLogModule is the LogModule.SECURITY value.
	nodeSecurityLogModule = db.LogModuleSecurity

	// nodeSecurityActionFallback is the nodeErrorDetail fallback for the three
	// node actions.
	nodeSecurityActionFallback = "节点操作未完成，请刷新状态并检查连接或存储"
	// nodeSecurityUnlockFallback is the fallback for keys/unlock.
	nodeSecurityUnlockFallback = "存储解锁未完成"
)

var (
	// nodeSecurityFingerprintPattern mirrors /^[0-9a-f]{64}$/ (server cert pin).
	nodeSecurityFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// nodeSecurityKeyPattern mirrors the storage key schema.
	nodeSecurityKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// nodeSecurityDatasetPattern mirrors vault.ts' datasetPattern.
	nodeSecurityDatasetPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)*$`)
)

// ==================== response views ====================

// nodeSecurityKeyView mirrors the GET / keys projection
// `{ id, dataset, fingerprint, version, createdAt }`.
type nodeSecurityKeyView struct {
	ID          int32  `json:"id"`
	Dataset     string `json:"dataset"`
	Fingerprint string `json:"fingerprint"`
	Version     int32  `json:"version"`
	CreatedAt   string `json:"createdAt"`
}

// nodeSecurityCreatedKeyView mirrors the POST /keys projection
// `{ id, dataset, fingerprint, version }` (no createdAt).
type nodeSecurityCreatedKeyView struct {
	ID          int32  `json:"id"`
	Dataset     string `json:"dataset"`
	Fingerprint string `json:"fingerprint"`
	Version     int32  `json:"version"`
}

// nodeSecurityEventView mirrors `{ ...event, uid: Number(uid) }`. The full Prisma
// scalar set is spread, so every column is present; dates are ISO strings and
// uid is a JSON number (the column is a BigInt, which JSON.stringify rejects).
type nodeSecurityEventView struct {
	HostID     int32  `json:"hostId"`
	EventID    string `json:"eventId"`
	Command    string `json:"command"`
	UID        int64  `json:"uid"`
	PID        int32  `json:"pid"`
	Critical   bool   `json:"critical"`
	Blocked    bool   `json:"blocked"`
	OccurredAt string `json:"occurredAt"`
	ReceivedAt string `json:"receivedAt"`
}

// nodeSecurityRecoveryTaskView mirrors latestNodeRecovery's select.
type nodeSecurityRecoveryTaskView struct {
	ID         int32   `json:"id"`
	Status     string  `json:"status"`
	Error      *string `json:"error"`
	CreatedAt  string  `json:"createdAt"`
	FinishedAt *string `json:"finishedAt"`
}

// nodeSecurityOverviewResponse is the GET body.
//
// serverFingerprint and reportedAt are always present (null when there is no
// stored state — `reported?.serverFingerprint ?? null` / `reported?.reportedAt`
// where an absent optional chain value is dropped as `undefined`).
type nodeSecurityOverviewResponse struct {
	ServerFingerprint *string                       `json:"serverFingerprint"`
	RecoveryTask      *nodeSecurityRecoveryTaskView `json:"recoveryTask"`
	State             json.RawMessage               `json:"state"`
	Keys              []nodeSecurityKeyView         `json:"keys"`
	Events            []nodeSecurityEventView       `json:"events"`
	ReportedAt        *string                       `json:"reportedAt,omitempty"`
	Offline           bool                          `json:"offline,omitempty"`
}

// ==================== schema helpers ====================

// nodeSecurityParamsFields mirrors the shared `params` schema.
var nodeSecurityParamsFields = []ajvFieldSpec{
	{Name: "hostId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: nodeSecurityHostIDMaximum},
}

var nodeSecurityIdentityFields = []ajvFieldSpec{
	{Name: "fingerprint", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN(), Pattern: nodeSecurityFingerprintPattern},
}

var nodeSecurityPolicyFields = []ajvFieldSpec{
	{Name: "enabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "revision", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: nodeSecurityRevisionMaximum},
}

var nodeSecurityRecoverFields = []ajvFieldSpec{
	{Name: "revision", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: nodeSecurityRevisionMaximum},
}

var nodeSecurityKeyFields = []ajvFieldSpec{
	{Name: "dataset", Kind: ajvKindString, MinLength: nodeSecurityDatasetMinimum, MaxLength: nodeSecurityDatasetMaximum, Minimum: math.NaN(), Maximum: math.NaN(), Pattern: nodeSecurityDatasetPattern},
	{Name: "key", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN(), Pattern: nodeSecurityKeyPattern},
}

var nodeSecurityDatasetFields = []ajvFieldSpec{
	{Name: "dataset", Kind: ajvKindString, MinLength: nodeSecurityDatasetMinimum, MaxLength: nodeSecurityDatasetMaximum, Minimum: math.NaN(), Maximum: math.NaN(), Pattern: nodeSecurityDatasetPattern},
}

// nodeSecurityNoStore applies the module-wide cache headers. Fastify sets these
// inside each handler; Go sets them first so a validation failure cannot be
// cached either.
func nodeSecurityNoStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
}

// nodeSecurityValidateBody mirrors AJV for the body schemas this module declares.
//
// It reuses the coercion helpers from the shared replica but evaluates a string
// property in AJV's keyword order — type, minLength, maxLength, pattern — because
// the shared storage-config validator checks the pattern first. The difference is
// observable: for dataset "a/" (too short AND malformed) ajv@8.17.1 reports
// `dataset is too short`, verified against the real compiler with Fastify's
// default options.
func nodeSecurityValidateBody(decoded any, required []string, fields []ajvFieldSpec) (ajvValidated, string) {
	out := ajvValidated{Values: map[string]any{}, Present: map[string]bool{}}

	object, ok := decoded.(map[string]any)
	if !ok {
		return out, "must be object"
	}
	for _, name := range required {
		if _, present := object[name]; !present {
			return out, name + " is required"
		}
	}
	for _, field := range fields {
		value, present := object[field.Name]
		if !present {
			continue
		}
		coerced, message := nodeSecurityCoerceField(field, value)
		if message != "" {
			return out, message
		}
		out.Present[field.Name] = true
		out.Values[field.Name] = coerced
	}
	return out, ""
}

// nodeSecurityCoerceField applies type coercion then the keyword constraints for
// one property, in AJV's evaluation order.
func nodeSecurityCoerceField(field ajvFieldSpec, value any) (any, string) {
	switch field.Kind {
	case ajvKindString:
		text, message := ajvCoerceString(value)
		if message != "" {
			return nil, message
		}
		if field.MinLength >= 0 && jsStringLength(text) < field.MinLength {
			return nil, field.Name + " is too short"
		}
		if field.MaxLength >= 0 && jsStringLength(text) > field.MaxLength {
			return nil, field.Name + " is too long"
		}
		if field.Pattern != nil && !field.Pattern.MatchString(text) {
			return nil, ajvPatternMessage(field.Name)
		}
		if len(field.Enum) > 0 && !ajvEnumContains(field.Enum, text) {
			return nil, "must be equal to one of the allowed values"
		}
		return text, ""

	case ajvKindInteger:
		number, ok := ajvCoerceInteger(value)
		if !ok {
			return nil, "must be integer"
		}
		if message := ajvIntegerBoundsMessage(field, number); message != "" {
			return nil, message
		}
		return number, ""

	case ajvKindBoolean:
		coerced, ok := ajvCoerceBoolean(value)
		if !ok {
			return nil, "must be boolean"
		}
		return coerced, ""
	}
	return nil, "must be object"
}

// nodeSecurityParseHostID mirrors the params schema: an integer in [1, 2^31-1]
// with Fastify's string→number coercion ("01" is 1, "abc" is invalid).
func nodeSecurityParseHostID(c *gin.Context) (int32, bool) {
	validated, message := nodeSecurityValidateBody(
		map[string]any{"hostId": c.Param("hostId")},
		[]string{"hostId"},
		nodeSecurityParamsFields,
	)
	if message != "" {
		respondAJVValidation(c, message)
		return 0, false
	}
	number, ok := validated.Values["hostId"].(float64)
	if !ok {
		respondAJVValidation(c, "hostId must be integer")
		return 0, false
	}
	return int32(number), true
}

// nodeSecurityDecodeBody runs the shared Fastify body decoder and schema check.
func nodeSecurityDecodeBody(c *gin.Context, required []string, fields []ajvFieldSpec) (ajvValidated, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return ajvValidated{}, false
	}
	body, validationMessage := nodeSecurityValidateBody(decoded.Value, required, fields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return ajvValidated{}, false
	}
	return body, true
}

// nodeSecurityPayload mirrors the coerced `request.body` that the TypeScript
// forwards to the node: only the declared properties survive
// `additionalProperties:false`, and absent ones are omitted.
func nodeSecurityPayload(body ajvValidated, fields []ajvFieldSpec) map[string]any {
	payload := make(map[string]any, len(fields))
	for _, field := range fields {
		if !body.Present[field.Name] {
			continue
		}
		payload[field.Name] = body.Values[field.Name]
	}
	return payload
}

// ==================== node client ====================

// nodeSecurityNodeClient is the route's `client(hostId)` helper: one short-lived
// pinned mTLS client per request that verifies the node's security extension
// before sending anything.
type nodeSecurityNodeClient struct {
	hostID        int32
	url           string
	certPath      *string
	keyPath       *string
	tunnelEnabled bool
	targetHost    string
	targetPort    int32
	fingerprint   string
}

// Request implements lib.RecoveryClient.
func (n *nodeSecurityNodeClient) Request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	node := lib.NewIncusClient(lib.IncusClientOptions{
		URL:               n.url,
		CertPath:          n.certPath,
		KeyPath:           n.keyPath,
		HostID:            int64(n.hostID),
		TunnelEnabled:     n.tunnelEnabled,
		TargetHost:        n.targetHost,
		TargetPort:        int(n.targetPort),
		ServerFingerprint: n.fingerprint,
	})
	defer func() { _ = node.Close() }()

	if _, err := node.Connect(ctx); err != nil {
		return nil, err
	}

	infoRaw, err := node.Request(ctx, http.MethodGet, "/1.0/payincus", nil, 0, nil)
	if err != nil {
		return nil, err
	}
	var info struct {
		APIExtensions []string `json:"api_extensions"`
	}
	if err := json.Unmarshal(infoRaw, &info); err != nil {
		// `!Array.isArray(info?.api_extensions)` — a node that answers with a
		// non-array cannot prove it supports the integration.
		return nil, errors.New("节点不支持此功能，请升级集成包")
	}
	if !nodeSecurityHasExtension(info.APIExtensions, nodeSecuritySecurityV1) {
		return nil, errors.New("节点不支持此功能，请升级集成包")
	}
	if path == "/1.0/payincus/security/recover" && !nodeSecurityHasExtension(info.APIExtensions, nodeSecuritySecurityResumeV1) {
		return nil, errors.New("节点不支持状态恢复")
	}

	return node.Request(ctx, method, path, body, 0, nil)
}

// nodeSecurityHasExtension mirrors api_extensions.includes(name).
func nodeSecurityHasExtension(extensions []string, name string) bool {
	for _, extension := range extensions {
		if extension == name {
			return true
		}
	}
	return false
}

// nodeSecurityClient mirrors the TypeScript client(hostId): it resolves the host
// and its stored server-certificate pin, or fails with the TypeScript messages.
func nodeSecurityClient(ctx context.Context, hostID int32) (*nodeSecurityNodeClient, error) {
	host, err := db.GetHostForNodeSecurity(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("节点不存在")
	}

	identity, err := db.GetNodeSecurityState(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if identity == nil || identity.ServerFingerprint == nil || *identity.ServerFingerprint == "" {
		return nil, errors.New("请先核对并保存节点证书指纹")
	}

	return &nodeSecurityNodeClient{
		hostID:        hostID,
		url:           host.URL,
		certPath:      host.CertPath,
		keyPath:       host.KeyPath,
		tunnelEnabled: host.TunnelEnabled,
		targetHost:    host.TargetHost,
		targetPort:    host.TargetPort,
		fingerprint:   *identity.ServerFingerprint,
	}, nil
}

// ==================== handlers ====================

// nodeSecurityIdentityHandler mirrors `POST {base}/identity`.
func nodeSecurityIdentityHandler(c *gin.Context) {
	nodeSecurityNoStore(c)

	hostID, ok := nodeSecurityParseHostID(c)
	if !ok {
		return
	}
	body, ok := nodeSecurityDecodeBody(c, []string{"fingerprint"}, nodeSecurityIdentityFields)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	exists, err := db.NodeSecurityHostExists(ctx, hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return
	}

	fingerprint, _ := body.Values["fingerprint"].(string)
	if err := db.UpsertNodeSecurityFingerprint(ctx, hostID, fingerprint); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	user := middleware.CurrentUser(c)
	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, nodeSecurityLogModule, "node.identity",
		fmt.Sprintf("节点 #%d 更新人工核对的服务端证书指纹", hostID),
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"fingerprint": fingerprint})
}

// nodeSecurityOverviewHandler mirrors `GET {base}`.
func nodeSecurityOverviewHandler(c *gin.Context) {
	nodeSecurityNoStore(c)

	hostID, ok := nodeSecurityParseHostID(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()

	// The TypeScript runs these four reads in parallel; the results are combined
	// before the node call, so the order is unobservable.
	keys, err := db.ListNodeStorageKeys(ctx, hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	events, err := db.ListNodeCliEvents(ctx, hostID, lib.IncusCLICommands(), nodeSecurityEventsTake)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	reported, err := db.GetNodeSecurityState(ctx, hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	recoveryTask, err := lib.LatestNodeRecovery(ctx, hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	response := nodeSecurityOverviewResponse{
		RecoveryTask: nodeSecurityRecoveryTaskResponse(recoveryTask),
		Keys:         nodeSecurityKeyResponses(keys),
		Events:       nodeSecurityEventResponses(events),
	}
	if reported != nil {
		response.ServerFingerprint = reported.ServerFingerprint
		if !reported.ReportedAt.IsZero() {
			reportedAt := storageISOTime(reported.ReportedAt)
			response.ReportedAt = &reportedAt
		}
	}

	state, err := nodeSecurityLiveState(ctx, hostID)
	if err != nil {
		// Offline: fall back to the last durable report, clearly flagged.
		response.State = db.NodeSecurityStoredReport(reported)
		response.Offline = true
		c.JSON(http.StatusOK, response)
		return
	}
	response.State = state
	c.JSON(http.StatusOK, response)
}

// nodeSecurityLiveState mirrors the try block of the GET handler: fetch the live
// state and record it. The returned JSON is the node's own document.
func nodeSecurityLiveState(ctx context.Context, hostID int32) (json.RawMessage, error) {
	client, err := nodeSecurityClient(ctx, hostID)
	if err != nil {
		return nil, err
	}
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/payincus/security", nil)
	if err != nil {
		return nil, err
	}
	decoded, err := nodeSecurityDecodeNodeJSON(raw)
	if err != nil {
		return nil, err
	}
	// `recordReport(hostId, { state, events: [] })` validates the payload; an
	// invalid one lands in the offline branch, exactly like the TypeScript.
	if _, err := lib.RecordReport(ctx, hostID, map[string]any{"state": decoded, "events": []any{}}); err != nil {
		return nil, err
	}
	return raw, nil
}

// nodeSecurityActionHandler mirrors the `policy` / `recover` / `lock` loop body.
func nodeSecurityActionHandler(c *gin.Context, action string) {
	nodeSecurityNoStore(c)

	hostID, ok := nodeSecurityParseHostID(c)
	if !ok {
		return
	}

	var (
		fields   []ajvFieldSpec
		required []string
	)
	switch action {
	case "policy":
		fields, required = nodeSecurityPolicyFields, []string{"enabled", "revision"}
	case "recover":
		fields, required = nodeSecurityRecoverFields, []string{"revision"}
	default: // lock: `{type:'object', additionalProperties:false, required:[], properties:{}}`
		fields, required = nil, nil
	}

	body, ok := nodeSecurityDecodeBody(c, required, fields)
	if !ok {
		return
	}
	payload := nodeSecurityPayload(body, fields)

	ctx := c.Request.Context()
	client, err := nodeSecurityClient(ctx, hostID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": lib.NodeErrorDetail(err, nodeSecurityActionFallback, payload)})
		return
	}

	if action == "recover" {
		nodeSecurityRecoverHandler(c, ctx, client, hostID, body, payload)
		return
	}

	raw, err := client.Request(ctx, http.MethodPost, "/1.0/payincus/security/"+action, payload)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": lib.NodeErrorDetail(err, nodeSecurityActionFallback, payload)})
		return
	}
	decoded, err := nodeSecurityDecodeNodeJSON(raw)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": lib.NodeErrorDetail(err, nodeSecurityActionFallback, payload)})
		return
	}
	if _, err := lib.RecordReport(ctx, hostID, map[string]any{"state": decoded, "events": []any{}}); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": lib.NodeErrorDetail(err, nodeSecurityActionFallback, payload)})
		return
	}

	user := middleware.CurrentUser(c)
	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, nodeSecurityLogModule, "node."+action,
		fmt.Sprintf("节点 #%d 管理操作 %s", hostID, action),
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"state": raw})
}

// nodeSecurityRecoverHandler mirrors the recover branch: enqueue a durable task,
// then return 202 immediately.
func nodeSecurityRecoverHandler(
	c *gin.Context,
	ctx context.Context,
	client *nodeSecurityNodeClient,
	hostID int32,
	body ajvValidated,
	payload map[string]any,
) {
	user := middleware.CurrentUser(c)
	actorID := int64(user.ID)

	approvedRevision, _ := body.Values["revision"].(float64)

	approval, err := db.GetNodeSecurityRecoveryGeneration(ctx, hostID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": lib.NodeErrorDetail(err, nodeSecurityActionFallback, payload)})
		return
	}
	if approval == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "节点状态不可用，请刷新后重试"})
		return
	}
	controlGeneration := *approval

	taskID, err := lib.EnqueueNodeRecovery(ctx, hostID, func(taskCtx context.Context) error {
		lib.TaskLog(taskCtx, "info", fmt.Sprintf("管理员 #%d 已批准恢复，正在核验节点状态及存储", actorID))
		db.CreateLog(taskCtx, &actorID, nodeSecurityLogModule, "node.recover",
			fmt.Sprintf("节点 #%d 管理员批准后台恢复", hostID),
			db.LogResultSuccess, db.CreateLogOptions{})

		rawState, err := client.Request(taskCtx, http.MethodPost, "/1.0/payincus/security/recover", map[string]any{
			"revision":          approvedRevision,
			"controlGeneration": controlGeneration,
		})
		if err != nil {
			return err
		}
		// The TypeScript types the response as RecoveryState without validating it
		// here; restoreNodeInstances is what rejects an unusable document. A decode
		// failure therefore leaves the zero state and fails there, after the durable
		// state has been refreshed — matching the TypeScript's ordering.
		state, _ := lib.DecodeRecoveryState(rawState)

		cleared, err := db.ClearNodeSecurityRecoveryBlocked(taskCtx, hostID, controlGeneration)
		if err != nil {
			return err
		}
		if cleared != 1 {
			return errors.New("恢复期间节点状态发生变化，请重新核验并批准")
		}

		lib.RecordHostSuccess(hostID, 0)

		decodedState, err := nodeSecurityDecodeNodeJSON(rawState)
		if err != nil {
			return err
		}
		if _, err := lib.RecordReport(taskCtx, hostID, map[string]any{"state": decodedState, "events": []any{}}); err != nil {
			return err
		}

		result, failed, restoreErr := lib.RestoreNodeInstances(
			taskCtx,
			client,
			state,
			nodeSecurityRecoveryAllowList(taskCtx, hostID),
			lib.RecoveryOptions{
				Types: []string{"container", "virtual-machine"},
				Cancelled: func() bool {
					// `task?.cancelled === true || task?.stopping === true`
					task := lib.CurrentOperationTask(taskCtx)
					return task != nil && lib.OperationTaskControl(task.ID).Stopping
				},
				Progress: func(completed, total, failedCount int) {
					lib.TaskLog(taskCtx, "info", fmt.Sprintf("恢复进度 %d/%d，未完成 %d", completed, total, failedCount))
				},
			},
		)

		// Everything from here on is the TypeScript's inner try/catch: on any error
		// refresh the durable state (so the panel sees what the node kept pending)
		// and re-throw.
		if restoreErr != nil {
			nodeSecurityRefreshAfterRecovery(taskCtx, client, hostID)
			return restoreErr
		}

		resultState, err := nodeSecurityDecodeNodeJSON(result.RawJSON())
		if err != nil {
			nodeSecurityRefreshAfterRecovery(taskCtx, client, hostID)
			return err
		}
		if _, err := lib.RecordReport(taskCtx, hostID, map[string]any{"state": resultState, "events": []any{}}); err != nil {
			nodeSecurityRefreshAfterRecovery(taskCtx, client, hostID)
			return err
		}
		if failed > 0 {
			nodeSecurityRefreshAfterRecovery(taskCtx, client, hostID)
			return fmt.Errorf("部分实例恢复未完成（%d），请刷新状态后重试", failed)
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": lib.NodeErrorDetail(err, nodeSecurityActionFallback, payload)})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"taskId": taskID, "status": "running"})
}

// nodeSecurityRecoveryAllowList mirrors the recovery allow-list callback.
func nodeSecurityRecoveryAllowList(ctx context.Context, hostID int32) func(lib.ResumeInstance) (bool, error) {
	return func(item lib.ResumeInstance) (bool, error) {
		lib.TaskLog(ctx, "info", fmt.Sprintf("核验恢复目标 %s/%s", item.Project, item.Name))
		if item.Project != "default" {
			return true, nil
		}
		instance, err := db.FindInstanceForNodeRecovery(ctx, hostID, item.Name)
		if err != nil {
			return false, err
		}
		if instance == nil {
			return true, nil
		}
		knownStatus := instance.Status == "running" || instance.Status == "stopped" || instance.Status == "error"
		if !knownStatus {
			return false, nil
		}
		if instance.ExpiresAt != nil && !instance.ExpiresAt.After(time.Now().UTC()) {
			return false, nil
		}
		return true, nil
	}
}

// nodeSecurityRefreshAfterRecovery mirrors
//
//	try { await recordReport(hostId, {state: await node.request('GET', '/1.0/payincus/security'), events: []}) }
//	catch { taskLog('warn', '无法刷新节点状态，待连接恢复后重新核验') }
func nodeSecurityRefreshAfterRecovery(ctx context.Context, client *nodeSecurityNodeClient, hostID int32) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/payincus/security", nil)
	if err != nil {
		lib.TaskLog(ctx, "warn", "无法刷新节点状态，待连接恢复后重新核验")
		return
	}
	decoded, err := nodeSecurityDecodeNodeJSON(raw)
	if err != nil {
		lib.TaskLog(ctx, "warn", "无法刷新节点状态，待连接恢复后重新核验")
		return
	}
	if _, err := lib.RecordReport(ctx, hostID, map[string]any{"state": decoded, "events": []any{}}); err != nil {
		lib.TaskLog(ctx, "warn", "无法刷新节点状态，待连接恢复后重新核验")
	}
}

// nodeSecurityKeysHandler mirrors `POST {base}/keys`.
func nodeSecurityKeysHandler(c *gin.Context) {
	nodeSecurityNoStore(c)

	hostID, ok := nodeSecurityParseHostID(c)
	if !ok {
		return
	}
	body, ok := nodeSecurityDecodeBody(c, []string{"dataset", "key"}, nodeSecurityKeyFields)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	exists, err := db.NodeSecurityHostExists(ctx, hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return
	}

	dataset, _ := body.Values["dataset"].(string)
	key, _ := body.Values["key"].(string)

	identity := lib.NodeStorageKeyIdentity{HostID: hostID, Dataset: dataset, Version: 1}
	keyFingerprint, err := lib.NodeStorageKeyFingerprint(key)
	if err != nil {
		// Unreachable: the body schema already enforces 64 lowercase hex chars.
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ciphertext, err := lib.SealNodeStorageKey(key, identity)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "控制端密钥托管未就绪，请检查主密钥配置"})
		return
	}

	// Never overwrite the sole active key through an enrollment request.
	existing, err := db.GetNodeStorageKey(ctx, hostID, dataset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if existing != nil {
		if existing.Fingerprint != keyFingerprint {
			c.JSON(http.StatusConflict, gin.H{"error": "已有不同密钥，不能直接覆盖"})
			return
		}
		unsealed, unsealErr := lib.UnsealNodeStorageKey(existing.Ciphertext, lib.NodeStorageKeyIdentity{
			HostID:  hostID,
			Dataset: existing.Dataset,
			Version: existing.Version,
		})
		if unsealErr != nil || unsealed != key {
			c.JSON(http.StatusConflict, gin.H{"error": "已有密钥无法校验，请先检查控制端托管配置"})
			return
		}
		c.JSON(http.StatusOK, nodeSecurityCreatedKeyView{
			ID:          existing.ID,
			Dataset:     dataset,
			Fingerprint: existing.Fingerprint,
			Version:     existing.Version,
		})
		return
	}

	saved, err := db.CreateNodeStorageKey(ctx, hostID, dataset, keyFingerprint, ciphertext)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	user := middleware.CurrentUser(c)
	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, nodeSecurityLogModule, "node.key.enroll",
		fmt.Sprintf("节点 #%d 保存存储密钥", hostID),
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, nodeSecurityCreatedKeyView{
		ID:          saved.ID,
		Dataset:     dataset,
		Fingerprint: keyFingerprint,
		Version:     saved.Version,
	})
}

// nodeSecurityKeyActionHandler mirrors `POST {base}/keys/{reveal,unlock}`.
func nodeSecurityKeyActionHandler(c *gin.Context, action string) {
	nodeSecurityNoStore(c)

	hostID, ok := nodeSecurityParseHostID(c)
	if !ok {
		return
	}
	body, ok := nodeSecurityDecodeBody(c, []string{"dataset"}, nodeSecurityDatasetFields)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	dataset, _ := body.Values["dataset"].(string)
	record, err := db.GetNodeStorageKey(ctx, hostID, dataset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "密钥尚未托管"})
		return
	}

	key, err := lib.UnsealNodeStorageKey(record.Ciphertext, lib.NodeStorageKeyIdentity{
		HostID:  hostID,
		Dataset: record.Dataset,
		Version: record.Version,
	})
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "密钥校验失败，未执行解锁"})
		return
	}

	// The audit row is written with a failing insert (unlike createLog) because the
	// key must never be handed out unlogged.
	user := middleware.CurrentUser(c)
	userID := int64(user.ID)
	actionLabel := "加载"
	if action == "reveal" {
		actionLabel = "领取"
	}
	if err := db.CreateNodeSecurityLogStrict(ctx, userID, nodeSecurityLogModule,
		"node.key."+action+".request",
		fmt.Sprintf("节点 #%d 管理员申请%s存储密钥", hostID, actionLabel),
		db.LogResultSuccess,
	); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "审计记录未能保存，未提供密钥"})
		return
	}

	if action == "reveal" {
		c.JSON(http.StatusOK, gin.H{"key": key, "fingerprint": record.Fingerprint})
		return
	}

	client, err := nodeSecurityClient(ctx, hostID)
	if err == nil {
		var state json.RawMessage
		state, err = client.Request(ctx, http.MethodPost, "/1.0/payincus/security/unlock", map[string]any{
			"dataset": record.Dataset,
			"key":     key,
		})
		if err == nil {
			c.JSON(http.StatusOK, gin.H{"state": state})
			return
		}
	}

	status := http.StatusConflict
	var apiError *lib.IncusApiError
	if errors.As(err, &apiError) {
		status = apiError.StatusCode
		if status == http.StatusUnauthorized {
			status = http.StatusBadGateway
		}
	}
	detail := lib.NodeErrorDetail(err, nodeSecurityUnlockFallback, map[string]any{"key": key})
	// `finally { key = '' }`: Go strings cannot be zeroed, but dropping the only
	// reference keeps the plaintext out of the rest of the request's lifetime.
	key = ""
	c.JSON(status, gin.H{"error": detail})
}

// ==================== view helpers ====================

// nodeSecurityKeyResponses maps models to the GET projection.
func nodeSecurityKeyResponses(keys []models.NodeStorageKey) []nodeSecurityKeyView {
	views := make([]nodeSecurityKeyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, nodeSecurityKeyView{
			ID:          key.ID,
			Dataset:     key.Dataset,
			Fingerprint: key.Fingerprint,
			Version:     key.Version,
			CreatedAt:   storageISOTime(key.CreatedAt),
		})
	}
	return views
}

// nodeSecurityEventResponses maps models to the event projection.
func nodeSecurityEventResponses(events []models.NodeCliEvent) []nodeSecurityEventView {
	views := make([]nodeSecurityEventView, 0, len(events))
	for _, event := range events {
		views = append(views, nodeSecurityEventView{
			HostID:     event.HostID,
			EventID:    event.EventID,
			Command:    event.Command,
			UID:        event.Uid,
			PID:        event.Pid,
			Critical:   event.Critical,
			Blocked:    event.Blocked,
			OccurredAt: storageISOTime(event.OccurredAt),
			ReceivedAt: storageISOTime(event.ReceivedAt),
		})
	}
	return views
}

// nodeSecurityRecoveryTaskResponse maps the latest recovery task.
func nodeSecurityRecoveryTaskResponse(task *db.NodeRecoveryTaskView) *nodeSecurityRecoveryTaskView {
	if task == nil {
		return nil
	}
	view := &nodeSecurityRecoveryTaskView{
		ID:        task.ID,
		Status:    task.Status,
		Error:     task.Error,
		CreatedAt: storageISOTime(task.CreatedAt),
	}
	if task.FinishedAt != nil {
		finished := storageISOTime(*task.FinishedAt)
		view.FinishedAt = &finished
	}
	return view
}

// nodeSecurityDecodeNodeJSON decodes a node document into the generic shape the
// report parser inspects.
func nodeSecurityDecodeNodeJSON(raw json.RawMessage) (any, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// RegisterNodeSecurity mounts the admin node-security routes.
//
// `onRequest: [authenticate, requireAdmin]` maps to AuthenticateAdmin, which also
// runs Fastify's body-parser rules after the admin check.
func RegisterNodeSecurity(r *gin.Engine) {
	group := r.Group(nodeSecurityBase, middleware.AuthenticateAdmin())

	group.POST("/identity", nodeSecurityIdentityHandler)
	group.GET("", nodeSecurityOverviewHandler)
	group.POST("/policy", func(c *gin.Context) { nodeSecurityActionHandler(c, "policy") })
	group.POST("/recover", func(c *gin.Context) { nodeSecurityActionHandler(c, "recover") })
	group.POST("/lock", func(c *gin.Context) { nodeSecurityActionHandler(c, "lock") })
	group.POST("/keys", nodeSecurityKeysHandler)
	group.POST("/keys/reveal", func(c *gin.Context) { nodeSecurityKeyActionHandler(c, "reveal") })
	group.POST("/keys/unlock", func(c *gin.Context) { nodeSecurityKeyActionHandler(c, "unlock") })
}

func init() {
	Register(RegisterNodeSecurity)
}
