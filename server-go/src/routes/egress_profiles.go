// Port of server/src/routes/egress-profiles.ts.
//
// Every path below is absolute: app.ts registers this plugin with no prefix
// (`await fastify.register(egressProfileRoutes)`), so the routes live directly
// under /api.
//
// The write paths are security controls, not CRUD conveniences:
//   - a saved profile must be proxy mode, so an authorized host never receives a
//     policy that routes traffic nowhere
//   - the probe URL is fetched FROM THE NODE, so it must pass the outbound
//     SSRF guard before it is stored
//   - the whole create/update/delete section runs under advisory lock 74201
//   - a host that still has a package, instance, binding or attachment cannot be
//     revoked from the profile
//   - every touched host (added, kept, or revoked) gets its revision bumped so an
//     offline node catches up on its next heartbeat instead of holding the editor
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// ==================== helpers ====================

// egressProfilesInternal reports an unexpected failure the way fastify's generic
// error handler does: production hides the detail, development surfaces it.
func egressProfilesInternal(c *gin.Context, err error) {
	message := "Internal server error"
	if !lib.IsProductionEnv() && err != nil {
		message = err.Error()
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": message})
}

// egressProfilesThrowable mirrors a thrown JS error: NetworkInputError carries
// statusCode 400, everything else falls through to 500.
func egressProfilesThrowable(c *gin.Context, err error) {
	var networkError *lib.NetworkInputError
	if errors.As(err, &networkError) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": networkError.Message})
		return
	}
	egressProfilesInternal(c, err)
}

// egressProfilesIntegerParamMessage mirrors AJV's first error for
// `{ type: 'integer', minimum, maximum }` as rendered by app.ts' setErrorHandler.
func egressProfilesIntegerParamMessage(raw string, minimum, maximum float64) string {
	if number, ok := ajvCoerceInteger(raw); ok {
		field := ajvFieldSpec{Minimum: minimum, Maximum: maximum}
		if message := ajvIntegerBoundsMessage(field, number); message != "" {
			return message
		}
	}
	return "must be integer"
}

// egressProfilesRouteID parses `:id` with the project's strict positive-id rule
// (lib.ParsePositiveRouteID) and renders the same 400 envelope AJV would.
//
// Fastify's params schema is `{ type: 'integer', minimum: 1, maximum: 2147483647 }`,
// so AJV would additionally coerce " 5 ", "0x4", "1e3" and "01". Those are
// rejected here on purpose: strict route ids are the repository-wide invariant
// (see GO_MIGRATION.md §3) and no client sends them.
func egressProfilesRouteID(c *gin.Context) (int32, bool) {
	raw := c.Param("id")
	parsed, ok := lib.ParsePositiveRouteID(raw)
	if !ok || parsed > 2147483647 {
		respondAJVValidation(c, egressProfilesIntegerParamMessage(raw, 1, 2147483647))
		return 0, false
	}
	return int32(parsed), true
}

// egressProfilesISOTime mirrors `date.toISOString()`.
func egressProfilesISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// egressProfilesJSNullableTime mirrors `policy?.reportedAt ?? null`.
func egressProfilesJSNullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return egressProfilesISOTime(*value)
}

// egressProfilesJSONObject decodes a jsonb column into a JSON object, mirroring
// the TypeScript's unchecked `policy?.status as {...}` cast (a non-object decodes
// to an empty object so the property lookups behave like `undefined`).
func egressProfilesJSONObject(raw []byte) map[string]any {
	object := map[string]any{}
	if len(raw) == 0 {
		return object
	}
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return map[string]any{}
	}
	return object
}

// egressProfilesJSONArray mirrors `value?.length ? value : []` for a property whose
// declared type is an array: anything else behaves like undefined (falsy).
func egressProfilesJSONArray(object map[string]any, key string) []any {
	array, ok := object[key].([]any)
	if !ok {
		return nil
	}
	return array
}

// egressProfilesJSONString mirrors `typeof value === 'string' ? value : undefined`
// compared with ===.
func egressProfilesJSONEqualsString(value any, expected string) bool {
	text, ok := value.(string)
	return ok && text == expected
}

// egressProfilesJSONEqualsInt mirrors `value === <integer>` for a JSON number.
func egressProfilesJSONEqualsInt(value any, expected int32) bool {
	number, ok := value.(float64)
	return ok && number == float64(expected)
}

// egressProfilesJSString mirrors `String(value)` for the `p${id}` group key.
func egressProfilesJSString(value *int32) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprint(*value)
}

// egressProfilesAnyIn mirrors `values.some(value => set.includes(value))`.
func egressProfilesAnyIn(values, set []int32) bool {
	for _, value := range values {
		if egressProfilesContains(set, value) {
			return true
		}
	}
	return false
}

// egressProfilesContains reports membership.
func egressProfilesContains(values []int32, candidate int32) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// egressProfilesFilterIn mirrors `values.filter(value => set.includes(value))`.
func egressProfilesFilterIn(values, set []int32) []int32 {
	filtered := make([]int32, 0, len(values))
	for _, value := range values {
		if egressProfilesContains(set, value) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

// ==================== body validation ====================

// egressProfilesBodyFields lists the required top-level properties in schema
// order. `hostIds` is validated by hand because the shared AJV replica has no
// array kind; the order of every check below mirrors AJV (maxItems → minItems →
// items → uniqueItems) as measured against ajv@8.
var egressProfilesBodyRequired = []string{"name", "hostIds", "egress"}

// egressProfilesBody decodes and validates the POST/PUT body exactly like Fastify's
// schema `{ type: 'object', additionalProperties: false, required: [name, hostIds,
// egress], properties: { name: {type: string, minLength: 1, maxLength: 80,
// pattern: '\\S'}, hostIds: {type: array, minItems: 1, maxItems: 1000,
// uniqueItems: true, items: {type: integer, minimum: 1, maximum: 2147483647}},
// egress: {type: object, additionalProperties: true} } }`.
//
// It returns the AJV-coerced values, or a message for respondAJVValidation.
// handled=true means the response was already written (body-parser failure).
func egressProfilesBody(c *gin.Context) (name string, hostIDs []int32, egress any, message string, handled bool) {
	body, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		// The body parser runs before schema validation in Fastify.
		respondAJVValidation(c, bodyMessage)
		return "", nil, nil, "", true
	}

	object, ok := body.Value.(map[string]any)
	if !ok {
		return "", nil, nil, "must be object", false
	}
	for _, field := range egressProfilesBodyRequired {
		if _, present := object[field]; !present {
			return "", nil, nil, field + " is required", false
		}
	}

	// name
	coercedName, nameMessage := ajvCoerceString(object["name"])
	if nameMessage != "" {
		return "", nil, nil, nameMessage, false
	}
	if jsStringLength(coercedName) > 80 {
		return "", nil, nil, "name is too long", false
	}
	if jsStringLength(coercedName) < 1 {
		return "", nil, nil, "name is too short", false
	}
	// JS `/\S/` is Unicode-aware; Go's `\S` is ASCII-only, so the predicate is
	// spelled out to keep ideographic spaces rejected like Node does.
	if egressJSTrim(coercedName) == "" {
		return "", nil, nil, "name format is invalid", false
	}

	// hostIds
	parsedHostIDs, hostMessage := egressProfilesHostIDs(object["hostIds"])
	if hostMessage != "" {
		return "", nil, nil, hostMessage, false
	}

	// egress
	egressObject, ok := object["egress"].(map[string]any)
	if !ok {
		return "", nil, nil, "must be object", false
	}

	return coercedName, parsedHostIDs, egressObject, "", false
}

// egressProfilesHostIDs mirrors the hostIds array schema, returning AJV's first
// message in AJV's own keyword order.
func egressProfilesHostIDs(value any) ([]int32, string) {
	// coerceTypes:'array' wraps a scalar into a one-element array.
	array, ok := value.([]any)
	if !ok {
		if value == nil {
			array = []any{nil}
		} else {
			array = []any{value}
		}
	}
	if len(array) > 1000 {
		return nil, "must NOT have more than 1000 items"
	}
	if len(array) < 1 {
		return nil, "must NOT have fewer than 1 items"
	}

	ids := make([]int32, 0, len(array))
	for _, item := range array {
		number, ok := ajvCoerceInteger(item)
		if !ok {
			return nil, "must be integer"
		}
		if message := ajvIntegerBoundsMessage(ajvFieldSpec{Minimum: 1, Maximum: 2147483647}, number); message != "" {
			return nil, message
		}
		ids = append(ids, int32(number))
	}

	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[i] == ids[j] {
				return nil, fmt.Sprintf("must NOT have duplicate items (items ## %d and %d are identical)", j, i)
			}
		}
	}
	return ids, ""
}

// ==================== handlers ====================

// egressProfilesInstanceEgress mirrors GET /api/instances/:id/egress.
func egressProfilesInstanceEgress(c *gin.Context) {
	instanceID, ok := egressProfilesRouteID(c)
	if !ok {
		return
	}
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	instance, err := db.EgressProfilesInstanceEgress(ctx, instanceID)
	if err != nil {
		egressProfilesInternal(c, err)
		return
	}
	if instance == nil || instance.Status == "deleted" {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}

	permission, err := lib.CheckInstancePermission(ctx,
		lib.AuthUser{ID: user.ID, Role: user.Role},
		lib.InstanceRef{UserID: instance.UserID, HostID: instance.HostID})
	if err != nil {
		egressProfilesInternal(c, err)
		return
	}
	if !permission.Allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权查看实例出口"})
		return
	}

	binding, err := db.EgressProfilesAttachmentByHostIncusName(ctx, instance.HostID, instance.IncusID)
	if err != nil {
		egressProfilesInternal(c, err)
		return
	}
	policy, err := db.EgressHostNetworkPolicyGet(ctx, instance.HostID)
	if err != nil {
		egressProfilesInternal(c, err)
		return
	}

	var statusObject map[string]any
	if policy != nil {
		statusObject = egressProfilesJSONObject(policy.Status)
	}

	// `applied = Boolean(binding && policy?.revision === policy?.appliedRevision &&
	//  status?.bindings?.some(item => item.instanceName === binding.incusName && item.generation === binding.generation))`
	applied := false
	if binding != nil && policy != nil && policy.Revision == policy.AppliedRevision && statusObject != nil {
		for _, item := range egressProfilesJSONArray(statusObject, "bindings") {
			record, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if egressProfilesJSONEqualsString(record["instanceName"], binding.IncusName) &&
				egressProfilesJSONEqualsInt(record["generation"], binding.Generation) {
				applied = true
				break
			}
		}
	}

	// `const group = status?.groups?.find(item => item.key === `p${instance.egressProfileId}`)`
	groupKey := "p" + egressProfilesJSString(instance.EgressProfileID)
	var groupState any
	groupFound := false
	if statusObject != nil {
		for _, item := range egressProfilesJSONArray(statusObject, "groups") {
			record, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if egressProfilesJSONEqualsString(record["key"], groupKey) {
				groupState = record["state"]
				groupFound = true
				break
			}
		}
	}

	// state: host → 'host'; otherwise pending until applied; direct → 'ready';
	// profile → the node-reported group state, defaulting to 'blocked'.
	var state any = "pending"
	switch {
	case instance.EgressMode == "host":
		state = "host"
	case applied && instance.EgressMode == "direct":
		state = "ready"
	case applied:
		if groupFound && groupState != nil {
			state = groupState
		} else {
			state = "blocked"
		}
	}

	var profileName any
	if instance.EgressProfileName != nil {
		profileName = *instance.EgressProfileName
	}

	var reportedAt any
	if policy != nil {
		reportedAt = egressProfilesJSNullableTime(policy.ReportedAt)
	}

	c.JSON(http.StatusOK, gin.H{
		"mode":        instance.EgressMode,
		"profileName": profileName,
		"state":       state,
		"reportedAt":  reportedAt,
	})
}

// egressProfilesList mirrors GET /api/egress/profiles.
func egressProfilesList(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	isAdmin := user.Role == "admin"

	// `own === null` for admins; an empty (not nil) slice for a user without hosts.
	var own []int32 = []int32{}
	if !isAdmin {
		owned, err := db.EgressProfilesOwnedHostIDs(ctx, user.ID)
		if err != nil {
			egressProfilesInternal(c, err)
			return
		}
		own = owned
	}

	profiles, err := db.EgressProfilesList(ctx, 1000)
	if err != nil {
		egressProfilesInternal(c, err)
		return
	}

	items := make([]gin.H, 0, len(profiles))
	for _, profile := range profiles {
		if !isAdmin && !egressProfilesAnyIn(profile.AllowedHostIDs, own) {
			continue
		}
		hostIDs := profile.AllowedHostIDs
		if !isAdmin {
			hostIDs = egressProfilesFilterIn(profile.AllowedHostIDs, own)
		}
		if hostIDs == nil {
			hostIDs = []int32{}
		}
		item := gin.H{
			"id":      profile.ID,
			"name":    profile.Name,
			"active":  profile.Active,
			"hostIds": hostIDs,
		}
		// `egress: role === 'admin' ? profile.egress : undefined` — undefined keys
		// disappear from JSON, so non-admins must not see the document at all.
		if isAdmin {
			item["egress"] = db.EgressProfilesEgressJSON(profile.Egress)
		}
		items = append(items, item)
	}

	c.JSON(http.StatusOK, gin.H{"profiles": items})
}

// egressProfilesSave mirrors POST /api/admin/egress-profiles and
// PUT /api/admin/egress-profiles/:id (the TypeScript registers one handler for
// both).
func egressProfilesSave(c *gin.Context, editing bool) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	var profileID int32
	if editing {
		parsed, ok := egressProfilesRouteID(c)
		if !ok {
			return
		}
		profileID = parsed
	}

	name, hostIDs, egressValue, message, handled := egressProfilesBody(c)
	if handled {
		return
	}
	if message != "" {
		respondAJVValidation(c, message)
		return
	}

	egress, err := lib.ValidateEgress(egressValue)
	if err != nil {
		egressProfilesThrowable(c, err)
		return
	}
	// A shared profile that is not proxy mode would authorize hosts to route
	// nowhere; the TypeScript rejects it before touching the database.
	if egress.Mode != lib.EgressModeProxy {
		egressProfilesThrowable(c, &lib.NetworkInputError{Message: "出口策略必须选择代理节点"})
		return
	}
	if _, err := lib.AssertSafeHTTPURL(ctx, egress.ProbeURL, "Probe URL"); err != nil {
		egressProfilesThrowable(c, err)
		return
	}

	egressJSON, err := json.Marshal(egress)
	if err != nil {
		egressProfilesInternal(c, err)
		return
	}
	defaultFirewall, err := json.Marshal(lib.DefaultFirewall())
	if err != nil {
		egressProfilesInternal(c, err)
		return
	}

	result, err := db.EgressProfilesSave(ctx, db.EgressProfilesSaveInput{
		Editing:         editing,
		ID:              profileID,
		Name:            egressJSTrim(name),
		HostIDs:         hostIDs,
		ProxyNodeIDs:    egress.ProxyNodeIDs,
		Egress:          egressJSON,
		DefaultFirewall: defaultFirewall,
	})
	if err != nil {
		var inputError *db.EgressInputError
		if errors.As(err, &inputError) {
			c.JSON(http.StatusBadRequest, gin.H{"error": inputError.Message})
			return
		}
		egressProfilesInternal(c, err)
		return
	}
	if !result.Found || result.Profile == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "出口策略不存在"})
		return
	}

	action := "egress.profile.create"
	if editing {
		action = "egress.profile.update"
	}
	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleHost, action,
		fmt.Sprintf("Saved egress policy %d", result.Profile.ID), db.LogResultSuccess, db.CreateLogOptions{})

	// Persisted host revisions retry on heartbeat/reconnect; slow or offline nodes
	// never hold the editor open, so the push is fire-and-forget.
	for _, hostID := range result.Targets {
		target := hostID
		go func() {
			if _, err := egressPushNetworkPolicy(context.Background(), target); err != nil {
				log.Printf("[Egress] Egress synchronization pending for host %d: %v", target, err)
			}
		}()
	}

	status := http.StatusCreated
	if editing {
		status = http.StatusAccepted
	}
	c.JSON(status, gin.H{"id": result.Profile.ID, "pendingHostIds": result.Targets})
}

// egressProfilesDelete mirrors DELETE /api/admin/egress-profiles/:id.
func egressProfilesDelete(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	profileID, ok := egressProfilesRouteID(c)
	if !ok {
		return
	}

	count, referenced, err := db.EgressProfilesDelete(ctx, profileID)
	if err != nil {
		// `Prisma.PrismaClientKnownRequestError` P2003/P2014: a foreign key still
		// points at the profile.
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "23503" {
			c.JSON(http.StatusConflict, gin.H{"error": "出口策略仍被引用，无法删除"})
			return
		}
		egressProfilesInternal(c, err)
		return
	}
	if referenced {
		c.JSON(http.StatusConflict, gin.H{"error": "出口策略仍被引用，无法删除"})
		return
	}
	if count == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "出口策略不存在"})
		return
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleHost, "egress.profile.delete",
		fmt.Sprintf("Deleted egress policy %d", profileID), db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// RegisterEgressProfiles mounts the egress-profile routes (no prefix, matching
// app.ts).
func RegisterEgressProfiles(r *gin.Engine) {
	r.GET("/api/instances/:id/egress", middleware.Authenticate(), egressProfilesInstanceEgress)
	r.GET("/api/egress/profiles", middleware.Authenticate(), egressProfilesList)

	admin := r.Group("/api/admin/egress-profiles",
		middleware.Authenticate(), middleware.RequireAdmin())
	admin.POST("", func(c *gin.Context) { egressProfilesSave(c, false) })
	admin.PUT("/:id", func(c *gin.Context) { egressProfilesSave(c, true) })
	admin.DELETE("/:id", egressProfilesDelete)
}

func init() {
	Register(RegisterEgressProfiles)
}
