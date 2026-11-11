// Notification + audit-log side effects for the Wave E workers.
//
// These are interfaces rather than direct calls so worker tests can assert the
// failure/retry/success paths without a database, a mail server or a websocket.
// The production implementation is deliberately thin: it only translates the
// worker's task state into the same notification events and `logs` rows the Node
// workers write. It never decides whether a task succeeded.
package workers

import (
	"context"
	"fmt"
	"log"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

// instanceTaskNotifier is the side-effect surface of instance_task_worker.go.
type instanceTaskNotifier interface {
	InstanceTaskFailed(ctx context.Context, notice instanceTaskFailureNotice, message string)
	InstanceStarted(ctx context.Context, userID int32, instanceName, hostName string, ipv4, ipv6 *string)
	InstanceStopped(ctx context.Context, userID int32, instanceName, hostName string)
	InstanceRestarted(ctx context.Context, userID int32, instanceName, hostName string, ipv4, ipv6 *string)
	InstanceRebuilt(ctx context.Context, userID int32, instanceName, hostName, image string)
}

// backupTaskNotifier is the side-effect surface of the restore and upload workers.
type backupTaskNotifier interface {
	BackupRestored(ctx context.Context, userID, instanceID int32, instanceName, backupName, hostName string)
	BackupRestoreFailed(ctx context.Context, userID, instanceID int32, instanceName, message string)
	BackupUploaded(ctx context.Context, userID, instanceID int32, instanceName, backupName, storageName, fileName, hostName string)
	BackupUploadFailed(ctx context.Context, userID, instanceID int32, message string)
	BackupUploadRetry(ctx context.Context, userID, instanceID, attempt, max int32, message string)
}

// pgNotifier is the production instanceTaskNotifier + backupTaskNotifier.
type pgNotifier struct{}

func newPGNotifier() *pgNotifier { return &pgNotifier{} }

func notifierUserID(userID int32) *int64 {
	value := int64(userID)
	return &value
}

func (n *pgNotifier) InstanceTaskFailed(ctx context.Context, notice instanceTaskFailureNotice, message string) {
	instanceName := fmt.Sprintf("实例 #%d", notice.InstanceID)
	if instance, err := db.GetInstanceById(ctx, notice.InstanceID); err == nil && instance != nil {
		instanceName = instance.Name
	}
	hostName := "未知"
	if host, err := db.HostsGetByID(ctx, notice.HostID); err == nil && host != nil {
		hostName = host.Name
	}

	db.CreateLog(ctx, notifierUserID(notice.UserID), "instance", "instance."+notice.TaskType,
		fmt.Sprintf("实例「%s」的 %s 任务失败：%s", instanceName, notice.TaskType, message),
		"failed", db.LogOptionsWithInstance(&notice.InstanceID))

	lib.SendNotification(ctx, int64(notice.UserID), lib.EventInstanceTaskFailed, lib.EventData{
		"instanceName": instanceName,
		"hostName":     hostName,
		"taskType":     notice.TaskType,
		"error":        message,
	})
}

func (n *pgNotifier) InstanceStarted(ctx context.Context, userID int32, instanceName, hostName string, ipv4, ipv6 *string) {
	data := lib.EventData{"instanceName": instanceName, "hostName": hostName}
	if ipv4 != nil {
		data["ipv4"] = *ipv4
	}
	if ipv6 != nil {
		data["ipv6"] = *ipv6
	}
	lib.SendNotification(ctx, int64(userID), lib.EventInstanceStarted, data)
}

func (n *pgNotifier) InstanceStopped(ctx context.Context, userID int32, instanceName, hostName string) {
	lib.SendNotification(ctx, int64(userID), lib.EventInstanceStopped, lib.EventData{
		"instanceName": instanceName,
		"hostName":     hostName,
	})
}

func (n *pgNotifier) InstanceRestarted(ctx context.Context, userID int32, instanceName, hostName string, ipv4, ipv6 *string) {
	data := lib.EventData{"instanceName": instanceName, "hostName": hostName}
	if ipv4 != nil {
		data["ipv4"] = *ipv4
	}
	if ipv6 != nil {
		data["ipv6"] = *ipv6
	}
	lib.SendNotification(ctx, int64(userID), lib.EventInstanceRestarted, data)
}

func (n *pgNotifier) InstanceRebuilt(ctx context.Context, userID int32, instanceName, hostName, image string) {
	lib.SendNotification(ctx, int64(userID), lib.EventInstanceRebuilt, lib.EventData{
		"instanceName": instanceName,
		"hostName":     hostName,
		"newImage":     image,
	})
}

func (n *pgNotifier) BackupRestored(ctx context.Context, userID, instanceID int32, instanceName, backupName, hostName string) {
	lib.SendNotification(ctx, int64(userID), lib.EventBackupRestored, lib.EventData{
		"instanceName": instanceName,
		"backupName":   backupName,
		"hostName":     hostName,
	})
	db.CreateLog(ctx, notifierUserID(userID), "backup", "backup.restore",
		fmt.Sprintf("Successfully restored instance \"%s\" from backup \"%s\"", instanceName, backupName),
		"success", db.LogOptionsWithInstance(&instanceID))
}

func (n *pgNotifier) BackupRestoreFailed(ctx context.Context, userID, instanceID int32, instanceName, message string) {
	db.CreateLog(ctx, notifierUserID(userID), "backup", "backup.restore",
		fmt.Sprintf("Failed to restore instance \"%s\": %s", instanceName, message),
		"failed", db.LogOptionsWithInstance(&instanceID))
}

func (n *pgNotifier) BackupUploaded(ctx context.Context, userID, instanceID int32, instanceName, backupName, storageName, fileName, hostName string) {
	lib.SendNotification(ctx, int64(userID), lib.EventBackupUploaded, lib.EventData{
		"instanceName": instanceName,
		"backupName":   backupName,
		"storageName":  storageName,
		"fileName":     fileName,
		"hostName":     hostName,
	})
	db.CreateLog(ctx, notifierUserID(userID), "backup", "backup.upload",
		fmt.Sprintf("Successfully uploaded backup \"%s\" to \"%s\"", backupName, storageName),
		"success", db.LogOptionsWithInstance(&instanceID))
}

func (n *pgNotifier) BackupUploadFailed(ctx context.Context, userID, instanceID int32, message string) {
	db.CreateLog(ctx, notifierUserID(userID), "backup", "backup.upload",
		"Failed to upload backup: "+message, "failed", db.LogOptionsWithInstance(&instanceID))
}

func (n *pgNotifier) BackupUploadRetry(ctx context.Context, userID, instanceID, attempt, max int32, message string) {
	db.CreateLog(ctx, notifierUserID(userID), "backup", "backup.upload",
		fmt.Sprintf("Retrying upload (attempt %d/%d): %s", attempt, max, message),
		"info", db.LogOptionsWithInstance(&instanceID))
}

// notifySafely runs a side effect that must never change the task outcome. The
// Node workers wrap these calls in try/catch for the same reason: a failed
// notification cannot be allowed to mark a successful task as failed (or vice
// versa).
func notifySafely(label string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("[workers] %s panicked: %v", label, recovered)
		}
	}()
	fn()
}
