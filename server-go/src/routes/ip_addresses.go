// Port of server/src/routes/ip-addresses.ts.
//
// Mounted at prefix /api in app.ts, so the paths are /api/instances/:instanceId/…
// This module has no `/` root route, so there is no trailing-slash twin to mount.
//
// Route params are named `:id` instead of the TypeScript's `:instanceId`: gin's
// radix tree allows exactly one wildcard name per path position, and
// /api/instances/:id/... is already registered by routes/snapshots.go (and will be
// by routes/instances.go). A different name panics at startup once both modules
// are mounted. The child params keep their TypeScript names (`:ipId`, `:subnetId`).
//
// Every route uses fastify.authenticateUser — regular users only; an admin gets
// 403 USER_ONLY from the middleware, which is why the `role !== 'admin'` branches
// below can never see an admin.
package routes

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// ipAddressesDeviceName is the routed IPv6 NIC in the dual-NIC architecture.
	ipAddressesDeviceName = "eth1"

	// ipAddressesMaxRandomAttempts mirrors maxAttempts in the random-allocation loop.
	ipAddressesMaxRandomAttempts = 50
	// ipAddressesMaxSubnetAttempts mirrors maxAttempts in the subnet-allocation loop.
	ipAddressesMaxSubnetAttempts = 20

	// ipAddressesDefaultParentInterface mirrors `ipv6_parent_interface || 'eth0'`.
	ipAddressesDefaultParentInterface = "eth0"
)

// ipAddressesIPView mirrors the GET /instances/:id/ips projection.
type ipAddressesIPView struct {
	ID        int32  `json:"id"`
	Address   string `json:"address"`
	Type      string `json:"type"`
	IsPrimary bool   `json:"isPrimary"`
	IsCustom  bool   `json:"isCustom"`
	Device    string `json:"device"`
	CreatedAt string `json:"createdAt"`
}

// ipAddressesCreatedIPView mirrors the POST /instances/:id/ips projection.
type ipAddressesCreatedIPView struct {
	ID        int32  `json:"id"`
	Address   string `json:"address"`
	Type      string `json:"type"`
	IsPrimary bool   `json:"isPrimary"`
	Device    string `json:"device"`
}

// ipAddressesSubnetView mirrors the GET /instances/:id/subnets projection.
type ipAddressesSubnetView struct {
	ID        int32  `json:"id"`
	CIDR      string `json:"cidr"`
	PrimaryIP string `json:"primaryIp"`
	Device    string `json:"device"`
	CreatedAt string `json:"createdAt"`
}

// ipAddressesCreatedSubnetView mirrors the POST /instances/:id/subnet projection.
type ipAddressesCreatedSubnetView struct {
	ID        int32  `json:"id"`
	CIDR      string `json:"cidr"`
	PrimaryIP string `json:"primaryIp"`
	Device    string `json:"device"`
}

// ipAddressesISOTime mirrors Date#toISOString().
func ipAddressesISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ipAddressesInternalError mirrors an uncaught exception reaching app.ts'
// setErrorHandler: production never echoes 5xx details.
func ipAddressesInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// ipAddressesRouteID parses a route id with the strict ^[1-9]\d*$ rule.
func ipAddressesRouteID(c *gin.Context, name string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(c.Param(name))
	if !ok {
		return 0, false
	}
	return int32(parsed), true
}

// ipAddressesIncusClient mirrors `getIncusClient(host)`.
func ipAddressesIncusClient(ctx context.Context, host *db.IpAddressesHost) (*lib.IncusClient, error) {
	if host == nil {
		// Unreachable with the foreign keys in place; kept so a missing join cannot
		// panic a handler.
		return nil, fmt.Errorf("host not found")
	}
	return lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:           host.URL,
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		TunnelEnabled: host.TunnelEnabled,
		HostID:        int64(host.ID),
		TargetHost:    host.TargetHost,
		TargetPort:    int(host.TargetPort),
	})
}

// ipAddressesParentInterface mirrors `host.ipv6_parent_interface || 'eth0'`.
func ipAddressesParentInterface(host *db.IpAddressesHost) string {
	if host != nil && host.IPv6ParentInterface != nil && *host.IPv6ParentInterface != "" {
		return *host.IPv6ParentInterface
	}
	return ipAddressesDefaultParentInterface
}

// ipAddressesCanAccess mirrors the repeated permission check:
// admin, instance owner, or owner of the host the instance runs on.
func ipAddressesCanAccess(ctx context.Context, user *lib.AccessTokenClaims, instance *db.IpAddressesInstance) (bool, error) {
	if user.Role == "admin" || instance.UserID == user.ID {
		return true, nil
	}
	host, err := db.IpAddressesGetHostByID(ctx, instance.HostID)
	if err != nil {
		return false, err
	}
	return host != nil && host.UserID == user.ID, nil
}

// ipAddressesBuildIPv6Config mirrors the repeated "find the primary inet6 record,
// collect the rest" block. ok is false when no primary exists, which the POST
// route treats as an internal error and the DELETE/PUT routes treat as "skip the
// node update".
func ipAddressesBuildIPv6Config(records []models.IpAddress, excludeIPID *int32) (lib.NetworkPayloadIPv6Config, bool) {
	primaryAddress := ""
	foundPrimary := false
	extra := make([]string, 0, len(records))
	for _, record := range records {
		if record.Type != models.IpTypeInet6 {
			continue
		}
		if excludeIPID != nil && record.ID == *excludeIPID {
			continue
		}
		if record.IsPrimary {
			if !foundPrimary {
				primaryAddress = record.Address
				foundPrimary = true
			}
			continue
		}
		extra = append(extra, record.Address)
	}
	if !foundPrimary {
		return lib.NetworkPayloadIPv6Config{}, false
	}
	config := lib.NetworkPayloadIPv6Config{PrimaryIP: primaryAddress}
	if len(extra) > 0 {
		config.ExtraIPs = extra
	}
	return config, true
}

// ipAddressesTruthy mirrors JavaScript truthiness for decoded JSON values.
func ipAddressesTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	case string:
		return typed != ""
	default:
		return true
	}
}

// ==================== GET /instances/:id/ips ====================

func ipAddressesListHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := ipAddressesRouteID(c, "id")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	instance, err := db.IpAddressesGetInstanceByID(ctx, instanceID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := ipAddressesCanAccess(ctx, user, instance)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if !allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	records, err := db.IpAddressesGetByInstanceID(ctx, instanceID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}

	items := make([]ipAddressesIPView, 0, len(records))
	for _, record := range records {
		items = append(items, ipAddressesIPView{
			ID:        record.ID,
			Address:   record.Address,
			Type:      string(record.Type),
			IsPrimary: record.IsPrimary,
			IsCustom:  record.IsCustom,
			Device:    record.Device,
			CreatedAt: ipAddressesISOTime(record.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{"ipAddresses": items})
}

// ==================== POST /instances/:id/ips ====================

func ipAddressesCreateHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	// Fastify validates `schema.body` before the handler runs, so a missing or
	// non-object body is a VALIDATION_ERROR even when the id is invalid too.
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, nil, []ajvFieldSpec{
		{Name: "customAddress", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	})
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	customAddress, _ := body.stringValue("customAddress")

	instanceID, ok := ipAddressesRouteID(c, "id")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	instance, err := db.IpAddressesGetInstanceByID(ctx, instanceID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := ipAddressesCanAccess(ctx, user, instance)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if !allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	if instance.Status != models.InstanceStatusRunning && instance.Status != models.InstanceStatusStopped {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStatusInvalid, "")
		return
	}

	if instance.NetworkMode == string(lib.NetworkModeNat) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "NAT mode does not support additional IPs"})
		return
	}

	host, err := db.IpAddressesGetHostByID(ctx, instance.HostID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if host == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Host not found"})
		return
	}
	if host.IPv6Subnet == nil || *host.IPv6Subnet == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Host does not have IPv6 subnet configured"})
		return
	}
	hostSubnet := *host.IPv6Subnet

	// ==================== validate the custom address before creating anything ====================
	newIPv6 := ""
	isCustom := false

	if customAddress != "" && strings.TrimSpace(customAddress) != "" {
		trimmedAddress := strings.TrimSpace(customAddress)

		// 1. format
		if !lib.IsStrictValidIpv6(trimmedAddress) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Invalid IPv6 address format",
				"code":  "INVALID_IPV6_FORMAT",
			})
			return
		}

		// 2. inside the host subnet
		if !lib.IsIpv6InSubnet(trimmedAddress, hostSubnet) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "IPv6 address must be within host subnet range (" + hostSubnet + ")",
				"code":  "IPV6_NOT_IN_SUBNET",
			})
			return
		}

		// 3. not already taken
		exists, err := db.IpAddressesExists(ctx, trimmedAddress)
		if err != nil {
			ipAddressesInternalError(c)
			return
		}
		if exists {
			c.JSON(http.StatusConflict, gin.H{
				"error": "IPv6 address already in use",
				"code":  "IPV6_ALREADY_EXISTS",
			})
			return
		}

		newIPv6 = trimmedAddress
		isCustom = true
	} else {
		attempts := 0
		for {
			generated, err := lib.GenerateRandomIPv6(hostSubnet)
			if err != nil {
				// The TypeScript lets generateRandomIPv6 throw.
				ipAddressesInternalError(c)
				return
			}
			newIPv6 = generated

			exists, err := db.IpAddressesExists(ctx, newIPv6)
			if err != nil {
				ipAddressesInternalError(c)
				return
			}
			if !exists {
				break
			}

			attempts++
			if attempts >= ipAddressesMaxRandomAttempts {
				c.JSON(http.StatusConflict, gin.H{
					"error": fmt.Sprintf("无法找到可用的 IPv6 地址，已尝试 %d 次随机生成", ipAddressesMaxRandomAttempts),
					"code":  "IPV6_POOL_EXHAUSTED",
				})
				return
			}
		}
	}

	deviceName := ipAddressesDeviceName

	// The validation above already ran under the allocation lock's protection, but
	// createIpAddress re-checks inside its own transaction, so a concurrent request
	// that slipped in between still loses cleanly with 409.
	ipRecord, err := db.IpAddressesCreate(ctx, db.IpAddressesCreateData{
		Address:    newIPv6,
		Type:       models.IpTypeInet6,
		IsPrimary:  false,
		Device:     deviceName,
		InstanceID: instanceID,
		IsCustom:   &isCustom,
	})
	if err != nil {
		if db.IpAddressesIsAlreadyExistsError(err) {
			c.JSON(http.StatusConflict, gin.H{
				"error": "IPv6 address already in use",
				"code":  "IPV6_ALREADY_EXISTS",
			})
			return
		}
		ipAddressesInternalError(c)
		return
	}

	// From here on every failure rolls the record back: the panel must not keep an
	// address the node never learned about.
	if instance.Status == models.InstanceStatusRunning || instance.Status == models.InstanceStatusStopped {
		client, err := ipAddressesIncusClient(ctx, host)
		if err != nil {
			_ = db.IpAddressesDelete(ctx, ipRecord.ID)
			ipAddressesInternalError(c)
			return
		}

		allRecords, err := db.IpAddressesGetByInstanceID(ctx, instanceID)
		if err != nil {
			_ = db.IpAddressesDelete(ctx, ipRecord.ID)
			ipAddressesInternalError(c)
			return
		}
		ipv6Config, ok := ipAddressesBuildIPv6Config(allRecords, nil)
		if !ok {
			_ = db.IpAddressesDelete(ctx, ipRecord.ID)
			ipAddressesInternalError(c)
			return
		}

		if err := lib.NetworkPayloadSyncInstanceNetwork(ctx, client, instance.IncusID, ipv6Config,
			ipAddressesParentInterface(host), instance.Status == models.InstanceStatusRunning); err != nil {
			_ = db.IpAddressesDelete(ctx, ipRecord.ID)
			ipAddressesInternalError(c)
			return
		}
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleInstance, "ip.add",
		"Added IP "+newIPv6+" to instance \""+instance.Name+"\"",
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"ipAddress": ipAddressesCreatedIPView{
			ID:        ipRecord.ID,
			Address:   newIPv6,
			Type:      "inet6",
			IsPrimary: false,
			Device:    deviceName,
		},
	})
}

// ==================== DELETE /instances/:id/ips/:ipId ====================

func ipAddressesDeleteHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := ipAddressesRouteID(c, "id")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}
	ipID, ok := ipAddressesRouteID(c, "ipId")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	ipRecord, err := db.IpAddressesGetByID(ctx, ipID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if ipRecord == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "IP address not found"})
		return
	}
	if ipRecord.InstanceID != instanceID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "IP does not belong to this instance"})
		return
	}

	instance := ipRecord.Instance
	if instance == nil {
		ipAddressesInternalError(c)
		return
	}
	allowed, err := ipAddressesCanAccess(ctx, user, instance)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if !allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	if ipRecord.IsPrimary {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot remove primary IP address"})
		return
	}

	// Remove the address from eth1 whether the instance is running or stopped; a
	// stopped instance applies the config on its next boot.
	if instance.Status == models.InstanceStatusRunning || instance.Status == models.InstanceStatusStopped {
		client, err := ipAddressesIncusClient(ctx, instance.Host)
		if err == nil {
			var allRecords []models.IpAddress
			allRecords, err = db.IpAddressesGetByInstanceID(ctx, instanceID)
			if err == nil {
				ipv6Config, found := ipAddressesBuildIPv6Config(allRecords, &ipID)
				if !found {
					// The TypeScript warns and skips the node update.
					err = nil
				} else {
					err = lib.NetworkPayloadSyncInstanceNetwork(ctx, client, instance.IncusID, ipv6Config,
						ipAddressesParentInterface(instance.Host),
						instance.Status == models.InstanceStatusRunning)
				}
			}
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "Failed to remove IP from instance network",
				"code":  "OPERATION_FAILED",
			})
			return
		}
	}

	if err := db.IpAddressesDelete(ctx, ipID); err != nil {
		ipAddressesInternalError(c)
		return
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleInstance, "ip.remove",
		"Removed IP "+ipRecord.Address+" from instance \""+instance.Name+"\"",
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ==================== PUT /instances/:id/ips/:ipId/custom ====================

func ipAddressesCustomHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	// `request.body.address?.trim()` is evaluated before the id checks: a missing
	// body is a TypeError (500), while a missing address is just undefined (400).
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	if decoded.Value == nil {
		ipAddressesInternalError(c)
		return
	}
	address := ""
	if object, ok := decoded.Value.(map[string]any); ok {
		if raw, present := object["address"]; present && raw != nil {
			text, isString := raw.(string)
			if !isString {
				// `123.trim` / `{}.trim` is not a function → TypeError.
				ipAddressesInternalError(c)
				return
			}
			address = strings.TrimSpace(text)
		}
	}

	instanceID, ok := ipAddressesRouteID(c, "id")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}
	ipID, ok := ipAddressesRouteID(c, "ipId")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	if address == "" || !lib.IsStrictValidIpv6(address) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid IPv6 address format",
			"code":  "INVALID_IPV6_FORMAT",
		})
		return
	}

	ipRecord, err := db.IpAddressesGetByID(ctx, ipID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if ipRecord == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "IP address not found"})
		return
	}
	if ipRecord.InstanceID != instanceID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "IP does not belong to this instance"})
		return
	}

	instance := ipRecord.Instance
	if instance == nil {
		ipAddressesInternalError(c)
		return
	}
	allowed, err := ipAddressesCanAccess(ctx, user, instance)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if !allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	host := instance.Host
	if host == nil || host.IPv6Subnet == nil || *host.IPv6Subnet == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Host does not have IPv6 subnet configured"})
		return
	}
	hostSubnet := *host.IPv6Subnet

	if !lib.IsIpv6InSubnet(address, hostSubnet) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "IPv6 address must be within host subnet range (" + hostSubnet + ")",
			"code":  "IPV6_NOT_IN_SUBNET",
		})
		return
	}

	exists, err := db.IpAddressesExists(ctx, address)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if exists && address != ipRecord.Address {
		c.JSON(http.StatusConflict, gin.H{
			"error": "IPv6 address already exists",
			"code":  "IPV6_ALREADY_EXISTS",
		})
		return
	}

	oldAddress := ipRecord.Address
	oldIsCustom := ipRecord.IsCustom

	// Reserve the new address through the DB lock first, so two concurrent requests
	// cannot both pass the existence check and only collide after the node update.
	markCustom := true
	_, err = db.IpAddressesUpdateAddress(ctx, db.IpAddressesUpdateAddressData{
		ID:       ipID,
		Address:  address,
		IsCustom: &markCustom,
	})
	if err != nil {
		if db.IpAddressesIsAlreadyExistsError(err) {
			c.JSON(http.StatusConflict, gin.H{
				"error": "IPv6 address already exists",
				"code":  "IPV6_ALREADY_EXISTS",
			})
			return
		}
		ipAddressesInternalError(c)
		return
	}

	// Sync to Incus; on failure roll the reservation back so the panel keeps
	// matching the real network.
	if instance.Status == models.InstanceStatusRunning || instance.Status == models.InstanceStatusStopped {
		syncFailed := false
		client, err := ipAddressesIncusClient(ctx, host)
		if err != nil {
			syncFailed = true
		} else {
			allRecords, err := db.IpAddressesGetByInstanceID(ctx, instanceID)
			if err != nil {
				syncFailed = true
			} else if ipv6Config, found := ipAddressesBuildIPv6Config(allRecords, nil); found {
				if err := lib.NetworkPayloadSyncInstanceNetwork(ctx, client, instance.IncusID, ipv6Config,
					ipAddressesParentInterface(host),
					instance.Status == models.InstanceStatusRunning); err != nil {
					syncFailed = true
				}
			}
		}
		if syncFailed {
			rollbackCustom := oldIsCustom
			if _, rollbackErr := db.IpAddressesUpdateAddress(ctx, db.IpAddressesUpdateAddressData{
				ID:       ipID,
				Address:  oldAddress,
				IsCustom: &rollbackCustom,
			}); rollbackErr != nil {
				fmt.Fprintf(gin.DefaultErrorWriter, "回滚自定义 IPv6 地址失败: %v\n", rollbackErr)
			}
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "Failed to update IP in instance network",
				"code":  "OPERATION_FAILED",
			})
			return
		}
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleInstance, "ip.custom",
		"Set custom IPv6 "+address+" (was "+oldAddress+") for instance \""+instance.Name+"\"",
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusOK, gin.H{"success": true, "address": address})
}

// ==================== POST /instances/:id/subnet ====================

func ipAddressesAllocateSubnetHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	// `const { cidr, prefix, primaryIp } = request.body` throws on undefined/null.
	if decoded.Value == nil {
		ipAddressesInternalError(c)
		return
	}
	object, _ := decoded.Value.(map[string]any)

	instanceID, ok := ipAddressesRouteID(c, "id")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	instance, err := db.IpAddressesGetInstanceByID(ctx, instanceID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := ipAddressesCanAccess(ctx, user, instance)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if !allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	if instance.NetworkMode == string(lib.NetworkModeNat) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "NAT mode does not support IPv6 subnet"})
		return
	}

	host, err := db.IpAddressesGetHostByID(ctx, instance.HostID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if host == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Host not found"})
		return
	}
	hostSubnet := ""
	if host.IPv6Subnet != nil {
		hostSubnet = *host.IPv6Subnet
	}

	// `if (inputCidr)` is a JavaScript truthiness test.
	inputCIDR := ""
	if raw, present := object["cidr"]; present && ipAddressesTruthy(raw) {
		text, isString := raw.(string)
		if !isString {
			// `cidr.split` on a non-string throws.
			ipAddressesInternalError(c)
			return
		}
		inputCIDR = text
	}

	prefixRaw, prefixPresent := object["prefix"]
	prefixAllowed := false
	if number, isNumber := prefixRaw.(float64); isNumber {
		for _, candidate := range db.Ipv6SubnetAllowedPrefixes {
			if number == float64(candidate) {
				prefixAllowed = true
			}
		}
	}

	cidr := ""
	switch {
	case inputCIDR != "":
		if !lib.NetworkPayloadIsValidIpv6Subnet(inputCIDR) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid IPv6 subnet CIDR format"})
			return
		}
		normalized, err := db.Ipv6SubnetsNormalizeCidrRange(inputCIDR)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid IPv6 subnet CIDR format"})
			return
		}
		if !ipAddressesPrefixAllowed(normalized.Prefix) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Prefix must be 112, 120, or 124"})
			return
		}
		if hostSubnet == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Host does not have IPv6 subnet configured"})
			return
		}
		if !lib.IsIpv6SubnetWithinSubnet(normalized.CIDR, hostSubnet) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "IPv6 subnet must be within host subnet range (" + hostSubnet + ")",
				"code":  "IPV6_SUBNET_NOT_IN_HOST_SUBNET",
			})
			return
		}
		cidr = normalized.CIDR

	case prefixPresent:
		// `prefix !== undefined` — null and strings reach the same rejection.
		if !prefixAllowed {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Prefix must be 112, 120, or 124"})
			return
		}
		if hostSubnet == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Host does not have IPv6 subnet configured"})
			return
		}
		prefix := int(prefixRaw.(float64))

		// Retry up to 20 times so an unlucky pick does not fail the request.
		attempts := 0
		for {
			generated, err := lib.GenerateRandomSubnet(hostSubnet, prefix)
			if err != nil {
				ipAddressesInternalError(c)
				return
			}
			cidr = generated
			exists, err := db.Ipv6SubnetsExists(ctx, cidr)
			if err != nil {
				ipAddressesInternalError(c)
				return
			}
			if !exists {
				break
			}
			attempts++
			if attempts >= ipAddressesMaxSubnetAttempts {
				break
			}
		}
		if attempts >= ipAddressesMaxSubnetAttempts {
			c.JSON(http.StatusConflict, gin.H{"error": "Unable to allocate subnet, pool may be exhausted"})
			return
		}

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Either cidr or prefix must be provided"})
		return
	}

	exists, err := db.Ipv6SubnetsExists(ctx, cidr)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if exists {
		c.JSON(http.StatusConflict, gin.H{"error": "IPv6 subnet already allocated"})
		return
	}

	allRecords, err := db.IpAddressesGetByInstanceID(ctx, instanceID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	primaryRecord := ipAddressesPrimaryIPv6(allRecords)
	if primaryRecord == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Instance does not have a primary IPv6 address"})
		return
	}

	// The subnet's primary IP: caller-provided, else the network address.
	subnetPrimaryIP := ""
	if raw, present := object["primaryIp"]; present && ipAddressesTruthy(raw) {
		text, isString := raw.(string)
		if !isString {
			// Prisma would reject a non-string primaryIp.
			ipAddressesInternalError(c)
			return
		}
		subnetPrimaryIP = text
	}
	if subnetPrimaryIP == "" {
		subnetPrimaryIP = strings.Split(cidr, "/")[0]
	}

	subnet, err := db.Ipv6SubnetsCreate(ctx, db.Ipv6SubnetsCreateData{
		CIDR:       cidr,
		PrimaryIP:  subnetPrimaryIP,
		Device:     ipAddressesStringPtr(ipAddressesDeviceName),
		InstanceID: instanceID,
	})
	if err != nil {
		if db.Ipv6SubnetsIsOverlapError(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "IPv6 subnet overlaps an allocated subnet"})
			return
		}
		ipAddressesInternalError(c)
		return
	}

	if instance.Status == models.InstanceStatusRunning || instance.Status == models.InstanceStatusStopped {
		client, err := ipAddressesIncusClient(ctx, host)
		if err == nil {
			// The TypeScript rebuilds the config from the same ipv6Records slice it
			// found the primary in, and adds the freshly allocated subnet.
			ipv6Config := lib.NetworkPayloadIPv6Config{PrimaryIP: primaryRecord.Address, Subnet: &cidr}
			extraIPs := make([]string, 0, len(allRecords))
			for _, record := range allRecords {
				if record.Type == models.IpTypeInet6 && !record.IsPrimary {
					extraIPs = append(extraIPs, record.Address)
				}
			}
			if len(extraIPs) > 0 {
				ipv6Config.ExtraIPs = extraIPs
			}
			err = lib.NetworkPayloadSyncInstanceNetwork(ctx, client, instance.IncusID, ipv6Config,
				ipAddressesParentInterface(host), instance.Status == models.InstanceStatusRunning)
		}
		if err != nil {
			_ = db.Ipv6SubnetsDelete(ctx, subnet.ID)
			ipAddressesInternalError(c)
			return
		}
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleInstance, "subnet.allocate",
		"Allocated IPv6 subnet "+cidr+" to instance \""+instance.Name+"\"",
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"subnet": ipAddressesCreatedSubnetView{
			ID:        subnet.ID,
			CIDR:      subnet.CIDR,
			PrimaryIP: subnet.PrimaryIP,
			Device:    subnet.Device,
		},
	})
}

// ==================== DELETE /instances/:id/subnet/:subnetId ====================

func ipAddressesDeleteSubnetHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := ipAddressesRouteID(c, "id")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}
	subnetID, ok := ipAddressesRouteID(c, "subnetId")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	instance, err := db.IpAddressesGetInstanceByID(ctx, instanceID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := ipAddressesCanAccess(ctx, user, instance)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if !allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	subnets, err := db.Ipv6SubnetsGetByInstanceID(ctx, instanceID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	var subnet *models.Ipv6Subnet
	for index := range subnets {
		if subnets[index].ID == subnetID {
			subnet = &subnets[index]
			break
		}
	}
	if subnet == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Subnet not found"})
		return
	}

	// Sync to Incus (drop ipv6.routes).
	host, err := db.IpAddressesGetHostByID(ctx, instance.HostID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if host != nil && (instance.Status == models.InstanceStatusRunning || instance.Status == models.InstanceStatusStopped) {
		client, err := ipAddressesIncusClient(ctx, host)
		if err == nil {
			var allRecords []models.IpAddress
			allRecords, err = db.IpAddressesGetByInstanceID(ctx, instanceID)
			if err == nil {
				// No subnet in the config → ipv6.routes is removed.
				if ipv6Config, found := ipAddressesBuildIPv6Config(allRecords, nil); found {
					err = lib.NetworkPayloadSyncInstanceNetwork(ctx, client, instance.IncusID, ipv6Config,
						ipAddressesParentInterface(host),
						instance.Status == models.InstanceStatusRunning)
				}
			}
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "Failed to remove subnet from instance network",
				"code":  "OPERATION_FAILED",
			})
			return
		}
	}

	if err := db.Ipv6SubnetsDelete(ctx, subnetID); err != nil {
		ipAddressesInternalError(c)
		return
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleInstance, "subnet.remove",
		"Removed IPv6 subnet "+subnet.CIDR+" from instance \""+instance.Name+"\"",
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ==================== GET /instances/:id/subnets ====================

func ipAddressesListSubnetsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := ipAddressesRouteID(c, "id")
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	instance, err := db.IpAddressesGetInstanceByID(ctx, instanceID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	allowed, err := ipAddressesCanAccess(ctx, user, instance)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}
	if !allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	subnets, err := db.Ipv6SubnetsGetByInstanceID(ctx, instanceID)
	if err != nil {
		ipAddressesInternalError(c)
		return
	}

	items := make([]ipAddressesSubnetView, 0, len(subnets))
	for _, subnet := range subnets {
		items = append(items, ipAddressesSubnetView{
			ID:        subnet.ID,
			CIDR:      subnet.CIDR,
			PrimaryIP: subnet.PrimaryIP,
			Device:    subnet.Device,
			CreatedAt: ipAddressesISOTime(subnet.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{"subnets": items})
}

// ipAddressesPrimaryIPv6 mirrors `ipv6Records.find(ip => ip.isPrimary)`.
func ipAddressesPrimaryIPv6(records []models.IpAddress) *models.IpAddress {
	for index := range records {
		if records[index].Type == models.IpTypeInet6 && records[index].IsPrimary {
			return &records[index]
		}
	}
	return nil
}

// ipAddressesPrefixAllowed mirrors IPV6_SUBNET_ALLOWED_PREFIXES.includes(prefix).
func ipAddressesPrefixAllowed(prefix int) bool {
	for _, candidate := range db.Ipv6SubnetAllowedPrefixes {
		if prefix == candidate {
			return true
		}
	}
	return false
}

// ipAddressesStringPtr returns a pointer to value.
func ipAddressesStringPtr(value string) *string { return &value }

// RegisterIPAddresses mounts the IP address module under /api.
func RegisterIPAddresses(r *gin.Engine) {
	group := r.Group("/api", middleware.AuthenticateUser())

	group.GET("/instances/:id/ips", ipAddressesListHandler)
	group.POST("/instances/:id/ips", ipAddressesCreateHandler)
	group.DELETE("/instances/:id/ips/:ipId", ipAddressesDeleteHandler)
	group.PUT("/instances/:id/ips/:ipId/custom", ipAddressesCustomHandler)
	group.POST("/instances/:id/subnet", ipAddressesAllocateSubnetHandler)
	group.DELETE("/instances/:id/subnet/:subnetId", ipAddressesDeleteSubnetHandler)
	group.GET("/instances/:id/subnets", ipAddressesListSubnetsHandler)
}

func init() { Register(RegisterIPAddresses) }
