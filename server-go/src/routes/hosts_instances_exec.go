// Execution helpers for host-owner / admin instance lifecycle routes in
// server-go/src/routes/hosts.go (refunds, batch delete, migration, create-for-user,
// and kernel-NAT proxy device optimization).
package routes

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// ==================== Refund Seam ====================

func hostsRefundFromInstance(ctx context.Context, input HostsRefundInput) (HostsRefundInfo, error) {
	detail := &db.InstanceDetailRecord{
		ID:            input.ID,
		PackagePlanID: input.PackagePlanID,
	}
	if input.BillingPrice != nil {
		v := input.BillingPrice.InexactFloat64()
		detail.BillingPrice = &v
	}
	if input.ExpiresAt != nil {
		iso := input.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
		detail.ExpiresAt = &iso
	}
	info, err := instancesCalculateInstanceRefund(ctx, detail)
	if err != nil {
		return HostsRefundInfo{}, err
	}
	return HostsRefundInfo{
		IsPaid:        info.IsPaid,
		RemainingDays: int(info.RemainingDays),
		RefundAmount:  info.RefundAmount,
	}, nil
}

// ==================== Managed Instance Provision Seam ====================

func hostsManagedProvision(ctx context.Context, instanceID int32, host *db.HostsSelectionRecord, options map[string]any) error {
	if host == nil {
		return errors.New("host is nil")
	}
	strField := func(key string) string {
		s, _ := options[key].(string)
		return s
	}
	strPtr := func(key string) *string {
		switch v := options[key].(type) {
		case string:
			if v != "" {
				return &v
			}
		case *string:
			if v != nil && *v != "" {
				return v
			}
		}
		return nil
	}
	i32Val := func(key string) int32 {
		switch v := options[key].(type) {
		case int32:
			return v
		case int64:
			return int32(v)
		case int:
			return int32(v)
		case float64:
			return int32(v)
		}
		return 0
	}
	i32Ptr := func(key string) *int32 {
		switch v := options[key].(type) {
		case *int32:
			return v
		case int32:
			c := v
			return &c
		case int64:
			c := int32(v)
			return &c
		case int:
			c := int32(v)
			return &c
		case float64:
			c := int32(v)
			return &c
		}
		return nil
	}
	boolVal := func(key string) bool {
		b, _ := options[key].(bool)
		return b
	}
	boolPtr := func(key string) *bool {
		switch v := options[key].(type) {
		case *bool:
			return v
		case bool:
			c := v
			return &c
		}
		return nil
	}

	var payload lib.IncusConfigPayload
	if rawPayload, ok := options["cloudInitConfig"].(lib.IncusConfigPayload); ok {
		payload = rawPayload
	} else if rawMap, ok := options["cloudInitConfig"].(map[string]any); ok {
		if ud, ok := rawMap["user.user-data"].(string); ok {
			payload.UserUserData = ud
		}
		if nc, ok := rawMap["user.network-config"].(string); ok && nc != "" {
			payload.UserNetworkConfig = nc
		}
	}

	userID := i32Val("userId")
	hostInterface := strField("hostInterface")
	if hostInterface == "" {
		hostInterface = "eth0"
	}
	storagePool := strPtr("storagePool")
	if storagePool == nil {
		def := "default"
		storagePool = &def
	}
	portLimit := i32Val("portLimit")
	cfg := instancesCreateConfig{
		AutoRemotePort:          boolPtr("autoRemotePort"),
		Name:                    strField("name"),
		Image:                   strField("image"),
		CPU:                     i32Val("cpu"),
		Memory:                  i32Val("memory"),
		Disk:                    i32Val("disk"),
		SwapEnabled:             boolVal("swapEnabled"),
		SwapSize:                i32Ptr("swapSize"),
		CloudInitConfig:         payload,
		NetworkMode:             strField("networkMode"),
		Nested:                  boolVal("nested"),
		Privileged:              boolVal("privileged"),
		PortLimit:               portLimit,
		InstanceType:            strField("instanceType"),
		SSHPort:                 i32Ptr("sshPort"),
		StoragePool:             storagePool,
		IPv4Address:             strPtr("ipv4Address"),
		IPv6Address:             strPtr("ipv6Address"),
		IPv6Gateway:             strPtr("ipv6Gateway"),
		HostInterface:           hostInterface,
		LimitsRead:              strPtr("limitsRead"),
		LimitsWrite:             strPtr("limitsWrite"),
		LimitsReadIops:          i32Ptr("limitsReadIops"),
		LimitsWriteIops:         i32Ptr("limitsWriteIops"),
		LimitsIngress:           strPtr("limitsIngress"),
		LimitsEgress:            strPtr("limitsEgress"),
		LimitsProcesses:         i32Ptr("limitsProcesses"),
		LimitsCPUPriority:       i32Ptr("limitsCpuPriority"),
		BootAutostart:           boolPtr("bootAutostart"),
		BootAutostartPriority:   i32Ptr("bootAutostartPriority"),
		BootAutostartDelay:      i32Ptr("bootAutostartDelay"),
		BootHostShutdownTimeout: i32Ptr("bootHostShutdownTimeout"),
	}
	return instancesCreateInstanceAsync(ctx, instanceID, host, cfg, userID, instancesResources{
		CPU:    cfg.CPU,
		Memory: cfg.Memory,
		Disk:   cfg.Disk,
	})
}

// ==================== Batch Delete Execution ====================

type hostsIncusClientError struct {
	Err error
}

func (e *hostsIncusClientError) Error() string { return e.Err.Error() }

func hostsClaimInstanceForDelete(ctx context.Context, instanceID int32, status models.InstanceStatus) (bool, error) {
	if db.DB == nil {
		return false, errors.New("database not initialized")
	}
	res := db.DB.WithContext(ctx).Exec(
		`UPDATE instances SET status = 'deleting', updated_at = ? WHERE id = ? AND status = ?`,
		time.Now().UTC(), instanceID, string(status),
	)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func hostsRestoreBatchDeleteClaim(ctx context.Context, instanceID int32, originalStatus models.InstanceStatus) {
	if db.DB == nil {
		return
	}
	_ = db.DB.WithContext(ctx).Exec(
		`UPDATE instances SET status = ?, updated_at = ? WHERE id = ? AND status = 'deleting'`,
		string(originalStatus), time.Now().UTC(), instanceID,
	).Error
}

func hostsHasActiveRestoreOrUpload(ctx context.Context, instanceID int32) (restoreActive, uploadActive bool, err error) {
	if db.DB == nil {
		return false, false, errors.New("database not initialized")
	}
	var count int64
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM restore_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING')`, instanceID,
	).Scan(&count).Error; err != nil {
		return false, false, err
	}
	restoreActive = count > 0
	count = 0
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING')`, instanceID,
	).Scan(&count).Error; err != nil {
		return false, false, err
	}
	uploadActive = count > 0
	return restoreActive, uploadActive, nil
}

func hostsDeleteIncusInstanceBestEffort(ctx context.Context, client *lib.IncusClient, incusID string, status models.InstanceStatus) error {
	if status == models.InstanceStatusRunning {
		if err := instancesIncusAction(ctx, client, incusID, "stop", true); err != nil {
			log.Printf("[HostBatchDelete] 停止实例 %s 失败，继续尝试删除: %v", incusID, err)
		}
	}
	return instancesIncusDelete(ctx, client, incusID)
}

func hostsExecuteBatchDelete(ctx context.Context, operatorID int32, plans []HostsBatchDeletePlan) ([]HostsBatchDeleteResult, float64, error) {
	if len(plans) == 0 {
		return []HostsBatchDeleteResult{}, 0, nil
	}
	host := plans[0].Host
	databaseOnly := plans[0].DBOnly
	reason := plans[0].Reason

	var client *lib.IncusClient
	if !databaseOnly {
		c, err := lib.GetIncusClient(ctx, hostsIncusClientOptions(host))
		if err != nil {
			return nil, 0, &hostsIncusClientError{Err: err}
		}
		client = c
	}

	results := make([]HostsBatchDeleteResult, 0, len(plans))
	totalRefundAmount := 0.0
	var deferredNotifications []func()
	releasedByPackage := map[int32]struct{ cpu, memory, count int32 }{}

	for _, p := range plans {
		instance := p.Instance
		failResult := func(msg string) {
			msgCopy := msg
			results = append(results, HostsBatchDeleteResult{
				ID:      instance.ID,
				Name:    instance.Name,
				Success: false,
				Error:   &msgCopy,
			})
		}

		pendingTransfer, err := db.HasPendingTransfer(ctx, instance.ID)
		if err != nil {
			failResult(err.Error())
			continue
		}
		if pendingTransfer {
			failResult("实例有待处理的转移请求")
			continue
		}
		restoreActive, uploadActive, err := hostsHasActiveRestoreOrUpload(ctx, instance.ID)
		if err != nil {
			failResult(err.Error())
			continue
		}
		if restoreActive {
			failResult("实例有正在进行的恢复任务")
			continue
		}
		if uploadActive {
			failResult("实例有正在进行的上传任务")
			continue
		}

		if !databaseOnly {
			claimed, claimErr := hostsClaimInstanceForDelete(ctx, instance.ID, instance.Status)
			if claimErr != nil || !claimed {
				failResult("实例正在删除或有正在进行的任务")
				continue
			}
		}

		proxySites, _ := db.ProxySitesByInstanceID(ctx, instance.ID)
		if len(proxySites) > 0 && !databaseOnly && host.CaddyEnabled {
			caddyPort := int(host.CaddyPort)
			if caddyClient, caddyErr := lib.GetCaddyClientForHost(lib.CaddyHostRef{
				ID: host.ID, CaddyEnabled: &host.CaddyEnabled, CaddyPort: &caddyPort,
			}, 0); caddyErr == nil {
				for _, site := range proxySites {
					_ = caddyClient.DeleteSite(ctx, site.Domain)
				}
			}
		}

		portMappings, _ := db.PortMappingsList(ctx, instance.ID)
		if len(portMappings) > 0 && client != nil {
			for _, m := range portMappings {
				_ = instancesIncusRemoveDevice(ctx, client, instance.IncusID, fmt.Sprintf("proxy-%s-%d", m.Protocol, m.PublicPort))
			}
		}

		snapshots, _ := db.SnapshotsListByInstanceID(ctx, instance.ID)
		if len(snapshots) > 0 && client != nil {
			for _, snap := range snapshots {
				_, _ = lib.DeleteSnapshot(ctx, client, instance.IncusID, snap.IncusName)
			}
		}

		backups, _ := db.GetBackupsByInstanceID(ctx, instance.ID)
		if len(backups) > 0 && client != nil {
			for _, bak := range backups {
				_, _ = lib.DeleteIncusBackup(ctx, client, instance.IncusID, bak.IncusName)
			}
		}

		if !databaseOnly && client != nil {
			if err := hostsDeleteIncusInstanceBestEffort(ctx, client, instance.IncusID, instance.Status); err != nil {
				hostsRestoreBatchDeleteClaim(ctx, instance.ID, instance.Status)
				failResult(err.Error())
				log.Printf("[HostBatchDelete] 批量删除实例失败 (%s): %v", instance.Name, err)
				continue
			}
		}

		refundAmount := 0.0
		if instance.UserID != host.UserID {
			refundInfo, refundErr := HostsInstanceRefund(ctx, HostsRefundInput{
				ID:            instance.ID,
				BillingPrice:  instance.BillingPrice,
				BillingCycle:  instance.BillingCycle,
				ExpiresAt:     instance.ExpiresAt,
				PackagePlanID: instance.PackagePlanID,
			})
			if refundErr != nil {
				failResult(refundErr.Error())
				continue
			}
			if refundInfo.IsPaid && refundInfo.RefundAmount > 0 {
				refundAmount = refundInfo.RefundAmount
				refundDecimal := instancesDecimalFromJSNumber(refundAmount)
				txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					if err := db.AdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, instance.UserID); err != nil {
						return err
					}
					var oldBalance models.Decimal
					if err := tx.WithContext(ctx).Raw(`SELECT balance FROM users WHERE id = ?`, instance.UserID).
						Scan(&oldBalance).Error; err != nil {
						return err
					}
					if err := tx.Exec(`UPDATE users SET balance = balance + ?, updated_at = ? WHERE id = ?`,
						refundDecimal, time.Now().UTC(), instance.UserID).Error; err != nil {
						return err
					}
					var newBalance models.Decimal
					if err := tx.WithContext(ctx).Raw(`SELECT balance FROM users WHERE id = ?`, instance.UserID).
						Scan(&newBalance).Error; err != nil {
						return err
					}
					_, err := instancesTxCreateBalanceLog(tx, instance.UserID, models.BalanceLogTypeRefund,
						refundDecimal, oldBalance, newBalance, instance.ID, "实例被删除退款："+instance.Name)
					return err
				}, &sql.TxOptions{Isolation: sql.LevelSerializable})
				if txErr != nil {
					failResult(txErr.Error())
					continue
				}
				totalRefundAmount += refundAmount
			}
		}

		for _, site := range proxySites {
			_, _ = db.ProxySitesDelete(ctx, site.ID)
		}
		for _, m := range portMappings {
			_ = db.PortMappingsDelete(ctx, m.ID)
		}
		for _, snap := range snapshots {
			_ = db.SnapshotsDelete(ctx, snap.ID)
		}
		for _, bak := range backups {
			_ = db.DeleteBackup(ctx, bak.ID)
		}
		cleanupQueries := []string{
			`DELETE FROM snapshot_policies WHERE instance_id = ?`,
			`DELETE FROM backup_policies WHERE instance_id = ?`,
			`DELETE FROM traffic_snapshots WHERE instance_id = ?`,
			`DELETE FROM daily_traffic WHERE instance_id = ?`,
			`DELETE FROM ip_addresses WHERE instance_id = ?`,
			`DELETE FROM ipv6_subnets WHERE instance_id = ?`,
			`DELETE FROM restore_tasks WHERE instance_id = ? AND status IN ('COMPLETED','FAILED')`,
			`DELETE FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('COMPLETED','FAILED')`,
		}
		for _, q := range cleanupQueries {
			_ = db.DB.WithContext(ctx).Exec(q, instance.ID).Error
		}
		_ = db.DB.WithContext(ctx).Exec(
			`UPDATE instance_transfers SET status = 'cancelled', cancelled_at = ? WHERE instance_id = ? AND status = 'pending'`,
			time.Now().UTC(), instance.ID,
		).Error

		if err := db.UpdateInstanceStatus(ctx, instance.ID, models.InstanceStatusDeleted, nil); err != nil {
			failResult(err.Error())
			continue
		}

		_ = db.RollbackResources(ctx, db.RollbackResourcesOptions{
			HostID:    instance.HostID,
			CPU:       instance.CPU,
			Memory:    instance.Memory,
			Disk:      instance.Disk,
			PortCount: int32(len(portMappings)),
		})

		deleteReason := strings.TrimSpace(reason)
		instCopy := instance
		notifyFn := func(notifyCtx context.Context) {
			if deleteReason != "" {
				lib.SendNotification(notifyCtx, int64(instCopy.UserID), lib.EventInstanceDeletedByHostOwner, lib.EventData{
					"instanceName": instCopy.Name,
					"hostName":     host.Name,
					"deleteReason": deleteReason,
				})
			} else {
				lib.SendNotification(notifyCtx, int64(instCopy.UserID), lib.EventInstanceDeleted, lib.EventData{
					"instanceName": instCopy.Name,
					"hostName":     host.Name,
				})
			}
		}
		if databaseOnly {
			deferredNotifications = append(deferredNotifications, func() {
				notifyFn(context.Background())
			})
		} else {
			notifyFn(ctx)
		}

		var refundPtr *float64
		if refundAmount > 0 {
			r := refundAmount
			refundPtr = &r
		}
		results = append(results, HostsBatchDeleteResult{
			ID:           instance.ID,
			Name:         instance.Name,
			Success:      true,
			RefundAmount: refundPtr,
		})

		if instance.PackageID != nil {
			agg := releasedByPackage[*instance.PackageID]
			agg.cpu += instance.CPU
			agg.memory += instance.Memory
			agg.count++
			releasedByPackage[*instance.PackageID] = agg
		}

		uid := int64(operatorID)
		refundSuffix := ""
		if refundAmount > 0 {
			refundSuffix = fmt.Sprintf(" (refund: ¥%.2f)", refundAmount)
		}
		hostsCreateLog(ctx, &uid, "instance.batch_delete",
			fmt.Sprintf(`Batch deleted instance "%s" on host "%s"%s`, instance.Name, host.Name, refundSuffix),
			"success", nil)
	}

	for pkgID, rel := range releasedByPackage {
		pID := pkgID
		c, m, cnt := rel.cpu, rel.memory, rel.count
		go lib.SendReleaseNotification(context.Background(), lib.ReleaseNotificationParams{
			PackageID: pID,
			CPU:       c,
			Memory:    m,
			Source:    fmt.Sprintf("节点所有者批量删除 %d 个实例", cnt),
		})
	}

	if used, err := db.HostsCalculateResourcesFromInstances(ctx, host.ID); err == nil {
		_ = db.HostsUpdateResources(ctx, host.ID, db.HostsResourceUpdate{
			CPUUsed:    &used.CPUUsed,
			MemoryUsed: &used.MemoryUsed,
			DiskUsed:   &used.DiskUsed,
		}, nil)
		var actualPortsUsed int64
		if db.DB.WithContext(ctx).Raw(
			`SELECT COUNT(*) FROM port_mappings pm JOIN instances i ON i.id = pm.instance_id WHERE i.host_id = ? AND i.status <> 'deleted'`,
			host.ID,
		).Scan(&actualPortsUsed).Error == nil {
			_ = db.DB.WithContext(ctx).Exec(`UPDATE hosts SET nat_ports_used_count = ?, updated_at = ? WHERE id = ?`,
				actualPortsUsed, time.Now().UTC(), host.ID).Error
		}
	}

	if len(deferredNotifications) > 0 {
		go func() {
			for _, fn := range deferredNotifications {
				fn()
			}
		}()
	}

	return results, math.Round(totalRefundAmount*100) / 100, nil
}

// ==================== Migration Execution ====================

const hostsNanoidAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func hostsNanoid8() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	out := make([]byte, 8)
	for i, v := range b {
		out[i] = hostsNanoidAlphabet[int(v)%len(hostsNanoidAlphabet)]
	}
	return string(out)
}

func hostsMigrateOne(
	ctx context.Context,
	sourceClient, targetClient *lib.IncusClient,
	plan HostsMigratePlan,
	targetPlan *models.PackagePlan,
	targetPkg *models.Package,
) (int32, error) {
	instance := plan.Instance
	sourceHost := plan.SourceHost
	targetHost := plan.TargetHost

	if err := db.GpusAssertMigration(ctx, instance.ID); err != nil {
		return 0, err
	}
	pending, err := db.HasPendingTransfer(ctx, instance.ID)
	if err != nil {
		return 0, err
	}
	if pending {
		return 0, errors.New("实例有待处理的转移请求")
	}
	restoreActive, uploadActive, err := hostsHasActiveRestoreOrUpload(ctx, instance.ID)
	if err != nil {
		return 0, err
	}
	if restoreActive {
		return 0, errors.New("实例有正在进行的恢复任务")
	}
	if uploadActive {
		return 0, errors.New("实例有正在进行的上传任务")
	}

	sshKeys, err := db.GetSSHKeysByUserID(ctx, instance.UserID)
	if err != nil {
		return 0, err
	}
	if len(sshKeys) == 0 {
		return 0, errors.New("用户没有 SSH 密钥")
	}
	sshKey := sshKeys[0].PublicKey

	rootPassword := ""
	if instance.RootPassword != nil && *instance.RootPassword != "" {
		decrypted := lib.DecryptSensitiveData(*instance.RootPassword)
		if decrypted != "" {
			rootPassword = decrypted
		}
	}
	if rootPassword == "" {
		rootPassword = lib.GenerateRandomPassword(16)
	}

	if instance.PackageID == nil {
		return 0, errors.New("实例套餐不存在")
	}
	var srcPkg models.Package
	if res := db.DB.WithContext(ctx).Where("id = ?", *instance.PackageID).Limit(1).Find(&srcPkg); res.Error != nil {
		return 0, res.Error
	} else if res.RowsAffected == 0 {
		return 0, errors.New("实例套餐不存在")
	}

	pkgType := string(srcPkg.InstanceType)
	if pkgType == "" {
		pkgType = "container"
	}
	effectiveType := "container"
	cfgType := "container"
	if pkgType == "vm" {
		effectiveType = "virtual-machine"
		cfgType = "vm"
	}

	if ok, err := db.IsImageCompatibleWithInstanceType(ctx, plan.TargetImage, pkgType); err != nil {
		return 0, err
	} else if !ok {
		return 0, errors.New("目标系统与实例类型不兼容")
	}
	if ok, err := db.IsImageCompatibleWithMemory(ctx, plan.TargetImage, int(instance.Memory)); err != nil {
		return 0, err
	} else if !ok {
		return 0, errors.New("目标系统与实例内存不兼容")
	}
	avail, err := db.GetSystemImageAvailabilityForHost(ctx, plan.TargetImage, targetHost.ID, db.SystemImageAvailabilityOptions{
		InstanceType: &pkgType,
		Memory:       hostsIntPtr(int(instance.Memory)),
	})
	if err != nil {
		return 0, err
	}
	if !avail.OK {
		return 0, fmt.Errorf("目标节点不可用该系统: %s", avail.Reason)
	}

	baseCfg, err := lib.GenerateIncusConfig(lib.IncusConfigParams{
		InstanceName: instance.Name,
		ImageAlias:   plan.TargetImage,
		RootPassword: rootPassword,
		SSHKey:       &sshKey,
		NetworkMode:  string(instance.NetworkMode),
		Type:         effectiveType,
	})
	if err != nil {
		return 0, err
	}

	newIncusID := fmt.Sprintf("u%d-%s", instance.UserID, hostsNanoid8())
	staticIPv4, err := db.IpAddressesAllocateAndReserveNatIpv4(ctx, targetHost.ID, &instance.ID)
	if err != nil {
		return 0, err
	}
	if staticIPv4 == nil || *staticIPv4 == "" {
		return 0, errors.New("无法分配 IPv4 地址")
	}

	var staticIPv6 *string
	if targetHost.IPv6Subnet != nil && *targetHost.IPv6Subnet != "" {
		for attempt := 0; attempt < 50; attempt++ {
			cand, genErr := lib.GenerateRandomIPv6(*targetHost.IPv6Subnet)
			if genErr != nil {
				break
			}
			exists, exErr := db.IpAddressesExists(ctx, cand)
			if exErr == nil && !exists {
				c := cand
				staticIPv6 = &c
				break
			}
		}
	}

	finalPayload := baseCfg.ConfigPayload
	netParams := &lib.IncusConfigStaticNetwork{
		IPAddress: *staticIPv4 + "/22",
		Gateway:   "10.10.0.1",
		DNS:       []string{"8.8.8.8", "1.1.1.1"},
	}
	if staticIPv6 != nil {
		if effectiveType == "virtual-machine" {
			netParams.IPv6Address = *staticIPv6
			if targetHost.IPv6Gateway != nil {
				netParams.IPv6Gateway = *targetHost.IPv6Gateway
			}
		} else {
			netParams.IPv6Address = *staticIPv6 + "/128"
			netParams.IPv6Gateway = "fe80::1"
		}
	}
	netCfg, err := lib.GenerateIncusConfig(lib.IncusConfigParams{
		InstanceName: instance.Name,
		ImageAlias:   plan.TargetImage,
		RootPassword: rootPassword,
		SSHKey:       &sshKey,
		NetworkMode:  string(instance.NetworkMode),
		Type:         effectiveType,
		Network:      netParams,
	})
	if err != nil {
		return 0, err
	}
	finalPayload = netCfg.ConfigPayload

	pkgIDForPool := instance.PackageID
	if targetPkg != nil {
		pkgIDForPool = &targetPkg.ID
	}
	selectedPool, err := db.StoragePoolsResolveNewInstancePool(ctx, targetHost.ID, pkgIDForPool)
	if err != nil {
		return 0, err
	}
	if selectedPool == nil || *selectedPool == "" {
		return 0, errors.New("STORAGE_POOL_UNAVAILABLE: 目标节点未配置可用于实例系统盘的存储池")
	}

	ioMode := srcPkg.IOLimitMode
	if ioMode == "" {
		ioMode = "throughput"
	}
	var limitsRead, limitsWrite *string
	var limitsReadIops, limitsWriteIops *int32
	if ioMode == "throughput" {
		limitsRead = firstNonEmptyStrPtr(instance.LimitsRead, strPtrNonEmpty(srcPkg.LimitsRead))
		limitsWrite = firstNonEmptyStrPtr(instance.LimitsWrite, strPtrNonEmpty(srcPkg.LimitsWrite))
	} else if ioMode == "iops" {
		limitsReadIops = firstPositiveI32Ptr(instance.LimitsReadIops, i32PtrNonZero(srcPkg.LimitsReadIops))
		limitsWriteIops = firstPositiveI32Ptr(instance.LimitsWriteIops, i32PtrNonZero(srcPkg.LimitsWriteIops))
	}
	limitsIngress := firstNonEmptyStrPtr(instance.LimitsIngress, strPtrNonEmpty(srcPkg.LimitsIngress))
	limitsEgress := firstNonEmptyStrPtr(instance.LimitsEgress, strPtrNonEmpty(srcPkg.LimitsEgress))
	limitsProcesses := firstPositiveI32Ptr(instance.LimitsProcesses, i32PtrNonZero(srcPkg.LimitsProcesses))
	limitsCPUPrio := firstPositiveI32Ptr(instance.LimitsCPUPriority, i32PtrNonZero(srcPkg.LimitsCPUPriority))
	bootAutostart := firstBoolPtr(instance.BootAutostart, &srcPkg.BootAutostart)
	bootPrio := firstPositiveI32Ptr(instance.BootAutostartPriority, &srcPkg.BootAutostartPriority)
	bootDelay := firstPositiveI32Ptr(instance.BootAutostartDelay, &srcPkg.BootAutostartDelay)
	bootTimeout := firstPositiveI32Ptr(instance.BootHostShutdownTimeout, &srcPkg.BootHostShutdownTimeout)

	var ipv6Cfg *lib.NetworkPayloadIPv6Config
	if staticIPv6 != nil {
		ipv6Cfg = &lib.NetworkPayloadIPv6Config{PrimaryIP: *staticIPv6}
	}
	hostIf := "eth0"
	if targetHost.IPv6ParentInterface != nil && *targetHost.IPv6ParentInterface != "" {
		hostIf = *targetHost.IPv6ParentInterface
	}
	swapEnabled := instance.SwapEnabled

	incusConfig, err := lib.IncusInstanceBuildConfig(lib.IncusInstanceConfigOptions{
		Name:                    newIncusID,
		Image:                   plan.TargetImage,
		CPU:                     int(instance.CPU),
		Memory:                  int(instance.Memory),
		Disk:                    int(instance.Disk),
		SwapEnabled:             &swapEnabled,
		SwapSize:                instance.SwapSize,
		CloudInitConfig:         &finalPayload,
		NetworkMode:             string(instance.NetworkMode),
		Nested:                  srcPkg.Nested,
		Privileged:              srcPkg.Privileged,
		InstanceType:            cfgType,
		StoragePool:             selectedPool,
		IPv4Address:             staticIPv4,
		IPv6Config:              ipv6Cfg,
		HostInterface:           &hostIf,
		IPv6Address:             staticIPv6,
		IPv6Gateway:             targetHost.IPv6Gateway,
		LimitsRead:              limitsRead,
		LimitsWrite:             limitsWrite,
		LimitsReadIops:          limitsReadIops,
		LimitsWriteIops:         limitsWriteIops,
		LimitsIngress:           limitsIngress,
		LimitsEgress:            limitsEgress,
		LimitsProcesses:         limitsProcesses,
		LimitsCPUPriority:       limitsCPUPrio,
		BootAutostart:           bootAutostart,
		BootAutostartPriority:   bootPrio,
		BootAutostartDelay:      bootDelay,
		BootHostShutdownTimeout: bootTimeout,
	})
	if err != nil {
		return 0, err
	}

	targetReserved := false
	targetCreated := false
	var newDBInstanceID int32
	sourceIncusDeleted := false

	rollbackOnFailure := func() {
		if sourceIncusDeleted {
			return
		}
		if newDBInstanceID != 0 {
			_ = db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				_ = tx.Exec(`UPDATE instance_billing_records SET instance_id = ? WHERE instance_id = ?`, instance.ID, newDBInstanceID).Error
				_ = tx.Exec(`DELETE FROM aff_bindings WHERE instance_id = ?`, newDBInstanceID).Error
				_ = tx.Exec(`DELETE FROM instances WHERE id = ?`, newDBInstanceID).Error
				_ = tx.Exec(`UPDATE instances SET status = ?, updated_at = ? WHERE id = ? AND status = 'deleted'`,
					string(instance.Status), time.Now().UTC(), instance.ID).Error
				return nil
			})
		}
		if targetCreated {
			_ = hostsDeleteIncusInstanceBestEffort(ctx, targetClient, newIncusID, models.InstanceStatusRunning)
		}
		if targetReserved {
			_ = db.RollbackResources(ctx, db.RollbackResourcesOptions{
				HostID: targetHost.ID,
				CPU:    instance.CPU,
				Memory: instance.Memory,
				Disk:   instance.Disk,
			})
		}
	}

	if err := db.ReserveResources(ctx, db.ReserveResourcesOptions{
		HostID:          targetHost.ID,
		CPU:             instance.CPU,
		Memory:          instance.Memory,
		Disk:            instance.Disk,
		StoragePoolName: selectedPool,
	}); err != nil {
		return 0, err
	}
	targetReserved = true

	if _, err := lib.IncusInstanceCreate(ctx, targetClient, incusConfig, &instance.ID); err != nil {
		rollbackOnFailure()
		return 0, err
	}
	targetCreated = true

	if _, err := lib.IncusInstanceStart(ctx, targetClient, newIncusID); err != nil {
		rollbackOnFailure()
		return 0, err
	}

	affBinding, _ := db.AffGetInstanceBinding(ctx, instance.ID, nil)
	isPaidInstance := instance.PackagePlanID != nil
	finalPackageID := instance.PackageID
	finalPlanID := instance.PackagePlanID
	finalBillingPrice := instance.BillingPrice
	finalBillingCycle := instance.BillingCycle
	if isPaidInstance && targetPkg != nil {
		idCopy := targetPkg.ID
		finalPackageID = &idCopy
	}
	if isPaidInstance && targetPlan != nil {
		pID := targetPlan.ID
		finalPlanID = &pID
		price := instancesDecimalFromJSNumber(targetPlan.Price.InexactFloat64() / 100.0)
		finalBillingPrice = &price
		cCopy := targetPlan.BillingCycle
		finalBillingCycle = &cCopy
	}

	encryptedPw, encErr := lib.EncryptSensitiveData(rootPassword)
	if encErr != nil {
		rollbackOnFailure()
		return 0, encErr
	}

	var movedBillingRows int64
	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(74201)`).Error; err != nil {
			return err
		}
		binding, err := instancesTxBindPackageEgress(ctx, tx, finalPackageID, targetHost.ID, &instance.ID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		var createdID int32
		if err := tx.Raw(`INSERT INTO instances (
			egress_mode, egress_profile_id, incus_id, name, user_id, host_id, storage_pool_name,
			package_id, package_plan_id, image, cpu, memory, disk, network_mode, snapshotted_specs,
			ssh_port, root_password, status, ipv4, ipv6, monthly_traffic_limit, port_limit,
			snapshot_limit, backup_limit, site_limit, expires_at, billing_price, billing_cycle, auto_renew,
			limits_read, limits_write, limits_read_iops, limits_write_iops, limits_ingress, limits_egress,
			limits_processes, limits_cpu_priority, boot_autostart, boot_autostart_priority,
			boot_autostart_delay, boot_host_shutdown_timeout, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
			binding.EgressMode, binding.EgressProfileID, newIncusID, instance.Name, instance.UserID, targetHost.ID, selectedPool,
			finalPackageID, finalPlanID, plan.TargetImage, instance.CPU, instance.Memory, instance.Disk, string(instance.NetworkMode), instance.SnapshottedSpecs,
			baseCfg.MetaData.SSHPort, encryptedPw, staticIPv4, staticIPv6, instance.MonthlyTrafficLimit, instance.PortLimit,
			instance.SnapshotLimit, instance.BackupLimit, instance.SiteLimit, instance.ExpiresAt, finalBillingPrice, finalBillingCycle, instance.AutoRenew,
			instance.LimitsRead, instance.LimitsWrite, instance.LimitsReadIops, instance.LimitsWriteIops, instance.LimitsIngress, instance.LimitsEgress,
			instance.LimitsProcesses, instance.LimitsCPUPriority, instance.BootAutostart, instance.BootAutostartPriority,
			instance.BootAutostartDelay, instance.BootHostShutdownTimeout, now, now,
		).Scan(&createdID).Error; err != nil {
			return err
		}
		newDBInstanceID = createdID

		moved := tx.Exec(`UPDATE instance_billing_records SET instance_id = ? WHERE instance_id = ?`, createdID, instance.ID)
		if moved.Error != nil {
			return moved.Error
		}
		movedBillingRows = moved.RowsAffected

		if affBinding != nil {
			if err := tx.Exec(`INSERT INTO aff_bindings (instance_id, aff_code_id) VALUES (?, ?)`, createdID, affBinding.AffCode.ID).Error; err != nil {
				return err
			}
		}

		srcUpd := tx.Exec(`UPDATE instances SET status = 'deleted', updated_at = ? WHERE id = ? AND status = ?`,
			now, instance.ID, string(instance.Status))
		if srcUpd.Error != nil {
			return srcUpd.Error
		}
		if srcUpd.RowsAffected != 1 {
			return errors.New("源实例状态已变化，无法完成迁移")
		}
		if err := tx.Exec(`UPDATE instance_egress_attachments SET instance_id = ? WHERE host_id = ? AND incus_name = ?`,
			createdID, targetHost.ID, newIncusID).Error; err != nil {
			return err
		}
		return nil
	})
	if txErr != nil {
		rollbackOnFailure()
		return 0, txErr
	}

	if isPaidInstance && movedBillingRows == 0 {
		log.Printf("[Migrate] 付费实例迁移时未迁移到任何计费记录 (oldInstanceId=%d, newInstanceId=%d, name=%s)",
			instance.ID, newDBInstanceID, instance.Name)
	}

	_ = hostsDeleteIncusInstanceBestEffort(ctx, sourceClient, instance.IncusID, instance.Status)
	sourceIncusDeleted = true

	_ = db.RollbackResources(ctx, db.RollbackResourcesOptions{
		HostID: sourceHost.ID,
		CPU:    instance.CPU,
		Memory: instance.Memory,
		Disk:   instance.Disk,
	})

	ipv6Line := ""
	if staticIPv6 != nil && *staticIPv6 != "" {
		ipv6Line = fmt.Sprintf("- IPv6: %s\n", *staticIPv6)
	}
	_, _ = db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
		UserID:    instance.UserID,
		EventType: "instance_migrated",
		Title:     "实例已迁移",
		Content: fmt.Sprintf("您的实例 %s 已被管理员迁移到新节点。\n\n新的连接信息：\n- IPv4: %s\n%s- 密码保持不变\n- SSH 密钥保持不变",
			instance.Name, *staticIPv4, ipv6Line),
		Data: map[string]any{
			"oldHostId":    sourceHost.ID,
			"newHostId":    targetHost.ID,
			"oldHostName":  sourceHost.Name,
			"newHostName":  targetHost.Name,
			"instanceId":   newDBInstanceID,
			"instanceName": instance.Name,
		},
	})

	opID := int64(plan.OperatorID)
	hostsCreateLog(ctx, &opID, "instance.migrate",
		fmt.Sprintf(`Migrated instance "%s" from host "%s" to "%s"`, instance.Name, sourceHost.Name, targetHost.Name),
		"success", nil)

	return newDBInstanceID, nil
}

// ==================== Create-For-User Execution ====================

func hostsExecuteCreateForUser(
	c *gin.Context,
	operator *lib.AccessTokenClaims,
	host *db.HostsRecord,
	target *hostsUserLookup,
	sshKey string,
	pkgRow *hostsPackageRow,
	plan *models.PackagePlan,
	name, image, networkMode string,
	requestedCPU, requestedMemory, requestedDisk int64,
	giftDays int64,
) {
	ctx := c.Request.Context()
	isAdmin := operator.Role == "admin"

	var pkg models.Package
	if res := db.DB.WithContext(ctx).Where("id = ?", pkgRow.ID).Limit(1).Find(&pkg); res.Error != nil || res.RowsAffected == 0 {
		hostsRespondInternal(c)
		return
	}

	var nodeSelectors []string
	if len(pkg.NodeSelectors) > 0 {
		_ = json.Unmarshal(pkg.NodeSelectors, &nodeSelectors)
	}
	pkgInstanceType := pkgRow.InstanceType
	if pkgInstanceType == "" {
		pkgInstanceType = "container"
	}

	preCheckHost, err := db.HostsSelectAvailable(ctx, db.HostsSelectOptions{
		PackageHostIDs: pkgRow.HostIDs,
		NodeSelectors:  nodeSelectors,
		CPU:            requestedCPU,
		Memory:         requestedMemory,
		Disk:           requestedDisk,
		HostID:         &host.ID,
		OwnerID:        &pkg.UserID,
		PackageID:      &pkg.ID,
		NetworkMode:    &networkMode,
	})
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if preCheckHost == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前节点资源不足或不可用"})
		return
	}

	hostInstanceType := preCheckHost.InstanceType
	if hostInstanceType == "" {
		hostInstanceType = "container"
	}
	if pkgInstanceType == "vm" && hostInstanceType == "container" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "套餐要求虚拟机，但当前节点只支持容器"})
		return
	}
	if pkgInstanceType == "container" && hostInstanceType == "vm" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "套餐要求容器，但当前节点只支持虚拟机"})
		return
	}

	avail, err := db.GetSystemImageAvailabilityForHost(ctx, image, preCheckHost.ID, db.SystemImageAvailabilityOptions{
		InstanceType: &pkgInstanceType,
		Memory:       hostsIntPtr(int(requestedMemory)),
	})
	if err != nil {
		hostsRespondInternal(c)
		return
	}
	if !avail.OK {
		switch avail.Reason {
		case "host_not_found":
			c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		case "image_not_found":
			c.JSON(http.StatusBadRequest, gin.H{"error": "镜像不存在"})
		case "memory_incompatible":
			c.JSON(http.StatusBadRequest, gin.H{"error": "128MB 内存只能使用 Alpine/Debian 镜像"})
		case "instance_type_mismatch":
			c.JSON(http.StatusBadRequest, gin.H{"error": "镜像类型与套餐类型不兼容"})
		case "host_instance_type_mismatch":
			c.JSON(http.StatusBadRequest, gin.H{"error": "套餐类型与宿主机不兼容"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "当前节点不可用该镜像"})
		}
		return
	}

	autoPassword := hostsNanoid8() + hostsNanoid8()
	effectiveType := "container"
	if pkgInstanceType == "vm" {
		effectiveType = "virtual-machine"
	}
	baseCfg, err := lib.GenerateIncusConfig(lib.IncusConfigParams{
		InstanceName: name,
		ImageAlias:   image,
		RootPassword: autoPassword,
		SSHKey:       &sshKey,
		NetworkMode:  networkMode,
		Type:         effectiveType,
	})
	if err != nil {
		hostsRespondInternal(c)
		return
	}

	incusID := fmt.Sprintf("u%d-%s", target.ID, hostsNanoid8())
	var paidGiftDays *int
	var paidGiftExpiresAt *time.Time
	var billingPrice *models.Decimal
	var billingCycle *int32
	var planTrafficSpeed *string
	if plan != nil {
		d := int(giftDays)
		paidGiftDays = &d
		exp := time.Now().UTC().Add(time.Duration(d) * 24 * time.Hour)
		paidGiftExpiresAt = &exp
		bp := instancesDecimalFromJSNumber(plan.Price.InexactFloat64() / 100.0)
		billingPrice = &bp
		bc := plan.BillingCycle
		billingCycle = &bc
		if plan.TrafficLimitSpeed != "" {
			norm := services.TrafficBandwidthNormalizePlanTrafficLimitSpeed(&plan.TrafficLimitSpeed)
			planTrafficSpeed = norm.Value
		}
	}

	portCount := int32(0)
	if lib.NetworkModeAllowsPortMapping(networkMode) {
		if plan != nil {
			portCount = plan.PortLimit
		} else {
			portCount = pkg.PortLimit
		}
	}

	var instanceID int32
	var lockedHost *db.HostsSelectionRecord
	encryptedPw, err := lib.EncryptSensitiveData(baseCfg.MetaData.RootPassword)
	if err != nil {
		hostsRespondInternal(c)
		return
	}

	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(74201)`).Error; err != nil {
			return err
		}
		reserved, err := db.HostsSelectAndReserveWithLock(ctx, tx, db.HostsReserveOptions{
			PackagePlanID:  hostsPlanIDPtr(plan),
			PackageHostIDs: pkgRow.HostIDs,
			NodeSelectors:  nodeSelectors,
			CPU:            requestedCPU,
			Memory:         requestedMemory,
			Disk:           requestedDisk,
			HostID:         &host.ID,
			OwnerID:        &pkg.UserID,
			PackageID:      &pkg.ID,
			NetworkMode:    &networkMode,
			PortCount:      portCount,
		})
		if err != nil {
			return err
		}
		if reserved == nil {
			return errors.New("HOST_RESOURCES_INSUFFICIENT")
		}
		lockedHost = reserved

		baseTraffic := pkg.MonthlyTrafficLimit
		if plan != nil {
			lim := plan.TrafficLimit
			baseTraffic = &lim
		}
		effectiveTraffic, err := lib.ResolveInstanceTrafficLimitForHost(ctx, tx, lib.TrafficMultiplierResolveParams{
			PackageID:        &pkg.ID,
			HostID:           reserved.ID,
			BaseTrafficLimit: baseTraffic,
		})
		if err != nil {
			return err
		}

		binding, err := instancesTxBindPackageEgress(ctx, tx, &pkg.ID, reserved.ID, nil)
		if err != nil {
			return err
		}
		gpuRes, err := db.GpusReserveInstanceGpu(ctx, tx, db.GpusReserveInput{
			HostID:    reserved.ID,
			PackageID: &pkg.ID,
			PlanID:    hostsPlanIDPtr(plan),
		})
		if err != nil {
			return err
		}

		var portLimit, snapshotLimit, backupLimit, siteLimit, swapSize *int32
		var limitsIngress, limitsEgress *string
		if plan != nil {
			portLimit = &plan.PortLimit
			snapshotLimit = &plan.SnapshotLimit
			backupLimit = &plan.BackupLimit
			siteLimit = &plan.SiteLimit
			if pkgInstanceType == "container" {
				s := plan.SwapSize
				swapSize = &s
			}
			limitsIngress = planTrafficSpeed
			limitsEgress = planTrafficSpeed
		} else {
			if pkg.PortLimit != 0 {
				v := pkg.PortLimit
				portLimit = &v
			}
			if pkg.SnapshotLimit != 0 {
				v := pkg.SnapshotLimit
				snapshotLimit = &v
			}
			if pkg.BackupLimit != 0 {
				v := pkg.BackupLimit
				backupLimit = &v
			}
			if pkg.SiteLimit != 0 {
				v := pkg.SiteLimit
				siteLimit = &v
			}
		}

		var planIDVal any
		var planNameVal any
		if plan != nil {
			planIDVal = plan.ID
			planNameVal = plan.Name
		}
		giftType := "host_owner"
		if isAdmin {
			giftType = "admin"
		}
		specPortLimit := pkg.PortLimit
		if specPortLimit == 0 {
			specPortLimit = 20
		}
		if plan != nil && plan.PortLimit != 0 {
			specPortLimit = plan.PortLimit
		}
		specsBytes, _ := json.Marshal(map[string]any{
			"packageId":         pkg.ID,
			"packageName":       pkg.Name,
			"planId":            planIDVal,
			"planName":          planNameVal,
			"cpuMax":            pkg.CPUMax,
			"memoryMax":         pkg.MemoryMax,
			"diskMax":           pkg.DiskMax,
			"networkMode":       networkMode,
			"portLimit":         specPortLimit,
			"nested":            pkg.Nested,
			"privileged":        pkg.Privileged,
			"nodeSelectors":     nodeSelectors,
			"createdAt":         time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			"hostOwnerGift":     !isAdmin,
			"adminGift":         isAdmin,
			"giftBy":            operator.ID,
			"giftByType":        giftType,
			"isPaidInstance":    plan != nil,
			"chargedFirstMonth": false,
			"giftDays":          paidGiftDays,
		})

		now := time.Now().UTC()
		if err := tx.Raw(`INSERT INTO instances (
			egress_mode, egress_profile_id, incus_id, name, user_id, host_id, storage_pool_name,
			package_id, package_plan_id, gpu_device_id, image, cpu, memory, disk, network_mode,
			snapshotted_specs, ssh_port, root_password, status, monthly_traffic_limit, port_limit,
			snapshot_limit, backup_limit, site_limit, swap_enabled, swap_size, limits_ingress,
			limits_egress, expires_at, billing_price, billing_cycle, auto_renew, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'creating', ?, ?, ?, ?, ?, false, ?, ?, ?, ?, ?, ?, false, ?, ?)
		RETURNING id`,
			binding.EgressMode, binding.EgressProfileID, incusID, name, target.ID, reserved.ID, reserved.StoragePoolName,
			pkg.ID, hostsPlanIDPtr(plan), gpuRes.GpuDeviceID, image, requestedCPU, requestedMemory, requestedDisk, networkMode,
			string(specsBytes), baseCfg.MetaData.SSHPort, encryptedPw, effectiveTraffic, portLimit,
			snapshotLimit, backupLimit, siteLimit, swapSize, limitsIngress,
			limitsEgress, paidGiftExpiresAt, billingPrice, billingCycle, now, now,
		).Scan(&instanceID).Error; err != nil {
			return err
		}
		for _, gpuID := range gpuRes.GpuDeviceIDs {
			if err := tx.Exec(`INSERT INTO instance_gpu_allocations (instance_id, gpu_device_id)
				VALUES (?, ?) ON CONFLICT DO NOTHING`, instanceID, gpuID).Error; err != nil {
				return err
			}
		}

		if plan != nil && paidGiftExpiresAt != nil && paidGiftDays != nil {
			roleLabel := "节点所有者"
			if isAdmin {
				roleLabel = "管理员"
			}
			if err := tx.Exec(`INSERT INTO instance_billing_records
				(instance_id, user_id, type, amount, months, period_start, period_end, balance_log_id, remark, created_at)
				VALUES (?, ?, 'newPurchase', 0, 0, ?, ?, NULL, ?, ?)`,
				instanceID, target.ID, now, *paidGiftExpiresAt,
				fmt.Sprintf("%s赠送付费实例免费时长 %d 天", roleLabel, *paidGiftDays), now,
			).Error; err != nil {
				return err
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})

	if txErr != nil {
		if txErr.Error() == "HOST_RESOURCES_INSUFFICIENT" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "当前节点资源不足或不可用"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建实例失败: " + txErr.Error()})
		return
	}

	var staticIPv4, staticIPv6 *string
	var publicAssign *db.PublicIpv4Assignment
	if lib.NetworkModeNeedsNatIpv4(networkMode) {
		if v4, ipErr := db.IpAddressesAllocateAndReserveNatIpv4(ctx, lockedHost.ID, &instanceID); ipErr == nil {
			staticIPv4 = v4
		}
	} else if lib.NetworkModeNeedsPublicIpv4(networkMode) {
		pgTx, err := db.Pool().Begin(ctx)
		if err == nil {
			assign, resErr := db.ReservePublicIpv4ForInstance(ctx, pgTx, db.PublicIpv4ReserveInput{
				HostID:     lockedHost.ID,
				InstanceID: instanceID,
			})
			if resErr == nil && assign != nil && pgTx.Commit(ctx) == nil {
				publicAssign = assign
				addr := assign.Address
				staticIPv4 = &addr
			} else {
				_ = pgTx.Rollback(ctx)
			}
		}
		if publicAssign == nil {
			_ = db.DB.WithContext(ctx).Exec(`UPDATE instances SET status = 'error', updated_at = ? WHERE id = ? AND status = 'creating'`,
				time.Now().UTC(), instanceID).Error
			_ = db.RollbackResources(ctx, db.RollbackResourcesOptions{
				HostID:    lockedHost.ID,
				CPU:       int32(requestedCPU),
				Memory:    int32(requestedMemory),
				Disk:      int32(requestedDisk),
				PortCount: portCount,
			})
			_, _ = db.CompensateFailedInstancePurchase(ctx, instanceID, target.ID, lockedHost.ID)
			_ = db.GpusReleaseFailed(ctx, instanceID)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("宿主机 %s 没有可用独立 IPv4 地址", lockedHost.Name)})
			return
		}
	}

	if lockedHost.IPv6Subnet != nil && *lockedHost.IPv6Subnet != "" && lib.NetworkModeNeedsRoutedIpv6(networkMode) {
		for attempt := 0; attempt < 50; attempt++ {
			cand, genErr := lib.GenerateRandomIPv6(*lockedHost.IPv6Subnet)
			if genErr != nil {
				break
			}
			exists, exErr := db.IpAddressesExists(ctx, cand)
			if exErr == nil && !exists {
				c := cand
				staticIPv6 = &c
				break
			}
		}
	}

	finalPayload := baseCfg.ConfigPayload
	if staticIPv4 != nil {
		ipv4CIDR := *staticIPv4 + "/22"
		gw := "10.10.0.1"
		dns := []string{"10.10.0.1"}
		if publicAssign != nil {
			ipv4CIDR = fmt.Sprintf("%s/%d", publicAssign.Address, publicAssign.PrefixLength)
			if publicAssign.Gateway != "" {
				gw = publicAssign.Gateway
			}
			if len(publicAssign.DNS) > 0 {
				dns = publicAssign.DNS
			}
		}
		netCfg := &lib.IncusConfigStaticNetwork{IPAddress: ipv4CIDR, Gateway: gw, DNS: dns}
		if staticIPv6 != nil {
			if pkgInstanceType == "vm" {
				netCfg.IPv6Address = *staticIPv6
				if lockedHost.IPv6Gateway != nil {
					netCfg.IPv6Gateway = *lockedHost.IPv6Gateway
				}
			} else {
				netCfg.IPv6Address = *staticIPv6 + "/128"
				netCfg.IPv6Gateway = "fe80::1"
			}
		}
		if updatedCfg, cfgErr := lib.GenerateIncusConfig(lib.IncusConfigParams{
			InstanceName: name,
			ImageAlias:   image,
			RootPassword: baseCfg.MetaData.RootPassword,
			SSHKey:       &sshKey,
			NetworkMode:  networkMode,
			Type:         effectiveType,
			Network:      netCfg,
		}); cfgErr == nil {
			finalPayload = updatedCfg.ConfigPayload
		}
	}

	if lockedHost.StoragePoolName == nil || *lockedHost.StoragePoolName == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "宿主机未配置可用于实例系统盘的存储池"})
		return
	}
	_ = db.DB.WithContext(ctx).Exec(`UPDATE instances SET storage_pool_name = ?, updated_at = ? WHERE id = ?`,
		*lockedHost.StoragePoolName, time.Now().UTC(), instanceID).Error

	ioMode := pkg.IOLimitMode
	if ioMode == "" {
		ioMode = "throughput"
	}
	var limitsRead, limitsWrite *string
	var limitsReadIops, limitsWriteIops *int32
	if ioMode == "throughput" {
		limitsRead = strPtrNonEmpty(pkg.LimitsRead)
		limitsWrite = strPtrNonEmpty(pkg.LimitsWrite)
	} else if ioMode == "iops" {
		limitsReadIops = i32PtrNonZero(pkg.LimitsReadIops)
		limitsWriteIops = i32PtrNonZero(pkg.LimitsWriteIops)
	}
	limIngress := strPtrNonEmpty(pkg.LimitsIngress)
	limEgress := strPtrNonEmpty(pkg.LimitsEgress)
	if planTrafficSpeed != nil {
		limIngress = planTrafficSpeed
		limEgress = planTrafficSpeed
	}
	hostIf := "eth0"
	if lockedHost.IPv6ParentInterface != nil && *lockedHost.IPv6ParentInterface != "" {
		hostIf = *lockedHost.IPv6ParentInterface
	}
	portLimitVal := pkg.PortLimit
	if portLimitVal == 0 {
		portLimitVal = 20
	}
	if plan != nil {
		portLimitVal = plan.PortLimit
	}
	bootAutostart := pkg.BootAutostart
	provOpts := map[string]any{
		"userId":                  target.ID,
		"name":                    incusID,
		"image":                   image,
		"cpu":                     int32(requestedCPU),
		"memory":                  int32(requestedMemory),
		"disk":                    int32(requestedDisk),
		"cloudInitConfig":         finalPayload,
		"networkMode":             networkMode,
		"nested":                  pkg.Nested,
		"privileged":              pkg.Privileged,
		"portLimit":               portLimitVal,
		"instanceType":            pkgInstanceType,
		"sshPort":                 baseCfg.MetaData.SSHPort,
		"storagePool":             lockedHost.StoragePoolName,
		"ipv4Address":             staticIPv4,
		"ipv6Address":             staticIPv6,
		"ipv6Gateway":             lockedHost.IPv6Gateway,
		"hostInterface":           hostIf,
		"limitsRead":              limitsRead,
		"limitsWrite":             limitsWrite,
		"limitsReadIops":          limitsReadIops,
		"limitsWriteIops":         limitsWriteIops,
		"limitsIngress":           limIngress,
		"limitsEgress":            limEgress,
		"limitsProcesses":         i32PtrNonZero(pkg.LimitsProcesses),
		"limitsCpuPriority":       i32PtrNonZero(pkg.LimitsCPUPriority),
		"bootAutostart":           &bootAutostart,
		"bootAutostartPriority":   i32PtrNonZero(pkg.BootAutostartPriority),
		"bootAutostartDelay":      i32PtrNonZero(pkg.BootAutostartDelay),
		"bootHostShutdownTimeout": i32PtrNonZero(pkg.BootHostShutdownTimeout),
	}

	bgCtx := context.WithoutCancel(ctx)
	go func() {
		if err := HostsManagedInstanceProvision(bgCtx, instanceID, lockedHost, provOpts); err != nil {
			log.Printf("[Host Create For User] instance %d provisioning failed: %v", instanceID, err)
		}
	}()

	actorPrefix := "Host owner"
	roleZh := "节点所有者"
	creatorType := "host_owner"
	if isAdmin {
		actorPrefix = "Admin"
		roleZh = "管理员"
		creatorType = "admin"
	}
	opID := int64(operator.ID)
	var logDetail string
	if plan != nil && paidGiftDays != nil {
		logDetail = fmt.Sprintf(`%s created paid instance "%s" for user %s on host "%s" (Package: %s, Plan: %s, Gift: %d days free)`,
			actorPrefix, name, target.Username, lockedHost.Name, pkg.Name, plan.Name, *paidGiftDays)
	} else {
		logDetail = fmt.Sprintf(`%s created free instance "%s" for user %s on host "%s" (Package: %s)`,
			actorPrefix, name, target.Username, lockedHost.Name, pkg.Name)
	}
	hostsCreateLog(ctx, &opID, "host.instance_create_for_user", logDetail, "success", &instanceID)

	memLabel := fmt.Sprintf("%d MB", requestedMemory)
	if requestedMemory >= 1024 {
		memLabel = fmt.Sprintf("%.1f GB", float64(requestedMemory)/1024.0)
	}
	diskLabel := fmt.Sprintf("%d MB", requestedDisk)
	if requestedDisk >= 1024 {
		diskLabel = fmt.Sprintf("%.0f GB", float64(requestedDisk)/1024.0)
	}
	resInfo := fmt.Sprintf("CPU %d%% | 内存 %s | 磁盘 %s", requestedCPU, memLabel, diskLabel)
	var notifyTitle, notifyContent, eventType string
	if plan != nil && paidGiftDays != nil && paidGiftExpiresAt != nil {
		eventType = "instance_paid_created"
		notifyTitle = fmt.Sprintf("收到%s赠送的付费实例", roleZh)
		notifyContent = fmt.Sprintf("%s %s 为您赠送了一台付费实例：%s\n套餐：%s\n方案：%s\n配置：%s\n节点：%s\n免费时长：%d 天\n到期时间：%s\n到期后将按方案价格正常计费",
			roleZh, operator.Username, name, pkg.Name, plan.Name, resInfo, lockedHost.Name, *paidGiftDays, paidGiftExpiresAt.Format("2006/01/02"))
	} else {
		eventType = "instance_gift"
		notifyTitle = fmt.Sprintf("收到%s赠送的实例", roleZh)
		notifyContent = fmt.Sprintf("%s %s 为您创建了一台实例：%s\n套餐：%s\n配置：%s\n节点：%s",
			roleZh, operator.Username, name, pkg.Name, resInfo, lockedHost.Name)
	}
	var planNameAny any
	var expiresAny any
	var giftDaysAny any
	if plan != nil {
		planNameAny = plan.Name
	}
	if paidGiftExpiresAt != nil {
		expiresAny = paidGiftExpiresAt.Format("2006-01-02T15:04:05.000Z")
	}
	if paidGiftDays != nil {
		giftDaysAny = *paidGiftDays
	}
	_, _ = db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
		UserID:    target.ID,
		EventType: eventType,
		Title:     notifyTitle,
		Content:   notifyContent,
		Data: map[string]any{
			"instanceId":   instanceID,
			"instanceName": name,
			"packageName":  pkg.Name,
			"planName":     planNameAny,
			"cpu":          requestedCPU,
			"memory":       requestedMemory,
			"disk":         requestedDisk,
			"hostName":     lockedHost.Name,
			"isPaid":       plan != nil,
			"charged":      false,
			"amount":       0,
			"expiresAt":    expiresAny,
			"giftDays":     giftDaysAny,
			"createdBy":    operator.Username,
			"creatorType":  creatorType,
		},
	})

	if target.Email != nil && *target.Email != "" {
		planNameStr := ""
		if plan != nil {
			planNameStr = plan.Name
		}
		gd := 0
		if paidGiftDays != nil {
			gd = *paidGiftDays
		}
		zeroAmt := 0.0
		_ = lib.SendAdminInstanceCreatedEmail(ctx, *target.Email, lib.AdminInstanceCreatedEmailData{
			Username:     target.Username,
			InstanceName: name,
			PackageName:  pkg.Name,
			PlanName:     planNameStr,
			HostName:     lockedHost.Name,
			CPU:          float64(requestedCPU),
			Memory:       float64(requestedMemory),
			Disk:         float64(requestedDisk),
			IsPaid:       plan != nil,
			Charged:      false,
			Amount:       &zeroAmt,
			ExpiresAt:    paidGiftExpiresAt,
			GiftDays:     gd,
			CreatorType:  creatorType,
			CreatorName:  operator.Username,
		})
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message": "实例创建中",
		"instance": gin.H{
			"id":      instanceID,
			"name":    name,
			"incusId": incusID,
			"host":    lockedHost.Name,
			"status":  "creating",
			"user": gin.H{
				"id":       target.ID,
				"username": target.Username,
			},
			"isPaid": plan != nil,
		},
	})
}

// ==================== Ops Optimize Port Mappings ====================

type hostsOptimizeDetail struct {
	Instance string `json:"instance"`
	Device   string `json:"device"`
	Result   string `json:"result"`
}

func hostsExecuteOptimizePortMappings(
	ctx context.Context,
	operatorID int32,
	host *db.HostsRecord,
	client *lib.IncusClient,
	bindableIPv4 string,
) (converted, skipped, failed int, details []hostsOptimizeDetail, err error) {
	if db.DB == nil {
		return 0, 0, 0, nil, errors.New("database not initialized")
	}
	type instRow struct {
		ID        int32  `gorm:"column:id"`
		IncusID   string `gorm:"column:incus_id"`
		Name      string `gorm:"column:name"`
		PackageID *int32 `gorm:"column:package_id"`
	}
	var instances []instRow
	if err := db.DB.WithContext(ctx).Raw(`
		SELECT id, incus_id, name, package_id
		FROM instances
		WHERE host_id = ? AND status <> 'deleted' AND network_mode IN ('nat', 'nat_ipv6', 'nat_ipv6_nat')`,
		host.ID,
	).Scan(&instances).Error; err != nil {
		return 0, 0, 0, nil, err
	}

	details = []hostsOptimizeDetail{}
	tcpPrefix := "tcp:" + bindableIPv4 + ":"
	udpPrefix := "udp:" + bindableIPv4 + ":"

	for _, inst := range instances {
		if inst.PackageID != nil {
			var pkgType string
			_ = db.DB.WithContext(ctx).Raw(`SELECT instance_type FROM packages WHERE id = ?`, *inst.PackageID).Scan(&pkgType).Error
			if pkgType == "vm" {
				continue
			}
		}

		current, getErr := lib.GetInstance(ctx, client, inst.IncusID)
		if getErr != nil {
			failed++
			details = append(details, hostsOptimizeDetail{
				Instance: inst.Name,
				Device:   "-",
				Result:   "读取实例配置失败: " + getErr.Error(),
			})
			continue
		}

		updatedDevices := map[string]map[string]any{}
		for devName, devCfg := range current.Devices {
			if devCfg["type"] != "proxy" {
				continue
			}
			if devCfg["nat"] == "true" {
				continue
			}
			listen, _ := devCfg["listen"].(string)
			if !strings.HasPrefix(listen, tcpPrefix) && !strings.HasPrefix(listen, udpPrefix) {
				skipped++
				details = append(details, hostsOptimizeDetail{
					Instance: inst.Name,
					Device:   devName,
					Result:   fmt.Sprintf("跳过（监听地址不是宿主机具体 IPv4: %s）", listen),
				})
				continue
			}
			copied := map[string]any{}
			for k, v := range devCfg {
				copied[k] = v
			}
			copied["nat"] = "true"
			updatedDevices[devName] = copied
		}
		if len(updatedDevices) == 0 {
			continue
		}

		mergedDevices := map[string]any{}
		for devName, devCfg := range current.Devices {
			mergedDevices[devName] = devCfg
		}
		for devName, devCfg := range updatedDevices {
			mergedDevices[devName] = devCfg
		}

		if _, updErr := lib.UpdateInstance(ctx, client, inst.IncusID, map[string]any{"devices": mergedDevices}); updErr != nil {
			failed += len(updatedDevices)
			names := make([]string, 0, len(updatedDevices))
			for k := range updatedDevices {
				names = append(names, k)
			}
			details = append(details, hostsOptimizeDetail{
				Instance: inst.Name,
				Device:   strings.Join(names, ","),
				Result:   "转换失败: " + updErr.Error(),
			})
			continue
		}
		converted += len(updatedDevices)
		for devName := range updatedDevices {
			details = append(details, hostsOptimizeDetail{
				Instance: inst.Name,
				Device:   devName,
				Result:   "已转为内核 NAT",
			})
		}
	}

	opID := int64(operatorID)
	resStr := "success"
	if failed > 0 {
		resStr = "failed"
	}
	hostsCreateLog(ctx, &opID, "ops.optimize_port_mappings",
		fmt.Sprintf(`Optimized port mappings on host "%s": converted=%d, skipped=%d, failed=%d`, host.Name, converted, skipped, failed),
		resStr, nil)
	return converted, skipped, failed, details, nil
}

// ==================== small helpers ====================

func hostsPlanIDPtr(plan *models.PackagePlan) *int32 {
	if plan == nil {
		return nil
	}
	id := plan.ID
	return &id
}

func strPtrNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func firstNonEmptyStrPtr(a, b *string) *string {
	if a != nil && *a != "" {
		return a
	}
	if b != nil && *b != "" {
		return b
	}
	return nil
}

func i32PtrNonZero(v int32) *int32 {
	if v == 0 {
		return nil
	}
	c := v
	return &c
}

func firstPositiveI32Ptr(a, b *int32) *int32 {
	if a != nil && *a > 0 {
		return a
	}
	if b != nil && *b > 0 {
		return b
	}
	return nil
}

func firstBoolPtr(a, b *bool) *bool {
	if a != nil {
		return a
	}
	return b
}
