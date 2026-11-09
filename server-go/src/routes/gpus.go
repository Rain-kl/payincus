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
	"errors"
	"net/http"
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
		if user.Role != "admin" && user.ID != instance.UserID && user.ID != host.UserID {
			c.JSON(http.StatusForbidden, gin.H{"error": "无权访问该实例"})
			return
		}

		// gpuAllocations ordered by gpuDeviceId, each joined to its device row.
		var allocations []struct {
			GPUDeviceID int32 `gorm:"column:gpu_device_id"`
		}
		err = db.DB.WithContext(ctx).Raw(
			"SELECT gpu_device_id FROM instance_gpu_allocations WHERE instance_id = ? ORDER BY gpu_device_id ASC",
			instanceID,
		).Scan(&allocations).Error
		if err != nil {
			gpusInternalError(c)
			return
		}

		deviceIDs := make([]int32, 0, len(allocations)+1)
		for _, allocation := range allocations {
			deviceIDs = append(deviceIDs, allocation.GPUDeviceID)
		}
		if len(deviceIDs) == 0 && instance.GPUDeviceID != nil {
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
				gpusInternalError(c)
				return
			}
			devices = append(devices, device)
		}

		if len(devices) == 0 {
			c.JSON(http.StatusOK, gin.H{"gpu": nil})
			return
		}

		summaries := make([]lib.GpuDevice, 0, len(devices))
		for _, device := range devices {
			summaries = append(summaries, lib.GpuDevice{Name: device.Name, MemoryTotalMiB: int64(device.MemoryTotalMiB)})
		}

		c.JSON(http.StatusOK, gin.H{"gpu": gpusInstanceGpuView{
			Name:                devices[0].Name,
			MemoryTotalMiB:      devices[0].MemoryTotalMiB,
			Devices:             lib.SummarizeGpuModels(summaries),
			Count:               len(devices),
			Shared:              true,
			ReservationReleased: instance.GPUReleasedAt != nil,
			SharingNotice:       lib.GpuSharingNotice,
		}})
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
