// Narrow read projections used by lib/permission.go.
//
// Why this file exists: lib/permission.ts calls db.getHostById and
// db.getInstanceById, which live in db/hosts.ts and db/instances.ts. Those two
// modules are part of the high-risk Phase 6 wave and are not ported yet, so the
// permission checks cannot depend on them. Rather than let a Phase 5 agent
// invent db/hosts.go, this file selects ONLY the columns the permission model
// reads. When the real modules land they can replace these helpers; until then
// they keep the security boundary in exactly one place.
package db

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// GetHostOwnerUserID returns the `user_id` of a host. found is false when the
// host does not exist, mirroring `host && host.user_id === user.id`.
func GetHostOwnerUserID(ctx context.Context, hostID int32) (ownerID int32, found bool, err error) {
	var rows []struct {
		UserID int32 `gorm:"column:user_id"`
	}
	err = DB.WithContext(ctx).
		Table("hosts").
		Select("user_id").
		Where("id = ?", hostID).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return 0, false, err
	}
	if len(rows) == 0 {
		return 0, false, nil
	}
	return rows[0].UserID, true, nil
}

// GetInstancePermissionRef returns the two columns the permission model needs.
// found is false when the instance row does not exist.
func GetInstancePermissionRef(ctx context.Context, instanceID int32) (ref InstancePermissionRef, found bool, err error) {
	var rows []struct {
		UserID int32 `gorm:"column:user_id"`
		HostID int32 `gorm:"column:host_id"`
	}
	err = DB.WithContext(ctx).
		Table("instances").
		Select("user_id, host_id").
		Where("id = ?", instanceID).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return InstancePermissionRef{}, false, nil
		}
		return InstancePermissionRef{}, false, err
	}
	if len(rows) == 0 {
		return InstancePermissionRef{}, false, nil
	}
	return InstancePermissionRef{UserID: rows[0].UserID, HostID: rows[0].HostID}, true, nil
}

// InstancePermissionRef is the projection returned by GetInstancePermissionRef.
// It deliberately does not depend on models.Instance so that generating the
// models again cannot silently widen what the permission layer reads.
type InstancePermissionRef struct {
	UserID int32
	HostID int32
}
