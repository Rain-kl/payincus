// Port of server/src/lib/permission.ts.
//
// Central permission model (AUTH004). Four actors exist:
//
//	admin       — inherits host-owner power everywhere
//	instance owner — full control of their own instance
//	host owner  — may operate a limited action set on other users' instances
//	              (start/stop/restart/force_stop/view_status/view_logs and
//	              snapshot create/restore/delete/delete-instance)
//	friend      — read-only sharing, checked elsewhere
//
// The host-owner action split is a security boundary: host owners and admins
// must NOT be able to transfer another user's instance, change its quota,
// download its backups, touch its SSH keys, rebuild it or edit its port
// mappings. Those checks are the reason this module exists, so every branch
// below mirrors the TypeScript literally rather than being simplified.
//
// Lookups go through narrow projections declared in db/permission_lookups.go
// instead of the full db/hosts.go / db/instances.go modules (those belong to the
// high-risk Phase 6 wave). The projections select only the columns the checks
// read, so they cannot drift into owning business logic.
package lib

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// AuthUser mirrors the TypeScript AuthUser interface.
type AuthUser struct {
	ID       int32
	Role     string
	Username string
}

// PermissionType mirrors the `permissionType` union.
type PermissionType string

const (
	PermissionOwner     PermissionType = "owner"
	PermissionHostOwner PermissionType = "hostOwner"
	PermissionAdmin     PermissionType = "admin"
	PermissionFriend    PermissionType = "friend"
)

// PermissionResult mirrors the TypeScript PermissionResult.
type PermissionResult struct {
	Allowed        bool
	Reason         string
	PermissionType PermissionType
}

// InstanceRef is the minimal instance projection the permission checks need.
// Mirrors `{ user_id, host_id }`.
type InstanceRef struct {
	UserID int32
	HostID int32
}

// HostOwnerOperation mirrors the HostOwnerOperation union (allowed for host
// owners and admins).
type HostOwnerOperation string

const (
	HostOpStart           HostOwnerOperation = "start"
	HostOpStop            HostOwnerOperation = "stop"
	HostOpRestart         HostOwnerOperation = "restart"
	HostOpForceStop       HostOwnerOperation = "force_stop"
	HostOpViewStatus      HostOwnerOperation = "view_status"
	HostOpViewLogs        HostOwnerOperation = "view_logs"
	HostOpSnapshotCreate  HostOwnerOperation = "snapshot_create"
	HostOpSnapshotRestore HostOwnerOperation = "snapshot_restore"
	HostOpSnapshotDelete  HostOwnerOperation = "snapshot_delete"
	HostOpDelete          HostOwnerOperation = "delete"
)

// OwnerOnlyOperation mirrors the OwnerOnlyOperation union (forbidden for host
// owners AND admins acting on someone else's instance).
type OwnerOnlyOperation string

const (
	OwnerOpTransfer       OwnerOnlyOperation = "transfer"
	OwnerOpModifyQuota    OwnerOnlyOperation = "modify_quota"
	OwnerOpBackupDownload OwnerOnlyOperation = "backup_download"
	OwnerOpBackupUpload   OwnerOnlyOperation = "backup_upload"
	OwnerOpSSHKeyAccess   OwnerOnlyOperation = "ssh_key_access"
	OwnerOpRebuild        OwnerOnlyOperation = "rebuild"
	OwnerOpPortMapping    OwnerOnlyOperation = "port_mapping"
)

// ownerOnlyOperations mirrors the literal array in checkHostOwnerOperationPermission.
var ownerOnlyOperations = map[HostOwnerOperation]bool{
	"transfer":        true,
	"modify_quota":    true,
	"backup_download": true,
	"backup_upload":   true,
	"ssh_key_access":  true,
	"rebuild":         true,
	"port_mapping":    true,
}

// CheckInstancePermission mirrors checkInstancePermission: admin → allowed
// (admin type), owner → allowed (owner type), host owner → allowed
// (hostOwner type), otherwise denied.
func CheckInstancePermission(ctx context.Context, user AuthUser, instance InstanceRef) (PermissionResult, error) {
	if user.Role == "admin" {
		return PermissionResult{Allowed: true, PermissionType: PermissionAdmin}, nil
	}
	if instance.UserID == user.ID {
		return PermissionResult{Allowed: true, PermissionType: PermissionOwner}, nil
	}

	hostOwnerID, found, err := db.GetHostOwnerUserID(ctx, instance.HostID)
	if err != nil {
		return PermissionResult{}, err
	}
	if found && hostOwnerID == user.ID {
		return PermissionResult{Allowed: true, PermissionType: PermissionHostOwner}, nil
	}

	return PermissionResult{Allowed: false, Reason: "No permission to operate this instance"}, nil
}

// CheckInstanceViewPermission mirrors checkInstanceViewPermission, which simply
// delegates to the operate check.
func CheckInstanceViewPermission(ctx context.Context, user AuthUser, instance InstanceRef) (PermissionResult, error) {
	return CheckInstancePermission(ctx, user, instance)
}

// RequireInstancePermission mirrors requireInstancePermission: on denial it has
// already written the 403 response, so the caller must return when ok is false.
func RequireInstancePermission(c *gin.Context, user AuthUser, instance InstanceRef) (bool, error) {
	result, err := CheckInstancePermission(c.Request.Context(), user, instance)
	if err != nil {
		return false, err
	}
	if !result.Allowed {
		RespondAPIError(c, http.StatusForbidden, ErrForbidden, "")
		return false, nil
	}
	return true, nil
}

// RequireInstanceViewPermission mirrors requireInstanceViewPermission.
func RequireInstanceViewPermission(c *gin.Context, user AuthUser, instance InstanceRef) (bool, error) {
	result, err := CheckInstanceViewPermission(c.Request.Context(), user, instance)
	if err != nil {
		return false, err
	}
	if !result.Allowed {
		RespondAPIError(c, http.StatusForbidden, ErrForbidden, "")
		return false, nil
	}
	return true, nil
}

// CheckHostPermission mirrors checkHostPermission: admin → allowed, host owner →
// allowed, otherwise denied.
func CheckHostPermission(user AuthUser, hostOwnerID int32) PermissionResult {
	if user.Role == "admin" {
		return PermissionResult{Allowed: true, PermissionType: PermissionAdmin}
	}
	if hostOwnerID == user.ID {
		return PermissionResult{Allowed: true, PermissionType: PermissionOwner}
	}
	return PermissionResult{Allowed: false, Reason: "No permission to operate this host"}
}

// RequireHostPermission mirrors requireHostPermission.
func RequireHostPermission(c *gin.Context, user AuthUser, hostOwnerID int32) bool {
	result := CheckHostPermission(user, hostOwnerID)
	if !result.Allowed {
		RespondAPIError(c, http.StatusForbidden, ErrForbidden, "")
		return false
	}
	return true
}

// CheckBackupPermission mirrors checkBackupPermission: permission inherits from
// the instance, and a missing instance is reported as a denial (the caller
// distinguishes the 404 itself).
func CheckBackupPermission(ctx context.Context, user AuthUser, instanceID int32) (PermissionResult, bool, error) {
	ref, found, err := db.GetInstancePermissionRef(ctx, instanceID)
	if err != nil {
		return PermissionResult{}, false, err
	}
	if !found {
		return PermissionResult{Allowed: false, Reason: "Instance not found"}, false, nil
	}

	result, err := CheckInstancePermission(ctx, user, InstanceRef{UserID: ref.UserID, HostID: ref.HostID})
	return result, true, err
}

// CheckSnapshotPermission mirrors checkSnapshotPermission.
func CheckSnapshotPermission(ctx context.Context, user AuthUser, instanceID int32) (PermissionResult, bool, error) {
	return CheckBackupPermission(ctx, user, instanceID)
}

// CheckHostOwnerOperationPermission mirrors checkHostOwnerOperationPermission.
//
// Order matters and is observable: the owner check runs first (so an owner is
// never charged a DB round trip), then the host lookup decides whether the
// caller is a host owner / admin at all, and only then is the operation
// allow-list consulted.
func CheckHostOwnerOperationPermission(
	ctx context.Context,
	user AuthUser,
	instance InstanceRef,
	operation string,
) (PermissionResult, error) {
	if instance.UserID == user.ID {
		return PermissionResult{Allowed: true, PermissionType: PermissionOwner}, nil
	}

	isAdmin := user.Role == "admin"

	hostOwnerID, found, err := db.GetHostOwnerUserID(ctx, instance.HostID)
	if err != nil {
		return PermissionResult{}, err
	}
	isHostOwner := found && hostOwnerID == user.ID

	if !isAdmin && !isHostOwner {
		return PermissionResult{Allowed: false, Reason: "No permission to operate this instance"}, nil
	}

	if ownerOnlyOperations[HostOwnerOperation(operation)] {
		kind := PermissionHostOwner
		if isAdmin {
			kind = PermissionAdmin
		}
		return PermissionResult{
			Allowed:        false,
			Reason:         "Host owner/admin cannot perform '" + operation + "' on other users' instances",
			PermissionType: kind,
		}, nil
	}

	kind := PermissionHostOwner
	if isAdmin {
		kind = PermissionAdmin
	}
	return PermissionResult{Allowed: true, PermissionType: kind}, nil
}

// CheckOwnerOnlyPermission mirrors checkOwnerOnlyPermission. Admins are
// explicitly NOT allowed here — this is the stricter check used for sensitive
// operations such as transferring ownership.
func CheckOwnerOnlyPermission(ctx context.Context, user AuthUser, instance InstanceRef) (PermissionResult, error) {
	if instance.UserID == user.ID {
		return PermissionResult{Allowed: true, PermissionType: PermissionOwner}, nil
	}

	if user.Role == "admin" {
		return PermissionResult{
			Allowed:        false,
			Reason:         "This operation is only allowed for the instance owner, not admin",
			PermissionType: PermissionAdmin,
		}, nil
	}

	hostOwnerID, found, err := db.GetHostOwnerUserID(ctx, instance.HostID)
	if err != nil {
		return PermissionResult{}, err
	}
	if found && hostOwnerID == user.ID {
		return PermissionResult{
			Allowed:        false,
			Reason:         "This operation is only allowed for the instance owner, not host owner",
			PermissionType: PermissionHostOwner,
		}, nil
	}

	return PermissionResult{Allowed: false, Reason: "No permission to operate this instance"}, nil
}

// RequireOwnerOnlyPermission mirrors requireOwnerOnlyPermission. The host-owner
// and admin denials use a THIRD error shape (`HOST_OWNER_RESTRICTED`) rather than
// the generic FORBIDDEN envelope, which clients distinguish — keep it verbatim.
func RequireOwnerOnlyPermission(c *gin.Context, user AuthUser, instance InstanceRef) (bool, error) {
	result, err := CheckOwnerOnlyPermission(c.Request.Context(), user, instance)
	if err != nil {
		return false, err
	}
	if !result.Allowed {
		if result.PermissionType == PermissionHostOwner || result.PermissionType == PermissionAdmin {
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "HOST_OWNER_RESTRICTED",
				"message": "This operation is only allowed for the instance owner",
			})
			return false, nil
		}
		RespondAPIError(c, http.StatusForbidden, ErrForbidden, "")
		return false, nil
	}
	return true, nil
}

// CheckPackagePermission mirrors checkPackagePermission: package operations are
// owner-only and admins get no special power here.
func CheckPackagePermission(user AuthUser, packageOwnerID int32) PermissionResult {
	if packageOwnerID == user.ID {
		return PermissionResult{Allowed: true, PermissionType: PermissionOwner}
	}
	return PermissionResult{Allowed: false, Reason: "No permission to operate this package"}
}

// CheckOwnership mirrors checkOwnership.
func CheckOwnership(user AuthUser, resourceOwnerID int32) PermissionResult {
	if user.ID == resourceOwnerID {
		return PermissionResult{Allowed: true, PermissionType: PermissionOwner}
	}
	return PermissionResult{Allowed: false, Reason: "Not the resource owner"}
}

// CheckStrictOwnership mirrors checkStrictOwnership. The TypeScript body is
// identical to checkOwnership; both are kept because call sites name them
// differently and the distinction may matter later.
func CheckStrictOwnership(user AuthUser, resourceOwnerID int32) PermissionResult {
	return CheckOwnership(user, resourceOwnerID)
}

// IsAdmin mirrors isAdmin.
func IsAdmin(user AuthUser) bool { return user.Role == "admin" }

// IsRegularUser mirrors isRegularUser.
func IsRegularUser(user AuthUser) bool { return user.Role == "user" }
