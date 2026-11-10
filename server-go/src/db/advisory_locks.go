// Port of server/src/db/advisory-locks.ts.
//
// The Node backend has no Redis: cross-request critical sections are serialized
// with PostgreSQL *transaction-scoped* advisory locks. A transaction-scoped lock
// is released automatically when its transaction ends, so every lock query here
// must run on the very same pgx.Tx as the work it guards. Running
// pg_advisory_xact_lock on the bare pool instead would scope it to an implicit
// single-statement transaction and release it immediately — no mutual exclusion.
package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
)

// Advisory lock namespaces. The numeric values are a cross-process contract that
// keeps unrelated critical sections from colliding, so they must stay identical
// to server/src/db/advisory-locks.ts.
const (
	HostTaskClaimLockNamespace                = 4101
	HostNotificationEmailLockNamespace        = 4102
	HostNotificationEmailLockKey              = 1
	TransferCreateLockNamespace               = 4103
	RedeemCodeLockNamespace                   = 4104
	InstanceOperationLockNamespace            = 4106
	UserDestroyBillingLockNamespace           = 4107
	UserAdminRoleLockNamespace                = 4108
	MailSubscriptionLockNamespace             = 4109
	UserBalanceLockNamespace                  = 4110
	UserPointsLockNamespace                   = 4111
	UserAffBalanceLockNamespace               = 4112
	UserBackupUploadLockNamespace             = 4113
	UserResourcePoolLockNamespace             = 4114
	UserVipBenefitClaimLockNamespace          = 4115
	MailDomainLockNamespace                   = 4116
	UserEmailRegistrationLockNamespace        = 4117
	OperationVerificationRequestLockNamespace = 4118
	IPAddressAllocationLockNamespace          = 4119
	GiftCardLockNamespace                     = 4120
	InstanceTrafficResetLockNamespace         = 4121
	// PackagePrerequisiteLockNamespace serialises package prerequisite checks
	// (mirrors PACKAGE_PREREQUISITE_LOCK_NAMESPACE); the key is always 1.
	PackagePrerequisiteLockNamespace = 4201
	// Ipv6SubnetAllocationLockNamespace is declared in ipv6_subnets.go (4202,
	// key 1) and EgressAdvisoryLockNamespace in egress.go (74201, single-argument
	// form, a different lock space entirely). They are intentionally not repeated
	// here; see those files for the measured details.
)

// TryAdvisoryTransactionLock mirrors tryAdvisoryTransactionLock: it tries to take
// the transaction-scoped lock (namespace, key) without blocking and reports
// whether it succeeded. The lock is held until tx commits or rolls back.
func TryAdvisoryTransactionLock(ctx context.Context, tx pgx.Tx, namespace, key int32) (bool, error) {
	var locked bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1, $2) AS locked", namespace, key).Scan(&locked); err != nil {
		return false, err
	}
	return locked, nil
}

// AdvisoryTransactionLock mirrors advisoryTransactionLock: it blocks until the
// transaction-scoped lock (namespace, key) is acquired.
func AdvisoryTransactionLock(ctx context.Context, tx pgx.Tx, namespace, key int32) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", namespace, key)
	return err
}

// AdvisoryTransactionLockString mirrors advisoryTransactionLockString: same as
// AdvisoryTransactionLock but hashes the string key with PostgreSQL hashtext()
// so arbitrary string keys map onto the two-int advisory lock space.
func AdvisoryTransactionLockString(ctx context.Context, tx pgx.Tx, namespace int32, key string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2))", namespace, key)
	return err
}

// AdvisoryTransactionLockStringTx is the GORM-transaction flavour of
// AdvisoryTransactionLockString. Critical sections that run inside a gorm.DB
// transaction (lib/operation_verification.go is the first one) must take the lock
// on that same transaction, otherwise pg_advisory_xact_lock would be scoped to an
// implicit single-statement transaction and released immediately.
//
// Keep the SQL in exactly one place: the namespace + hashtext(key) scheme is a
// cross-process contract, so a drifting local copy silently breaks mutual
// exclusion. lib/* is allowed to import db; db must never import lib.
func AdvisoryTransactionLockStringTx(tx *gorm.DB, namespace int32, key string) error {
	return tx.Exec(
		"WITH acquired_lock AS (SELECT pg_advisory_xact_lock(?, hashtext(?))) SELECT true AS locked FROM acquired_lock",
		namespace, key,
	).Error
}

// AdvisoryTransactionLockTx is the GORM-transaction flavour of
// AdvisoryTransactionLock (two-int namespace form). Same warning as above: tx must
// be the transaction that performs the guarded work.
func AdvisoryTransactionLockTx(tx *gorm.DB, namespace, key int32) error {
	if tx == nil {
		return errors.New("advisory lock: requires a transaction")
	}
	return tx.Exec(
		"WITH acquired_lock AS (SELECT pg_advisory_xact_lock(?, ?)) SELECT true AS locked FROM acquired_lock",
		namespace, key,
	).Error
}

// TryAdvisoryTransactionLockTx is the GORM-transaction flavour of
// TryAdvisoryTransactionLock: non-blocking, reports whether the lock was taken.
func TryAdvisoryTransactionLockTx(tx *gorm.DB, namespace, key int32) (bool, error) {
	if tx == nil {
		return false, errors.New("advisory lock: requires a transaction")
	}
	var locked bool
	if err := tx.Raw("SELECT pg_try_advisory_xact_lock(?, ?) AS locked", namespace, key).Scan(&locked).Error; err != nil {
		return false, err
	}
	return locked, nil
}
