// Port of server/src/routes/gpus.ts.
//
// app.ts mounts this plugin as `fastify.register(gpuRoutes, { prefix: '/api' })`,
// so the three paths are absolute below /api. Gin shares one wildcard node per path
// position, so the host parameter is named `:hostId` to match the routes already
// mounted under /api/hosts (batch-config, redeem-codes, storage-encryption, egress);
// the Fastify source calls it `:id`. The instance parameter stays `:id`, matching
// snapshots.ts. Every route authenticates in
// onRequest, and the two `:id` routes also declare a params schema — Fastify
// validates (and coerces) route params AFTER the auth hook, so an unauthenticated
// request with a bad id gets 401, not 400. The Go middleware order reproduces that.
//
// PUT /api/hosts/:id/gpus/:gpuId is the money-adjacent path: it changes how many
// instances may share a physical card. The TypeScript wraps the read-check-write in
// `prisma.$transaction` and takes `SELECT ... FOR UPDATE` on the host_gpus row
// before counting reservations, so two admins cannot both lower the seat count
// below the live allocation. That lock is reproduced here with a GORM transaction
// on the shared pgx pool.
package routes

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// gpusIDMaximum mirrors the `maximum: 2147483647` bound shared by every id schema
// in the TypeScript route.
const gpusIDMaximum = 2147483647

// gpusIDSpec is the shared id schema: integer between 1 and 2^31-1.
func gpusIDSpec(name string) ajvFieldSpec {
	return ajvFieldSpec{
		Name:      name,
		Kind:      ajvKindInteger,
		MinLength: -1,
		MaxLength: -1,
		Minimum:   1,
		Maximum:   gpusIDMaximum,
	}
}

// gpusValidateRouteID validates one path parameter through the shared AJV replica.
// Fastify coerces the string, so "01" is accepted as 1 and "1.5"/"abc" are 400s.
func gpusValidateRouteID(c *gin.Context, name string) (int32, bool) {
	validated, message := validateAJVObject(
		map[string]any{name: c.Param(name)},
		[]string{name},
		[]ajvFieldSpec{gpusIDSpec(name)},
	)
	if message != "" {
		respondAJVValidation(c, message)
		return 0, false
	}
	number, _ := validated.Values[name].(float64)
	return int32(number), true
}

// gpusAgentView mirrors `select: { enabled, version, lastSeenAt }`.
type gpusAgentView struct {
	Enabled    bool    `json:"enabled"`
	Version    *string `json:"version"`
	LastSeenAt *string `json:"lastSeenAt"`
}

// gpusInstanceGpuView mirrors the `gpu` object returned by GET /instances/:id/gpu.
type gpusInstanceGpuView struct {
	Name                string                `json:"name"`
	MemoryTotalMiB      int32                 `json:"memoryTotalMiB"`
	Devices             []lib.GpuModelSummary `json:"devices"`
	Count               int                   `json:"count"`
	Shared              bool                  `json:"shared"`
	ReservationReleased bool                  `json:"reservationReleased"`
	SharingNotice       string                `json:"sharingNotice"`
}

// gpusLiveGpuDeviceView mirrors listIncusGpuDevices(live) enriched with hostMatched.
type gpusLiveGpuDeviceView struct {
	Name        string  `json:"name"`
	Pci         *string `json:"pci"`
	Gputype     *string `json:"gputype"`
	HostMatched bool    `json:"hostMatched"`
}

// gpusManagementView mirrors the `management` object returned to admin / host owners.
type gpusManagementView struct {
	CanManage           bool                    `json:"canManage"`
	InstanceID          int32                   `json:"instanceId"`
	HostID              int32                   `json:"hostId"`
	InstanceStatus      string                  `json:"instanceStatus"`
	InstanceType        string                  `json:"instanceType"`
	Privileged          bool                    `json:"privileged"`
	Supported           bool                    `json:"supported"`
	AllocatedDeviceIDs  []int32                 `json:"allocatedDeviceIds"`
	ReservationReleased bool                    `json:"reservationReleased"`
	HostGpus            []db.GpusHostGpu        `json:"hostGpus"`
	LiveGpuDevices      []gpusLiveGpuDeviceView `json:"liveGpuDevices"`
	LiveSyncError       *string                 `json:"liveSyncError"`
}

type gpusLiveOverride struct {
	Live      map[string]any
	SyncError *string
}

var (
	gpusGetIncusClientForHost = func(ctx context.Context, host *db.HostsRecord) (*lib.IncusClient, error) {
		return lib.GetIncusClientForHost(ctx, host)
	}
	gpusFetchLiveInstance = lib.IncusInstanceFetchRaw
	gpusRunningHotplugRe  = regexp.MustCompile(`(?i)running|stopped|hotplug`)
)

func gpusIncusStatusCode(statusCode int) int {
	switch statusCode {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable:
		return statusCode
	default:
		return http.StatusBadGateway
	}
}

func gpusFormatInstanceSummary(devices []models.HostGpu, reservationReleased bool) *gpusInstanceGpuView {
	if len(devices) == 0 {
		return nil
	}
	summaries := make([]lib.GpuDevice, 0, len(devices))
	for _, device := range devices {
		summaries = append(summaries, lib.GpuDevice{Name: device.Name, MemoryTotalMiB: int64(device.MemoryTotalMiB)})
	}
	return &gpusInstanceGpuView{
		Name:                devices[0].Name,
		MemoryTotalMiB:      devices[0].MemoryTotalMiB,
		Devices:             lib.SummarizeGpuModels(summaries),
		Count:               len(devices),
		Shared:              true,
		ReservationReleased: reservationReleased,
		SharingNotice:       lib.GpuSharingNotice,
	}
}

func gpusBuildInstanceGpuResponse(
	ctx context.Context,
	instanceID int32,
	includeManagement bool,
	liveOverride *gpusLiveOverride,
) (gin.H, bool, error) {
	var instance models.Instance
	err := db.DB.WithContext(ctx).
		Select("id", "user_id", "host_id", "package_id", "incus_id", "status", "gpu_device_id", "gpu_released_at").
		Where("id = ?", instanceID).First(&instance).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && string(instance.Status) == "deleted") {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	var allocations []struct {
		GPUDeviceID int32 `gorm:"column:gpu_device_id"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT gpu_device_id FROM instance_gpu_allocations WHERE instance_id = ? ORDER BY gpu_device_id ASC",
		instanceID,
	).Scan(&allocations).Error; err != nil {
		return nil, false, err
	}

	allocIDs := make([]int32, 0, len(allocations))
	for _, allocation := range allocations {
		allocIDs = append(allocIDs, allocation.GPUDeviceID)
	}
	deviceIDs := make([]int32, 0, len(allocIDs)+1)
	if len(allocIDs) > 0 {
		deviceIDs = append(deviceIDs, allocIDs...)
	} else if instance.GPUDeviceID != nil {
		deviceIDs = append(deviceIDs, *instance.GPUDeviceID)
	}

	devices := make([]models.HostGpu, 0, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		var device models.HostGpu
		err := db.DB.WithContext(ctx).Where("id = ?", deviceID).First(&device).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		devices = append(devices, device)
	}

	gpu := gpusFormatInstanceSummary(devices, instance.GPUReleasedAt != nil)
	if !includeManagement {
		return gin.H{"gpu": gpu}, true, nil
	}

	hostGpus, err := db.GpusListHostGpus(ctx, instance.HostID)
	if err != nil {
		return nil, false, err
	}

	var live map[string]any
	var liveSyncError *string
	if liveOverride != nil {
		live = liveOverride.Live
		liveSyncError = liveOverride.SyncError
	} else {
		host, hostErr := db.HostsGetByID(ctx, instance.HostID)
		if hostErr != nil {
			msg := lib.NodeErrorDetail(hostErr, "无法读取节点实例 GPU 配置", nil)
			liveSyncError = &msg
		} else if host == nil {
			msg := "无法读取节点实例 GPU 配置"
			liveSyncError = &msg
		} else {
			client, clientErr := gpusGetIncusClientForHost(ctx, host)
			if clientErr != nil {
				msg := lib.NodeErrorDetail(clientErr, "无法读取节点实例 GPU 配置", nil)
				liveSyncError = &msg
			} else {
				fetched, fetchErr := gpusFetchLiveInstance(ctx, client, instance.IncusID)
				if fetchErr != nil {
					msg := lib.NodeErrorDetail(fetchErr, "无法读取节点实例 GPU 配置", nil)
					liveSyncError = &msg
				} else {
					live = fetched
				}
			}
		}
	}

	var pkg struct {
		InstanceType string `gorm:"column:instance_type"`
		Privileged   bool   `gorm:"column:privileged"`
	}
	if instance.PackageID != nil {
		if err := db.DB.WithContext(ctx).Raw(
			"SELECT instance_type, privileged FROM packages WHERE id = ? LIMIT 1",
			*instance.PackageID,
		).Scan(&pkg).Error; err != nil {
			return nil, false, err
		}
	}

	effectiveInstanceType := "container"
	if live != nil {
		if t, _ := live["type"].(string); t == "virtual-machine" {
			effectiveInstanceType = "vm"
		}
	} else if pkg.InstanceType == "vm" {
		effectiveInstanceType = "vm"
	}

	privileged := pkg.Privileged
	if live != nil {
		privileged = false
		if rawCfg, ok := live["config"].(map[string]any); ok {
			if val, _ := rawCfg["security.privileged"].(string); val == "true" {
				privileged = true
			}
		}
	}

	hostPciSet := make(map[string]struct{}, len(hostGpus))
	for _, card := range hostGpus {
		hostPciSet[strings.ToLower(card.PciAddress)] = struct{}{}
	}

	var liveGpuDevices []gpusLiveGpuDeviceView
	if live != nil {
		rawDevs := lib.ListIncusGpuDevices(live)
		liveGpuDevices = make([]gpusLiveGpuDeviceView, 0, len(rawDevs))
		for _, dev := range rawDevs {
			matched := false
			if dev.Pci != nil {
				_, matched = hostPciSet[strings.ToLower(*dev.Pci)]
			}
			liveGpuDevices = append(liveGpuDevices, gpusLiveGpuDeviceView{
				Name:        dev.Name,
				Pci:         dev.Pci,
				Gputype:     dev.Gputype,
				HostMatched: matched,
			})
		}
	}

	return gin.H{
		"gpu": gpu,
		"management": gpusManagementView{
			CanManage:           true,
			InstanceID:          instance.ID,
			HostID:              instance.HostID,
			InstanceStatus:      string(instance.Status),
			InstanceType:        effectiveInstanceType,
			Privileged:          privileged,
			Supported:           effectiveInstanceType == "container" && !privileged,
			AllocatedDeviceIDs:  db.GpusAllocatedIDs(instance.GPUDeviceID, allocIDs),
			ReservationReleased: instance.GPUReleasedAt != nil,
			HostGpus:            hostGpus,
			LiveGpuDevices:      liveGpuDevices,
			LiveSyncError:       liveSyncError,
		},
	}, true, nil
}

// gpusParsePutInstanceGpuBody validates the PUT /instances/:id/gpu request body
// against the TS AJV schema (additionalProperties: false, required: ['gpuShared'],
// gpuDeviceIds: array of idSchema maxItems 64).
func gpusParsePutInstanceGpuBody(decoded any) (map[string]any, string) {
	decoded = unwrapAJVRootArray(decoded)
	obj, ok := decoded.(map[string]any)
	if !ok || obj == nil {
		return nil, "must be object"
	}
	for key := range obj {
		if key != "gpuShared" && key != "gpuDeviceIds" {
			return nil, "must NOT have additional properties"
		}
	}
	validated, msg := validateAJVObject(obj, []string{"gpuShared"}, []ajvFieldSpec{
		{Name: "gpuShared", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
	})
	if msg != "" {
		return nil, msg
	}
	out := map[string]any{
		"gpuShared": validated.boolValue("gpuShared"),
	}
	if rawIDs, exists := obj["gpuDeviceIds"]; exists {
		arr, ok := rawIDs.([]any)
		if !ok {
			return nil, "must be array"
		}
		if len(arr) > 64 {
			return nil, "must NOT have more than 64 items"
		}
		coerced := make([]any, 0, len(arr))
		for _, item := range arr {
			itemValidated, itemMsg := validateAJVObject(
				map[string]any{"gpuDeviceId": item},
				[]string{"gpuDeviceId"},
				[]ajvFieldSpec{gpusIDSpec("gpuDeviceId")},
			)
			if itemMsg != "" {
				return nil, itemMsg
			}
			coerced = append(coerced, itemValidated.Values["gpuDeviceId"])
		}
		out["gpuDeviceIds"] = coerced
	}
	return out, ""
}

// gpusHostAccess is the projection GET /hosts/:id/gpus authorizes against.
type gpusHostAccess struct {
	UserID       int32
	InstanceType string
	Agent        *gpusAgentView
}

func gpusInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// RegisterGpus mounts the shared-GPU routes (prefix /api, matching app.ts).
func RegisterGpus(r *gin.Engine) {
	group := r.Group("/api")

	group.GET("/hosts/:hostId/gpus", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		hostID, ok := gpusValidateRouteID(c, "hostId")
		if !ok {
			return
		}

		var host models.Host
		err := db.DB.WithContext(ctx).Select("id", "user_id").Where("id = ?", hostID).First(&host).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "宿主机不存在"})
			return
		}
		if err != nil {
			gpusInternalError(c)
			return
		}

		user := middleware.CurrentUser(c)
		if user.Role != "admin" && user.ID != host.UserID {
			c.JSON(http.StatusForbidden, gin.H{"error": "无权访问该宿主机"})
			return
		}

		var agent *gpusAgentView
		var agentRow models.HostAgent
		err = db.DB.WithContext(ctx).
			Select("enabled", "version", "last_seen_at").
			Where("host_id = ?", hostID).First(&agentRow).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			agent = nil
		case err != nil:
			gpusInternalError(c)
			return
		default:
			view := gpusAgentView{Enabled: agentRow.Enabled, Version: agentRow.Version}
			if agentRow.LastSeenAt != nil {
				formatted := gpusISOTime(*agentRow.LastSeenAt)
				view.LastSeenAt = &formatted
			}
			agent = &view
		}

		gpus, err := db.GpusListHostGpus(ctx, hostID)
		if err != nil {
			gpusInternalError(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"gpus":          gpus,
			"agent":         agent,
			"sharingNotice": lib.GpuSharingNotice,
		})
	})

	group.PUT("/hosts/:hostId/gpus/:gpuId", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		hostID, ok := gpusValidateRouteID(c, "hostId")
		if !ok {
			return
		}
		gpuID, ok := gpusValidateRouteID(c, "gpuId")
		if !ok {
			return
		}

		body, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}
		validated, validationMessage := validateAJVObject(
			body.Value,
			[]string{"sharingEnabled", "maxInstances"},
			[]ajvFieldSpec{
				{Name: "sharingEnabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
				{Name: "maxInstances", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 1000},
			},
		)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}
		sharingEnabled := validated.boolValue("sharingEnabled")
		maxInstances := int32(validated.Values["maxInstances"].(float64))

		var host models.Host
		err := db.DB.WithContext(ctx).
			Select("id", "user_id", "instance_type").
			Where("id = ?", hostID).First(&host).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "宿主机不存在"})
			return
		}
		if err != nil {
			gpusInternalError(c)
			return
		}

		user := middleware.CurrentUser(c)
		if user.Role != "admin" && user.ID != host.UserID {
			c.JSON(http.StatusForbidden, gin.H{"error": "无权修改该宿主机"})
			return
		}
		if string(host.InstanceType) == "vm" && sharingEnabled {
			c.JSON(http.StatusBadRequest, gin.H{"error": "共享 GPU 仅支持 LXC 宿主机"})
			return
		}

		result := ""
		err = db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(
				"SELECT id FROM host_gpus WHERE id = ? AND host_id = ? FOR UPDATE",
				gpuID, hostID,
			).Error; err != nil {
				return err
			}

			var gpu models.HostGpu
			err := tx.Select("id", "host_id").Where("id = ? AND host_id = ?", gpuID, hostID).First(&gpu).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				result = "not_found"
				return nil
			}
			if err != nil {
				return err
			}

			used, err := db.GpusCountReservations(ctx, tx, gpu.ID, nil)
			if err != nil {
				return err
			}
			if used > int64(maxInstances) {
				result = "in_use"
				return nil
			}

			if err := tx.Model(&models.HostGpu{}).Where("id = ?", gpu.ID).
				Updates(map[string]any{
					"sharing_enabled": sharingEnabled,
					"max_instances":   maxInstances,
				}).Error; err != nil {
				return err
			}
			result = "ok"
			return nil
		})
		if err != nil {
			gpusInternalError(c)
			return
		}

		switch result {
		case "not_found":
			c.JSON(http.StatusNotFound, gin.H{"error": "GPU 不存在"})
			return
		case "in_use":
			c.JSON(http.StatusConflict, gin.H{"error": "共享名额不能低于当前已分配实例数（包含已停止和封停实例）"})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, db.LogModuleHost, "host.gpu.configure",
			"GPU "+gpusItoa(int64(gpuID))+": sharing="+gpusBoolString(sharingEnabled)+", maxInstances="+gpusItoa(int64(maxInstances)),
			db.LogResultSuccess, db.CreateLogOptions{})
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	group.GET("/instances/:id/gpu", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		instanceID, ok := gpusValidateRouteID(c, "id")
		if !ok {
			return
		}

		var instance models.Instance
		err := db.DB.WithContext(ctx).
			Select("id", "user_id", "host_id", "status", "gpu_device_id", "gpu_released_at").
			Where("id = ?", instanceID).First(&instance).Error
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && string(instance.Status) == "deleted") {
			c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
			return
		}
		if err != nil {
			gpusInternalError(c)
			return
		}

		var host struct {
			UserID int32 `gorm:"column:user_id"`
		}
		err = db.DB.WithContext(ctx).Raw("SELECT user_id FROM hosts WHERE id = ? LIMIT 1", instance.HostID).Scan(&host).Error
		if err != nil {
			gpusInternalError(c)
			return
		}

		user := middleware.CurrentUser(c)
		isManager := user.Role == "admin" || user.ID == host.UserID
		if !isManager && user.ID != instance.UserID {
			c.JSON(http.StatusForbidden, gin.H{"error": "无权访问该实例"})
			return
		}

		payload, found, err := gpusBuildInstanceGpuResponse(ctx, instance.ID, isManager, nil)
		if err != nil {
			gpusInternalError(c)
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
			return
		}
		c.JSON(http.StatusOK, payload)
	})

	group.PUT("/instances/:id/gpu", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		instanceID, ok := gpusValidateRouteID(c, "id")
		if !ok {
			return
		}

		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}
		parsedBody, validationMessage := gpusParsePutInstanceGpuBody(decoded.Value)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		var instance models.Instance
		err := db.DB.WithContext(ctx).
			Select("id", "user_id", "host_id", "incus_id", "status").
			Where("id = ?", instanceID).First(&instance).Error
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && string(instance.Status) == "deleted") {
			c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
			return
		}
		if err != nil {
			gpusInternalError(c)
			return
		}

		host, err := db.HostsGetByID(ctx, instance.HostID)
		if err != nil {
			gpusInternalError(c)
			return
		}
		user := middleware.CurrentUser(c)
		isManager := user.Role == "admin" || (host != nil && user.ID == host.UserID)
		if !isManager {
			c.JSON(http.StatusForbidden, gin.H{"error": "无权修改该实例的 GPU 共享配置"})
			return
		}

		locked, err := instancesCheckTransferLock(c, instance.ID)
		if err != nil {
			gpusInternalError(c)
			return
		}
		if locked {
			return
		}

		var activeRestore struct{ ID int32 }
		if err := db.DB.WithContext(ctx).Raw(
			`SELECT id FROM restore_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') LIMIT 1`,
			instance.ID,
		).Scan(&activeRestore).Error; err != nil {
			gpusInternalError(c)
			return
		}
		var activeUpload struct{ ID int32 }
		if err := db.DB.WithContext(ctx).Raw(
			`SELECT id FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') LIMIT 1`,
			instance.ID,
		).Scan(&activeUpload).Error; err != nil {
			gpusInternalError(c)
			return
		}
		activeTask, err := db.GetActiveTaskForInstance(ctx, instance.ID)
		if err != nil {
			gpusInternalError(c)
			return
		}
		if activeRestore.ID != 0 || activeUpload.ID != 0 || activeTask != nil {
			c.JSON(http.StatusConflict, gin.H{
				"error": "实例有正在执行的任务，请等待完成后再修改 GPU 配置",
				"code":  "TASK_IN_PROGRESS",
			})
			return
		}

		sharedPtr, _, deviceIDs, _, gpuErr := lib.NormalizeGpuInput(parsedBody, false)
		if gpuErr != nil {
			var ge *lib.GpuError
			if errors.As(gpuErr, &ge) {
				c.JSON(ge.StatusCode(), gin.H{"error": ge.Error(), "code": ge.Code()})
				return
			}
			gpusInternalError(c)
			return
		}
		gpuShared := sharedPtr != nil && *sharedPtr
		var selectedDeviceIDs []int32
		if gpuShared {
			selectedDeviceIDs = deviceIDs
		}

		if host == nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "无法连接宿主机以更新 GPU 配置"})
			return
		}
		client, err := gpusGetIncusClientForHost(ctx, host)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": lib.NodeErrorDetail(err, "无法连接宿主机以更新 GPU 配置", nil)})
			return
		}

		var live map[string]any
		fetched, err := gpusFetchLiveInstance(ctx, client, instance.IncusID)
		if err != nil {
			var apiErr *lib.IncusApiError
			missingOnNode := errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
			if !(!gpuShared && missingOnNode && string(instance.Status) == "error") {
				code := http.StatusBadGateway
				if errors.As(err, &apiErr) {
					code = gpusIncusStatusCode(apiErr.StatusCode)
				}
				c.JSON(code, gin.H{"error": lib.NodeErrorDetail(err, "读取节点实例配置失败", nil)})
				return
			}
		} else {
			live = fetched
		}

		var liveInstanceType *string
		var livePrivileged *bool
		if live != nil {
			t := "container"
			if v, _ := live["type"].(string); v == "virtual-machine" {
				t = "vm"
			}
			liveInstanceType = &t
			priv := false
			if cfg, ok := live["config"].(map[string]any); ok {
				if val, _ := cfg["security.privileged"].(string); val == "true" {
					priv = true
				}
			}
			livePrivileged = &priv
		}

		incusUpdated := false
		pciAddresses, hasGPUs, allocErr := db.GpusUpdateInstanceAllocation(ctx, db.GpusUpdateAllocationInput{
			InstanceID:       instance.ID,
			GpuShared:        gpuShared,
			GpuDeviceIDs:     selectedDeviceIDs,
			LiveInstanceType: liveInstanceType,
			LivePrivileged:   livePrivileged,
			SyncIncus: func(syncCtx context.Context, pcis []string) error {
				if live == nil {
					return nil
				}
				if pcis == nil {
					if !lib.ClearGpuDevices(live) {
						return nil
					}
				} else {
					if err := lib.ApplyGpuDevice(live, pcis); err != nil {
						return err
					}
				}
				escaped := url.PathEscape(instance.IncusID)
				if _, err := client.Request(syncCtx, http.MethodPut, "/1.0/instances/"+escaped, live, 30*time.Second, live); err != nil {
					detail := err.Error()
					if string(instance.Status) == "running" && gpusRunningHotplugRe.MatchString(detail) {
						msg := "实例正在运行且节点不支持热卸载 GPU，请先停止实例后再取消共享"
						if pcis != nil {
							msg = "实例正在运行且节点不支持热挂载 GPU，请先停止实例后再分配"
						}
						return &db.GpusError{
							Message:    msg,
							StatusCode: http.StatusConflict,
							Code:       "INSTANCE_RUNNING",
						}
					}
					statusCode := http.StatusBadGateway
					var apiErr *lib.IncusApiError
					if errors.As(err, &apiErr) {
						statusCode = gpusIncusStatusCode(apiErr.StatusCode)
					}
					return &db.GpusError{
						Message:    lib.NodeErrorDetail(err, "同步节点 Incus GPU 配置失败", nil),
						StatusCode: statusCode,
					}
				}
				incusUpdated = true
				return nil
			},
		})
		if allocErr != nil {
			var ge *db.GpusError
			if errors.As(allocErr, &ge) {
				c.JSON(ge.HTTPStatus(), gin.H{"error": ge.Error(), "code": ge.ErrorCode()})
				return
			}
			var libErr *lib.GpuError
			if errors.As(allocErr, &libErr) {
				c.JSON(libErr.StatusCode(), gin.H{"error": libErr.Error(), "code": libErr.Code()})
				return
			}
			gpusInternalError(c)
			return
		}

		logDetail := "Instance #" + gpusItoa(int64(instance.ID)) + " (" + instance.IncusID + ") cleared shared GPU allocation"
		if hasGPUs {
			logDetail = "Instance #" + gpusItoa(int64(instance.ID)) + " (" + instance.IncusID + ") assigned shared GPU PCIs: " + strings.Join(pciAddresses, ", ")
		}
		actorID := int64(user.ID)
		instID := instance.ID
		db.CreateLog(ctx, &actorID, db.LogModuleInstance, "instance.gpu.configure",
			logDetail, db.LogResultSuccess, db.CreateLogOptions{InstanceID: &instID})

		payload, found, err := gpusBuildInstanceGpuResponse(ctx, instance.ID, true, &gpusLiveOverride{
			Live:      live,
			SyncError: nil,
		})
		if err != nil {
			gpusInternalError(c)
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
			return
		}
		payload["success"] = true
		payload["restartRequired"] = string(instance.Status) == "running" && incusUpdated
		c.JSON(http.StatusOK, payload)
	})
}

// gpusISOTime mirrors Date.prototype.toISOString() for the agent timestamp.
func gpusISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// gpusItoa renders an integer the way a JS template literal would.
func gpusItoa(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}

// gpusBoolString mirrors String(boolean) inside the audit log template.
func gpusBoolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func init() {
	Register(RegisterGpus)
}
