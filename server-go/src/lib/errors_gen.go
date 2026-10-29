// Code generated from server/src/lib/errors.ts by tools/gen-errors.py. DO NOT EDIT.

package lib

// ErrorCode mirrors the ErrorCode union in the Node backend. The frontend keys off these strings.
type ErrorCode string

const (
	ErrInvalidId                              ErrorCode = "INVALID_ID"
	ErrNotFound                               ErrorCode = "NOT_FOUND"
	ErrUnauthorized                           ErrorCode = "UNAUTHORIZED"
	ErrForbidden                              ErrorCode = "FORBIDDEN"
	ErrAdminRequired                          ErrorCode = "ADMIN_REQUIRED"
	ErrFeatureDisabled                        ErrorCode = "FEATURE_DISABLED"
	ErrInvalidInput                           ErrorCode = "INVALID_INPUT"
	ErrUserNotFound                           ErrorCode = "USER_NOT_FOUND"
	ErrUserExists                             ErrorCode = "USER_EXISTS"
	ErrCannotModifySelf                       ErrorCode = "CANNOT_MODIFY_SELF"
	ErrCannotDeleteSelf                       ErrorCode = "CANNOT_DELETE_SELF"
	ErrCannotBanAdmin                         ErrorCode = "CANNOT_BAN_ADMIN"
	ErrCannotDeleteAdmin                      ErrorCode = "CANNOT_DELETE_ADMIN"
	ErrUserHasInstances                       ErrorCode = "USER_HAS_INSTANCES"
	ErrUserHasResources                       ErrorCode = "USER_HAS_RESOURCES"
	ErrInvalidCredentials                     ErrorCode = "INVALID_CREDENTIALS"
	ErrAccountBanned                          ErrorCode = "ACCOUNT_BANNED"
	ErrTooManyAttempts                        ErrorCode = "TOO_MANY_ATTEMPTS"
	ErrRegistrationDisabled                   ErrorCode = "REGISTRATION_DISABLED"
	ErrInvalidInviteCode                      ErrorCode = "INVALID_INVITE_CODE"
	ErrInviteCodeExpired                      ErrorCode = "INVITE_CODE_EXPIRED"
	ErrInvalid2faCode                         ErrorCode = "INVALID_2FA_CODE"
	ErrTwoFaRequired                          ErrorCode = "TWO_FA_REQUIRED"
	ErrTwoFaAlreadyEnabled                    ErrorCode = "TWO_FA_ALREADY_ENABLED"
	ErrTwoFaNotEnabled                        ErrorCode = "TWO_FA_NOT_ENABLED"
	ErrRefreshTokenMissing                    ErrorCode = "REFRESH_TOKEN_MISSING"
	ErrRefreshTokenInvalid                    ErrorCode = "REFRESH_TOKEN_INVALID"
	ErrSessionNotFound                        ErrorCode = "SESSION_NOT_FOUND"
	ErrInvalidEmail                           ErrorCode = "INVALID_EMAIL"
	ErrEmailContainsIllegal                   ErrorCode = "EMAIL_CONTAINS_ILLEGAL"
	ErrUsernameContainsIllegal                ErrorCode = "USERNAME_CONTAINS_ILLEGAL"
	ErrPasswordTooWeak                        ErrorCode = "PASSWORD_TOO_WEAK"
	ErrInvalidSshKey                          ErrorCode = "INVALID_SSH_KEY"
	ErrSshKeyExists                           ErrorCode = "SSH_KEY_EXISTS"
	ErrInvalidName                            ErrorCode = "INVALID_NAME"
	ErrInstanceNotFound                       ErrorCode = "INSTANCE_NOT_FOUND"
	ErrInstanceAlreadyRunning                 ErrorCode = "INSTANCE_ALREADY_RUNNING"
	ErrInstanceAlreadyStopped                 ErrorCode = "INSTANCE_ALREADY_STOPPED"
	ErrInstanceStatusInvalid                  ErrorCode = "INSTANCE_STATUS_INVALID"
	ErrInstanceSuspended                      ErrorCode = "INSTANCE_SUSPENDED"
	ErrInstanceNotSuspended                   ErrorCode = "INSTANCE_NOT_SUSPENDED"
	ErrInstanceSuspendedExpired               ErrorCode = "INSTANCE_SUSPENDED_EXPIRED"
	ErrInstanceDestroyTrafficLimitExceeded    ErrorCode = "INSTANCE_DESTROY_TRAFFIC_LIMIT_EXCEEDED"
	ErrHostNotFound                           ErrorCode = "HOST_NOT_FOUND"
	ErrHostOffline                            ErrorCode = "HOST_OFFLINE"
	ErrHostOnline                             ErrorCode = "HOST_ONLINE"
	ErrHostHasInstances                       ErrorCode = "HOST_HAS_INSTANCES"
	ErrHostAlreadyOfficial                    ErrorCode = "HOST_ALREADY_OFFICIAL"
	ErrHostTakeoverPackageBindingConflict     ErrorCode = "HOST_TAKEOVER_PACKAGE_BINDING_CONFLICT"
	ErrNoAvailableHosts                       ErrorCode = "NO_AVAILABLE_HOSTS"
	ErrImageNotFound                          ErrorCode = "IMAGE_NOT_FOUND"
	ErrImageSyncedOnHosts                     ErrorCode = "IMAGE_SYNCED_ON_HOSTS"
	ErrImageTypeMismatch                      ErrorCode = "IMAGE_TYPE_MISMATCH"
	ErrImageMemoryIncompatible                ErrorCode = "IMAGE_MEMORY_INCOMPATIBLE"
	ErrPackageNotFound                        ErrorCode = "PACKAGE_NOT_FOUND"
	ErrPackageInUse                           ErrorCode = "PACKAGE_IN_USE"
	ErrPackagePrerequisiteMissing             ErrorCode = "PACKAGE_PREREQUISITE_MISSING"
	ErrQuotaExceeded                          ErrorCode = "QUOTA_EXCEEDED"
	ErrQuotaCpuExceeded                       ErrorCode = "QUOTA_CPU_EXCEEDED"
	ErrQuotaMemoryExceeded                    ErrorCode = "QUOTA_MEMORY_EXCEEDED"
	ErrQuotaDiskExceeded                      ErrorCode = "QUOTA_DISK_EXCEEDED"
	ErrQuotaInstanceExceeded                  ErrorCode = "QUOTA_INSTANCE_EXCEEDED"
	ErrQuotaPortExceeded                      ErrorCode = "QUOTA_PORT_EXCEEDED"
	ErrQuotaSnapshotExceeded                  ErrorCode = "QUOTA_SNAPSHOT_EXCEEDED"
	ErrQuotaBackupExceeded                    ErrorCode = "QUOTA_BACKUP_EXCEEDED"
	ErrQuotaNotAllocated                      ErrorCode = "QUOTA_NOT_ALLOCATED"
	ErrQuotaIncreaseTypeRequired              ErrorCode = "QUOTA_INCREASE_TYPE_REQUIRED"
	ErrQuotaInfoNotFound                      ErrorCode = "QUOTA_INFO_NOT_FOUND"
	ErrQuotaIncreaseStepInvalid               ErrorCode = "QUOTA_INCREASE_STEP_INVALID"
	ErrQuotaUsageUnavailable                  ErrorCode = "QUOTA_USAGE_UNAVAILABLE"
	ErrQuotaUsageTooLow                       ErrorCode = "QUOTA_USAGE_TOO_LOW"
	ErrSnapshotNotFound                       ErrorCode = "SNAPSHOT_NOT_FOUND"
	ErrSnapshotRestoreRequiresStop            ErrorCode = "SNAPSHOT_RESTORE_REQUIRES_STOP"
	ErrBackupNotFound                         ErrorCode = "BACKUP_NOT_FOUND"
	ErrBackupNotReady                         ErrorCode = "BACKUP_NOT_READY"
	ErrExportTaskNotFound                     ErrorCode = "EXPORT_TASK_NOT_FOUND"
	ErrExportTaskExpired                      ErrorCode = "EXPORT_TASK_EXPIRED"
	ErrPortInUse                              ErrorCode = "PORT_IN_USE"
	ErrPortOutOfRange                         ErrorCode = "PORT_OUT_OF_RANGE"
	ErrPortMappingNotFound                    ErrorCode = "PORT_MAPPING_NOT_FOUND"
	ErrPortConflict                           ErrorCode = "PORT_CONFLICT"
	ErrPortRangeMismatch                      ErrorCode = "PORT_RANGE_MISMATCH"
	ErrQuotaPortBatchExceeded                 ErrorCode = "QUOTA_PORT_BATCH_EXCEEDED"
	ErrNotificationChannelNotFound            ErrorCode = "NOTIFICATION_CHANNEL_NOT_FOUND"
	ErrArticleNotFound                        ErrorCode = "ARTICLE_NOT_FOUND"
	ErrSlugExists                             ErrorCode = "SLUG_EXISTS"
	ErrInviteCodeUsed                         ErrorCode = "INVITE_CODE_USED"
	ErrInviteCodeNotFound                     ErrorCode = "INVITE_CODE_NOT_FOUND"
	ErrOauthProviderDisabled                  ErrorCode = "OAUTH_PROVIDER_DISABLED"
	ErrOauthAlreadyBound                      ErrorCode = "OAUTH_ALREADY_BOUND"
	ErrOauthNotBound                          ErrorCode = "OAUTH_NOT_BOUND"
	ErrOauthTokenError                        ErrorCode = "OAUTH_TOKEN_ERROR"
	ErrSshKeyNotFound                         ErrorCode = "SSH_KEY_NOT_FOUND"
	ErrSshKeyRequired                         ErrorCode = "SSH_KEY_REQUIRED"
	ErrSshKeyNotOwned                         ErrorCode = "SSH_KEY_NOT_OWNED"
	ErrPackageUnavailable                     ErrorCode = "PACKAGE_UNAVAILABLE"
	ErrPlanRequired                           ErrorCode = "PLAN_REQUIRED"
	ErrPlanNotFound                           ErrorCode = "PLAN_NOT_FOUND"
	ErrPlanInactive                           ErrorCode = "PLAN_INACTIVE"
	ErrPlanSoldOut                            ErrorCode = "PLAN_SOLD_OUT"
	ErrCannotCreateOwnPaidPackage             ErrorCode = "CANNOT_CREATE_OWN_PAID_PACKAGE"
	ErrBalanceInsufficient                    ErrorCode = "BALANCE_INSUFFICIENT"
	ErrHostUnavailable                        ErrorCode = "HOST_UNAVAILABLE"
	ErrHostNoOnline                           ErrorCode = "HOST_NO_ONLINE"
	ErrHostResourcesNotSynced                 ErrorCode = "HOST_RESOURCES_NOT_SYNCED"
	ErrHostNodeGroupNoHosts                   ErrorCode = "HOST_NODE_GROUP_NO_HOSTS"
	ErrHostResourcesInsufficient              ErrorCode = "HOST_RESOURCES_INSUFFICIENT"
	ErrHostNameExists                         ErrorCode = "HOST_NAME_EXISTS"
	ErrHostAddressExists                      ErrorCode = "HOST_ADDRESS_EXISTS"
	ErrHostAddressUnresolvable                ErrorCode = "HOST_ADDRESS_UNRESOLVABLE"
	ErrHostCertNotConfigured                  ErrorCode = "HOST_CERT_NOT_CONFIGURED"
	ErrHostInvalidCpuMax                      ErrorCode = "HOST_INVALID_CPU_MAX"
	ErrHostInvalidMemoryMax                   ErrorCode = "HOST_INVALID_MEMORY_MAX"
	ErrHostInvalidIpv6Mode                    ErrorCode = "HOST_INVALID_IPV6_MODE"
	ErrHostIpv6RouteRequiresConfig            ErrorCode = "HOST_IPV6_ROUTE_REQUIRES_CONFIG"
	ErrHostCpuBelowUsed                       ErrorCode = "HOST_CPU_BELOW_USED"
	ErrHostMemoryBelowUsed                    ErrorCode = "HOST_MEMORY_BELOW_USED"
	ErrHostInstanceTypeMismatch               ErrorCode = "HOST_INSTANCE_TYPE_MISMATCH"
	ErrInstanceStopRequired                   ErrorCode = "INSTANCE_STOP_REQUIRED"
	ErrInstanceImageRequired                  ErrorCode = "INSTANCE_IMAGE_REQUIRED"
	ErrInstanceImageUnavailable               ErrorCode = "INSTANCE_IMAGE_UNAVAILABLE"
	ErrInstanceRebuildFailed                  ErrorCode = "INSTANCE_REBUILD_FAILED"
	ErrInstanceCloneFailed                    ErrorCode = "INSTANCE_CLONE_FAILED"
	ErrInstanceIpv6NotSupported               ErrorCode = "INSTANCE_IPV6_NOT_SUPPORTED"
	ErrInstanceIpv6ReassignFailed             ErrorCode = "INSTANCE_IPV6_REASSIGN_FAILED"
	ErrInstanceNoIpv4                         ErrorCode = "INSTANCE_NO_IPV4"
	ErrHostNoIpv6Subnet                       ErrorCode = "HOST_NO_IPV6_SUBNET"
	ErrIpv6PoolExhausted                      ErrorCode = "IPV6_POOL_EXHAUSTED"
	ErrIpv6ReassignCooldown                   ErrorCode = "IPV6_REASSIGN_COOLDOWN"
	ErrPortMappingNatOnly                     ErrorCode = "PORT_MAPPING_NAT_ONLY"
	ErrPortNoAvailable                        ErrorCode = "PORT_NO_AVAILABLE"
	ErrPortMappingInvalidId                   ErrorCode = "PORT_MAPPING_INVALID_ID"
	ErrImageCreateFailed                      ErrorCode = "IMAGE_CREATE_FAILED"
	ErrImageUpdateFailed                      ErrorCode = "IMAGE_UPDATE_FAILED"
	ErrImageInvalidHostId                     ErrorCode = "IMAGE_INVALID_HOST_ID"
	ErrBackupCreateFailed                     ErrorCode = "BACKUP_CREATE_FAILED"
	ErrBackupDeleteFailed                     ErrorCode = "BACKUP_DELETE_FAILED"
	ErrBackupExportFailed                     ErrorCode = "BACKUP_EXPORT_FAILED"
	ErrBackupQuotaNotSet                      ErrorCode = "BACKUP_QUOTA_NOT_SET"
	ErrBackupExportStatusInvalid              ErrorCode = "BACKUP_EXPORT_STATUS_INVALID"
	ErrConfigInvalidKey                       ErrorCode = "CONFIG_INVALID_KEY"
	ErrConfigInvalidValue                     ErrorCode = "CONFIG_INVALID_VALUE"
	ErrSnapshotQuotaNotSet                    ErrorCode = "SNAPSHOT_QUOTA_NOT_SET"
	ErrPackageHasInstances                    ErrorCode = "PACKAGE_HAS_INSTANCES"
	ErrResourceCpuExceedsPackage              ErrorCode = "RESOURCE_CPU_EXCEEDS_PACKAGE"
	ErrResourceMemoryExceedsPackage           ErrorCode = "RESOURCE_MEMORY_EXCEEDS_PACKAGE"
	ErrResourceDiskExceedsPackage             ErrorCode = "RESOURCE_DISK_EXCEEDS_PACKAGE"
	ErrQuotaCpuInsufficient                   ErrorCode = "QUOTA_CPU_INSUFFICIENT"
	ErrQuotaMemoryInsufficient                ErrorCode = "QUOTA_MEMORY_INSUFFICIENT"
	ErrQuotaDiskInsufficient                  ErrorCode = "QUOTA_DISK_INSUFFICIENT"
	ErrQuotaInstanceLimitReached              ErrorCode = "QUOTA_INSTANCE_LIMIT_REACHED"
	ErrQuotaHostLimitReached                  ErrorCode = "QUOTA_HOST_LIMIT_REACHED"
	ErrQuotaPackageLimitReached               ErrorCode = "QUOTA_PACKAGE_LIMIT_REACHED"
	ErrQuotaFriendLimitReached                ErrorCode = "QUOTA_FRIEND_LIMIT_REACHED"
	ErrQuotaPortBelowUsed                     ErrorCode = "QUOTA_PORT_BELOW_USED"
	ErrQuotaSnapshotBelowUsed                 ErrorCode = "QUOTA_SNAPSHOT_BELOW_USED"
	ErrQuotaBackupBelowUsed                   ErrorCode = "QUOTA_BACKUP_BELOW_USED"
	ErrQuotaPortTotalExceeded                 ErrorCode = "QUOTA_PORT_TOTAL_EXCEEDED"
	ErrQuotaSnapshotTotalExceeded             ErrorCode = "QUOTA_SNAPSHOT_TOTAL_EXCEEDED"
	ErrQuotaBackupTotalExceeded               ErrorCode = "QUOTA_BACKUP_TOTAL_EXCEEDED"
	ErrImageNotSynced                         ErrorCode = "IMAGE_NOT_SYNCED"
	ErrImageSyncingCannotDelete               ErrorCode = "IMAGE_SYNCING_CANNOT_DELETE"
	ErrImageNoHosts                           ErrorCode = "IMAGE_NO_HOSTS"
	ErrPortRangeInvalid                       ErrorCode = "PORT_RANGE_INVALID"
	ErrSnapshotManualFull                     ErrorCode = "SNAPSHOT_MANUAL_FULL"
	ErrSnapshotRetentionExceeds               ErrorCode = "SNAPSHOT_RETENTION_EXCEEDS"
	ErrBackupManualFull                       ErrorCode = "BACKUP_MANUAL_FULL"
	ErrBackupRetentionExceeds                 ErrorCode = "BACKUP_RETENTION_EXCEEDS"
	ErrOauthNotEnabled                        ErrorCode = "OAUTH_NOT_ENABLED"
	ErrStorageConfigNotFound                  ErrorCode = "STORAGE_CONFIG_NOT_FOUND"
	ErrStorageConfigCreateFailed              ErrorCode = "STORAGE_CONFIG_CREATE_FAILED"
	ErrStorageConfigUpdateFailed              ErrorCode = "STORAGE_CONFIG_UPDATE_FAILED"
	ErrStorageConfigDeleteFailed              ErrorCode = "STORAGE_CONFIG_DELETE_FAILED"
	ErrStorageTypeNotSupported                ErrorCode = "STORAGE_TYPE_NOT_SUPPORTED"
	ErrStoragePoolNotConfigured               ErrorCode = "STORAGE_POOL_NOT_CONFIGURED"
	ErrBackupUploadTaskNotFound               ErrorCode = "BACKUP_UPLOAD_TASK_NOT_FOUND"
	ErrBackupUploadInProgress                 ErrorCode = "BACKUP_UPLOAD_IN_PROGRESS"
	ErrBackupUploadFailed                     ErrorCode = "BACKUP_UPLOAD_FAILED"
	ErrConfigDiskCannotShrink                 ErrorCode = "CONFIG_DISK_CANNOT_SHRINK"
	ErrConfigNoChanges                        ErrorCode = "CONFIG_NO_CHANGES"
	ErrConfigUpdateFailed                     ErrorCode = "CONFIG_UPDATE_FAILED"
	ErrTransferNotFound                       ErrorCode = "TRANSFER_NOT_FOUND"
	ErrTransferToSelf                         ErrorCode = "TRANSFER_TO_SELF"
	ErrTransferToBanned                       ErrorCode = "TRANSFER_TO_BANNED"
	ErrTransferAlreadyPending                 ErrorCode = "TRANSFER_ALREADY_PENDING"
	ErrTransferNotPending                     ErrorCode = "TRANSFER_NOT_PENDING"
	ErrTransferInvalidStatus                  ErrorCode = "TRANSFER_INVALID_STATUS"
	ErrTransferQuotaNotFound                  ErrorCode = "TRANSFER_QUOTA_NOT_FOUND"
	ErrTransferQuotaInsufficient              ErrorCode = "TRANSFER_QUOTA_INSUFFICIENT"
	ErrTransferInstanceLocked                 ErrorCode = "TRANSFER_INSTANCE_LOCKED"
	ErrTransferInsufficientBalance            ErrorCode = "TRANSFER_INSUFFICIENT_BALANCE"
	ErrTransferHostDisabled                   ErrorCode = "TRANSFER_HOST_DISABLED"
	ErrInvalidParams                          ErrorCode = "INVALID_PARAMS"
	ErrCannotAddSelf                          ErrorCode = "CANNOT_ADD_SELF"
	ErrAlreadyFriends                         ErrorCode = "ALREADY_FRIENDS"
	ErrFriendRequestPending                   ErrorCode = "FRIEND_REQUEST_PENDING"
	ErrFriendRequestNotFound                  ErrorCode = "FRIEND_REQUEST_NOT_FOUND"
	ErrFriendRequestNotPending                ErrorCode = "FRIEND_REQUEST_NOT_PENDING"
	ErrFriendshipNotFound                     ErrorCode = "FRIENDSHIP_NOT_FOUND"
	ErrNotFriends                             ErrorCode = "NOT_FRIENDS"
	ErrTargetFriendQuotaFull                  ErrorCode = "TARGET_FRIEND_QUOTA_FULL"
	ErrCannotShareToSelf                      ErrorCode = "CANNOT_SHARE_TO_SELF"
	ErrPackageAlreadyShared                   ErrorCode = "PACKAGE_ALREADY_SHARED"
	ErrShareNotFound                          ErrorCode = "SHARE_NOT_FOUND"
	ErrShareQuotaCpuExceeded                  ErrorCode = "SHARE_QUOTA_CPU_EXCEEDED"
	ErrShareQuotaMemoryExceeded               ErrorCode = "SHARE_QUOTA_MEMORY_EXCEEDED"
	ErrShareQuotaInstancesExceeded            ErrorCode = "SHARE_QUOTA_INSTANCES_EXCEEDED"
	ErrEmailVerificationDisabled              ErrorCode = "EMAIL_VERIFICATION_DISABLED"
	ErrEmailCodeRequired                      ErrorCode = "EMAIL_CODE_REQUIRED"
	ErrInvalidEmailCode                       ErrorCode = "INVALID_EMAIL_CODE"
	ErrTooManyVerificationRequests            ErrorCode = "TOO_MANY_VERIFICATION_REQUESTS"
	ErrEmailSendFailed                        ErrorCode = "EMAIL_SEND_FAILED"
	ErrEmailAlreadyRegistered                 ErrorCode = "EMAIL_ALREADY_REGISTERED"
	ErrEmailDomainNotAllowed                  ErrorCode = "EMAIL_DOMAIN_NOT_ALLOWED"
	ErrMessageNotFound                        ErrorCode = "MESSAGE_NOT_FOUND"
	ErrCheckinNoInstance                      ErrorCode = "CHECKIN_NO_INSTANCE"
	ErrCheckinAlreadyToday                    ErrorCode = "CHECKIN_ALREADY_TODAY"
	ErrRedeemAlreadyToday                     ErrorCode = "REDEEM_ALREADY_TODAY"
	ErrRedeemCodeNotFound                     ErrorCode = "REDEEM_CODE_NOT_FOUND"
	ErrRedeemCodeUsed                         ErrorCode = "REDEEM_CODE_USED"
	ErrRedeemCodeExpired                      ErrorCode = "REDEEM_CODE_EXPIRED"
	ErrRedeemCodeSelfOnly                     ErrorCode = "REDEEM_CODE_SELF_ONLY"
	ErrRedeemCodeDisabled                     ErrorCode = "REDEEM_CODE_DISABLED"
	ErrRedeemCodeInvalidFormat                ErrorCode = "REDEEM_CODE_INVALID_FORMAT"
	ErrRedeemCodeExhausted                    ErrorCode = "REDEEM_CODE_EXHAUSTED"
	ErrRedeemCodeAlreadyUsedByUser            ErrorCode = "REDEEM_CODE_ALREADY_USED_BY_USER"
	ErrRedeemCodeHostMismatch                 ErrorCode = "REDEEM_CODE_HOST_MISMATCH"
	ErrRedeemCodeBatchLimit                   ErrorCode = "REDEEM_CODE_BATCH_LIMIT"
	ErrRedeemExceedsPackageQuota              ErrorCode = "REDEEM_EXCEEDS_PACKAGE_QUOTA"
	ErrRedeemAlreadyAtLimit                   ErrorCode = "REDEEM_ALREADY_AT_LIMIT"
	ErrCheckinCodePaidInstance                ErrorCode = "CHECKIN_CODE_PAID_INSTANCE"
	ErrResourcePoolInsufficient               ErrorCode = "RESOURCE_POOL_INSUFFICIENT"
	ErrResourcePoolKvmCpuMultiple             ErrorCode = "RESOURCE_POOL_KVM_CPU_MULTIPLE"
	ErrResourcePoolKvmMemoryMultiple          ErrorCode = "RESOURCE_POOL_KVM_MEMORY_MULTIPLE"
	ErrResourcePoolKvmDiskMultiple            ErrorCode = "RESOURCE_POOL_KVM_DISK_MULTIPLE"
	ErrResourcePoolVmMustStop                 ErrorCode = "RESOURCE_POOL_VM_MUST_STOP"
	ErrResourcePoolSystemCodeRequiresInstance ErrorCode = "RESOURCE_POOL_SYSTEM_CODE_REQUIRES_INSTANCE"
	ErrInternalError                          ErrorCode = "INTERNAL_ERROR"
	ErrValidationError                        ErrorCode = "VALIDATION_ERROR"
	ErrOperationNotAllowed                    ErrorCode = "OPERATION_NOT_ALLOWED"
	ErrResourceExists                         ErrorCode = "RESOURCE_EXISTS"
	ErrUpstreamError                          ErrorCode = "UPSTREAM_ERROR"
	ErrInsufficientBalance                    ErrorCode = "INSUFFICIENT_BALANCE"
	ErrGiftCardNotFound                       ErrorCode = "GIFT_CARD_NOT_FOUND"
	ErrGiftCardUsed                           ErrorCode = "GIFT_CARD_USED"
	ErrGiftCardExpired                        ErrorCode = "GIFT_CARD_EXPIRED"
	ErrGiftCardDisabled                       ErrorCode = "GIFT_CARD_DISABLED"
	ErrGiftCardSelfRedeem                     ErrorCode = "GIFT_CARD_SELF_REDEEM"
	ErrGiftCardAlreadyUsedByUser              ErrorCode = "GIFT_CARD_ALREADY_USED_BY_USER"
	ErrGiftCardRateLimited                    ErrorCode = "GIFT_CARD_RATE_LIMITED"
	ErrGiftCardInsufficientBalance            ErrorCode = "GIFT_CARD_INSUFFICIENT_BALANCE"
	ErrGiftCardBatchTooLarge                  ErrorCode = "GIFT_CARD_BATCH_TOO_LARGE"
	ErrGiftCardInvalidCode                    ErrorCode = "GIFT_CARD_INVALID_CODE"
	ErrGiftCardTurnstileRequired              ErrorCode = "GIFT_CARD_TURNSTILE_REQUIRED"
)

// ErrorMessages maps every ErrorCode to its canonical English message.
var ErrorMessages = map[ErrorCode]string{
	ErrInvalidId:                              "Invalid ID",
	ErrNotFound:                               "Resource not found",
	ErrUnauthorized:                           "Unauthorized",
	ErrForbidden:                              "Access denied",
	ErrAdminRequired:                          "Admin privileges required",
	ErrFeatureDisabled:                        "This feature has been disabled",
	ErrInvalidInput:                           "Invalid input",
	ErrUserNotFound:                           "User not found",
	ErrUserExists:                             "Username already exists",
	ErrCannotModifySelf:                       "Cannot modify your own status",
	ErrCannotDeleteSelf:                       "Cannot delete yourself",
	ErrCannotBanAdmin:                         "Cannot ban admin account",
	ErrCannotDeleteAdmin:                      "Cannot delete admin account",
	ErrUserHasInstances:                       "User has instances, please delete them first",
	ErrUserHasResources:                       "User owns resources, please transfer or remove them first",
	ErrInvalidCredentials:                     "Invalid username or password",
	ErrAccountBanned:                          "Account has been disabled",
	ErrTooManyAttempts:                        "Too many login attempts, please try again later",
	ErrRegistrationDisabled:                   "Registration is currently closed",
	ErrInvalidInviteCode:                      "Invalid or used invite code",
	ErrInviteCodeExpired:                      "Invite code has expired",
	ErrInvalid2faCode:                         "Invalid verification code or recovery code",
	ErrTwoFaRequired:                          "Two-factor authentication required",
	ErrTwoFaAlreadyEnabled:                    "2FA is already enabled, please disable it first",
	ErrTwoFaNotEnabled:                        "2FA is not enabled",
	ErrRefreshTokenMissing:                    "Refresh token missing",
	ErrRefreshTokenInvalid:                    "Refresh token invalid or expired",
	ErrSessionNotFound:                        "Session not found",
	ErrInvalidEmail:                           "Please enter a valid email address",
	ErrEmailContainsIllegal:                   "Email contains illegal characters",
	ErrUsernameContainsIllegal:                "Username contains illegal characters",
	ErrPasswordTooWeak:                        "Password is too weak",
	ErrInvalidSshKey:                          "Invalid SSH public key format",
	ErrSshKeyExists:                           "This SSH key has already been added",
	ErrInvalidName:                            "Invalid name format",
	ErrInstanceNotFound:                       "Instance not found",
	ErrInstanceAlreadyRunning:                 "Instance is already running",
	ErrInstanceAlreadyStopped:                 "Instance is already stopped",
	ErrInstanceStatusInvalid:                  "Instance status does not allow this operation",
	ErrInstanceSuspended:                      "Instance is suspended and cannot be started",
	ErrInstanceNotSuspended:                   "Instance is not in suspended status",
	ErrInstanceSuspendedExpired:               "Instance is suspended due to expiration, please renew to unsuspend",
	ErrInstanceDestroyTrafficLimitExceeded:    "Current monthly traffic cycle usage has reached the destroy limit",
	ErrHostNotFound:                           "Host not found",
	ErrHostOffline:                            "Host is offline",
	ErrHostOnline:                             "Host is online, reinstall not needed",
	ErrHostHasInstances:                       "Host has instances, please delete them first",
	ErrHostAlreadyOfficial:                    "Host is already official",
	ErrHostTakeoverPackageBindingConflict:     "Takeover blocked because some packages would lose all bound hosts",
	ErrNoAvailableHosts:                       "No available hosts",
	ErrImageNotFound:                          "Image not found",
	ErrImageSyncedOnHosts:                     "Image is synced on hosts, please delete from hosts first",
	ErrImageTypeMismatch:                      "Selected image is not compatible with the package instance type",
	ErrImageMemoryIncompatible:                "128MB memory only supports Alpine/Debian images",
	ErrPackageNotFound:                        "Package not found",
	ErrPackageInUse:                           "Package is in use by instances",
	ErrPackagePrerequisiteMissing:             "Required package instance is missing",
	ErrQuotaExceeded:                          "Quota exceeded",
	ErrQuotaCpuExceeded:                       "CPU quota exceeded",
	ErrQuotaMemoryExceeded:                    "Memory quota exceeded",
	ErrQuotaDiskExceeded:                      "Disk quota exceeded",
	ErrQuotaInstanceExceeded:                  "Instance quota exceeded",
	ErrQuotaPortExceeded:                      "Port quota exceeded",
	ErrQuotaSnapshotExceeded:                  "Snapshot quota exceeded",
	ErrQuotaBackupExceeded:                    "Backup quota exceeded",
	ErrQuotaNotAllocated:                      "Quota not allocated",
	ErrQuotaIncreaseTypeRequired:              "Exactly one quota type must be increased at a time",
	ErrQuotaInfoNotFound:                      "Quota information not found",
	ErrQuotaIncreaseStepInvalid:               "Quota increase step is invalid",
	ErrQuotaUsageUnavailable:                  "Quota usage cannot be calculated",
	ErrQuotaUsageTooLow:                       "Quota usage is below the required threshold",
	ErrSnapshotNotFound:                       "Snapshot not found",
	ErrSnapshotRestoreRequiresStop:            "Please stop the instance before restoring snapshot",
	ErrBackupNotFound:                         "Backup not found",
	ErrBackupNotReady:                         "Backup is not ready for export",
	ErrExportTaskNotFound:                     "Export task not found or expired",
	ErrExportTaskExpired:                      "Export task has expired",
	ErrPortInUse:                              "Port is already in use",
	ErrPortOutOfRange:                         "Port is out of allowed range",
	ErrPortMappingNotFound:                    "Port mapping not found",
	ErrPortConflict:                           "Some ports are already in use",
	ErrPortRangeMismatch:                      "Private and public port range count must match",
	ErrQuotaPortBatchExceeded:                 "Port quota exceeded for batch mapping",
	ErrNotificationChannelNotFound:            "Notification channel not found",
	ErrArticleNotFound:                        "Article not found",
	ErrSlugExists:                             "Slug already exists",
	ErrInviteCodeUsed:                         "Invite code has been used",
	ErrInviteCodeNotFound:                     "Invite code not found",
	ErrOauthProviderDisabled:                  "This login method has been disabled",
	ErrOauthAlreadyBound:                      "This account is already bound to another user",
	ErrOauthNotBound:                          "Account not bound",
	ErrOauthTokenError:                        "Failed to get authorization",
	ErrSshKeyNotFound:                         "SSH key not found",
	ErrSshKeyRequired:                         "SSH key is required, please add one in settings first",
	ErrSshKeyNotOwned:                         "SSH key not found or does not belong to this user",
	ErrPackageUnavailable:                     "Package is unavailable or discontinued",
	ErrPlanRequired:                           "This is a paid package, a plan must be selected",
	ErrPlanNotFound:                           "Plan not found or does not belong to this package",
	ErrPlanInactive:                           "Plan is inactive",
	ErrPlanSoldOut:                            "Plan is sold out",
	ErrCannotCreateOwnPaidPackage:             "Cannot create instance with your own paid package",
	ErrBalanceInsufficient:                    "Insufficient balance",
	ErrHostUnavailable:                        "Selected host is unavailable or has insufficient resources",
	ErrHostNoOnline:                           "No online hosts available, please test connection in node management",
	ErrHostResourcesNotSynced:                 "Host resources not synced, please click \"Test Connection\" in node management",
	ErrHostNodeGroupNoHosts:                   "No available hosts bound to this package",
	ErrHostResourcesInsufficient:              "All hosts have insufficient resources, please try later or choose another package",
	ErrHostNameExists:                         "Host name already exists",
	ErrHostAddressExists:                      "Host connection address already exists",
	ErrHostAddressUnresolvable:                "Panel could not resolve the host connection address",
	ErrHostCertNotConfigured:                  "Please configure certificate path first",
	ErrHostInvalidCpuMax:                      "CPU max allowance cannot be negative",
	ErrHostInvalidMemoryMax:                   "Memory max cannot be negative or less than 256MB",
	ErrHostInvalidIpv6Mode:                    "IPv6 mode must be 1 (route), 2 (NAT), or 3 (disabled)",
	ErrHostIpv6RouteRequiresConfig:            "IPv6 route mode requires subnet and parent interface configuration",
	ErrHostCpuBelowUsed:                       "CPU max allowance cannot be less than current usage by instances",
	ErrHostMemoryBelowUsed:                    "Memory max cannot be less than current usage by instances",
	ErrHostInstanceTypeMismatch:               "Package instance type is not supported by the selected host",
	ErrInstanceStopRequired:                   "Please stop the instance first",
	ErrInstanceImageRequired:                  "Please specify an image",
	ErrInstanceImageUnavailable:               "Selected image is not available on this host",
	ErrInstanceRebuildFailed:                  "Instance rebuild failed",
	ErrInstanceCloneFailed:                    "Instance clone failed",
	ErrInstanceIpv6NotSupported:               "Only NAT + IPv6 mode instances support IPv6 reassignment",
	ErrInstanceIpv6ReassignFailed:             "Failed to reassign IPv6 address",
	ErrInstanceNoIpv4:                         "VM instance requires IPv4 address for port mapping",
	ErrHostNoIpv6Subnet:                       "Host has no IPv6 subnet configured",
	ErrIpv6PoolExhausted:                      "IPv6 address pool exhausted, please try again later",
	ErrIpv6ReassignCooldown:                   "IPv6 can only be reassigned once per day, please try again later",
	ErrPortMappingNatOnly:                     "Port mapping is only supported in NAT or dual-stack mode",
	ErrPortNoAvailable:                        "No available ports, please contact administrator",
	ErrPortMappingInvalidId:                   "Invalid instance or port mapping ID",
	ErrImageCreateFailed:                      "Image creation failed",
	ErrImageUpdateFailed:                      "Image update failed",
	ErrImageInvalidHostId:                     "Invalid image or host ID",
	ErrBackupCreateFailed:                     "Backup creation failed",
	ErrBackupDeleteFailed:                     "Backup deletion failed",
	ErrBackupExportFailed:                     "Backup export failed",
	ErrBackupQuotaNotSet:                      "Please set backup quota for this instance first",
	ErrBackupExportStatusInvalid:              "Export task status is invalid",
	ErrConfigInvalidKey:                       "Invalid configuration key",
	ErrConfigInvalidValue:                     "Configuration value must be a non-negative integer",
	ErrSnapshotQuotaNotSet:                    "Please set snapshot quota for this instance first",
	ErrPackageHasInstances:                    "Package is in use by instances and cannot be deleted",
	ErrResourceCpuExceedsPackage:              "CPU configuration exceeds package limit",
	ErrResourceMemoryExceedsPackage:           "Memory configuration exceeds package limit",
	ErrResourceDiskExceedsPackage:             "Disk configuration exceeds package limit",
	ErrQuotaCpuInsufficient:                   "CPU quota insufficient",
	ErrQuotaMemoryInsufficient:                "Memory quota insufficient",
	ErrQuotaDiskInsufficient:                  "Disk quota insufficient",
	ErrQuotaInstanceLimitReached:              "Instance limit reached",
	ErrQuotaHostLimitReached:                  "Host limit reached",
	ErrQuotaPackageLimitReached:               "Package limit reached",
	ErrQuotaFriendLimitReached:                "Friend limit reached",
	ErrQuotaPortBelowUsed:                     "Port quota cannot be less than current usage",
	ErrQuotaSnapshotBelowUsed:                 "Snapshot quota cannot be less than current usage",
	ErrQuotaBackupBelowUsed:                   "Backup quota cannot be less than current usage",
	ErrQuotaPortTotalExceeded:                 "Total port quota exceeds user limit",
	ErrQuotaSnapshotTotalExceeded:             "Total snapshot quota exceeds user limit",
	ErrQuotaBackupTotalExceeded:               "Total backup quota exceeds user limit",
	ErrImageNotSynced:                         "Image not synced to selected host",
	ErrImageSyncingCannotDelete:               "Image is syncing on hosts and cannot be deleted",
	ErrImageNoHosts:                           "No available hosts, please add hosts first",
	ErrPortRangeInvalid:                       "Port must be within allowed range",
	ErrSnapshotManualFull:                     "Manual snapshots have reached instance quota, cannot enable auto snapshot",
	ErrSnapshotRetentionExceeds:               "Retention count exceeds available quota",
	ErrBackupManualFull:                       "Manual backups have reached instance quota, cannot enable auto backup",
	ErrBackupRetentionExceeds:                 "Retention count exceeds available quota",
	ErrOauthNotEnabled:                        "This OAuth provider is not enabled",
	ErrStorageConfigNotFound:                  "Storage configuration not found",
	ErrStorageConfigCreateFailed:              "Failed to create storage configuration",
	ErrStorageConfigUpdateFailed:              "Failed to update storage configuration",
	ErrStorageConfigDeleteFailed:              "Failed to delete storage configuration",
	ErrStorageTypeNotSupported:                "Storage type not supported",
	ErrStoragePoolNotConfigured:               "No system disk storage pool configured on host",
	ErrBackupUploadTaskNotFound:               "Backup upload task not found",
	ErrBackupUploadInProgress:                 "A backup upload task is already in progress",
	ErrBackupUploadFailed:                     "Backup upload failed",
	ErrConfigDiskCannotShrink:                 "Disk size can only be increased, not decreased",
	ErrConfigNoChanges:                        "No configuration changes provided",
	ErrConfigUpdateFailed:                     "Failed to update instance configuration",
	ErrTransferNotFound:                       "Transfer request not found",
	ErrTransferToSelf:                         "Cannot transfer to yourself",
	ErrTransferToBanned:                       "Cannot transfer to a banned user",
	ErrTransferAlreadyPending:                 "This instance already has a pending transfer request",
	ErrTransferNotPending:                     "Transfer request is not in pending status",
	ErrTransferInvalidStatus:                  "Instance status does not allow transfer",
	ErrTransferQuotaNotFound:                  "Target user quota not found",
	ErrTransferQuotaInsufficient:              "Target user quota insufficient",
	ErrTransferInstanceLocked:                 "Instance is locked due to pending transfer",
	ErrTransferInsufficientBalance:            "Insufficient balance for transfer fee",
	ErrTransferHostDisabled:                   "Transfer is disabled for this host",
	ErrInvalidParams:                          "Invalid parameters",
	ErrCannotAddSelf:                          "Cannot add yourself as a friend",
	ErrAlreadyFriends:                         "You are already friends with this user",
	ErrFriendRequestPending:                   "A friend request is already pending",
	ErrFriendRequestNotFound:                  "Friend request not found",
	ErrFriendRequestNotPending:                "Friend request is not in pending status",
	ErrFriendshipNotFound:                     "Friendship not found",
	ErrNotFriends:                             "You are not friends with this user",
	ErrTargetFriendQuotaFull:                  "Target user friend quota is full",
	ErrCannotShareToSelf:                      "Cannot share package to yourself",
	ErrPackageAlreadyShared:                   "Package is already shared to this user",
	ErrShareNotFound:                          "Share record not found",
	ErrShareQuotaCpuExceeded:                  "Shared package CPU quota exceeded",
	ErrShareQuotaMemoryExceeded:               "Shared package memory quota exceeded",
	ErrShareQuotaInstancesExceeded:            "Shared package instance limit exceeded",
	ErrEmailVerificationDisabled:              "Email verification is not enabled",
	ErrEmailCodeRequired:                      "Email verification code is required",
	ErrInvalidEmailCode:                       "Invalid or expired verification code",
	ErrTooManyVerificationRequests:            "Too many verification requests, please try again later",
	ErrEmailSendFailed:                        "Failed to send verification email",
	ErrEmailAlreadyRegistered:                 "Email address is already registered",
	ErrEmailDomainNotAllowed:                  "This email domain is not allowed for registration",
	ErrMessageNotFound:                        "Message not found",
	ErrCheckinNoInstance:                      "You need at least one instance to check in",
	ErrCheckinAlreadyToday:                    "You have already checked in today",
	ErrRedeemAlreadyToday:                     "You have already redeemed a code today",
	ErrRedeemCodeNotFound:                     "Redeem code not found",
	ErrRedeemCodeUsed:                         "Redeem code has already been used",
	ErrRedeemCodeExpired:                      "Redeem code has expired",
	ErrRedeemCodeSelfOnly:                     "This redeem code can only be used by its owner",
	ErrRedeemCodeDisabled:                     "This redeem code has been disabled",
	ErrRedeemCodeInvalidFormat:                "Invalid code format, only h- prefix codes are supported",
	ErrRedeemCodeExhausted:                    "This redeem code has reached its maximum usage limit",
	ErrRedeemCodeAlreadyUsedByUser:            "You have already used this redeem code",
	ErrRedeemCodeHostMismatch:                 "This redeem code can only be used on instances from the same host",
	ErrRedeemCodeBatchLimit:                   "You have already used a redeem code from this batch",
	ErrRedeemExceedsPackageQuota:              "Redeeming would exceed instance package quota",
	ErrRedeemAlreadyAtLimit:                   "Instance resource is already at package limit",
	ErrCheckinCodePaidInstance:                "Check-in redeem codes can only be used on free instances",
	ErrResourcePoolInsufficient:               "Insufficient resource pool balance",
	ErrResourcePoolKvmCpuMultiple:             "KVM instance CPU must be a multiple of 100",
	ErrResourcePoolKvmMemoryMultiple:          "KVM instance memory must be a multiple of 128MB",
	ErrResourcePoolKvmDiskMultiple:            "KVM instance disk must be a multiple of 1GB (1024MB)",
	ErrResourcePoolVmMustStop:                 "KVM instance must be stopped to adjust memory or disk",
	ErrResourcePoolSystemCodeRequiresInstance: "System redeem code requires an instance to be specified",
	ErrInternalError:                          "Internal server error",
	ErrValidationError:                        "Validation error",
	ErrOperationNotAllowed:                    "Operation not allowed",
	ErrResourceExists:                         "Resource already exists",
	ErrUpstreamError:                          "Upstream service error",
	ErrInsufficientBalance:                    "Insufficient balance",
	ErrGiftCardNotFound:                       "Gift card not found",
	ErrGiftCardUsed:                           "Gift card has already been used",
	ErrGiftCardExpired:                        "Gift card has expired",
	ErrGiftCardDisabled:                       "Gift card has been disabled",
	ErrGiftCardSelfRedeem:                     "Cannot redeem your own gift card",
	ErrGiftCardAlreadyUsedByUser:              "You have already used this gift card",
	ErrGiftCardRateLimited:                    "Too many attempts, please try again later",
	ErrGiftCardInsufficientBalance:            "Insufficient balance to generate gift card",
	ErrGiftCardBatchTooLarge:                  "Batch count exceeds maximum allowed",
	ErrGiftCardInvalidCode:                    "Invalid gift card code format",
	ErrGiftCardTurnstileRequired:              "Turnstile verification required",
}

// AllErrorCodes lists every code in declaration order (used by parity tests).
var AllErrorCodes = []ErrorCode{
	ErrInvalidId,
	ErrNotFound,
	ErrUnauthorized,
	ErrForbidden,
	ErrAdminRequired,
	ErrFeatureDisabled,
	ErrInvalidInput,
	ErrUserNotFound,
	ErrUserExists,
	ErrCannotModifySelf,
	ErrCannotDeleteSelf,
	ErrCannotBanAdmin,
	ErrCannotDeleteAdmin,
	ErrUserHasInstances,
	ErrUserHasResources,
	ErrInvalidCredentials,
	ErrAccountBanned,
	ErrTooManyAttempts,
	ErrRegistrationDisabled,
	ErrInvalidInviteCode,
	ErrInviteCodeExpired,
	ErrInvalid2faCode,
	ErrTwoFaRequired,
	ErrTwoFaAlreadyEnabled,
	ErrTwoFaNotEnabled,
	ErrRefreshTokenMissing,
	ErrRefreshTokenInvalid,
	ErrSessionNotFound,
	ErrInvalidEmail,
	ErrEmailContainsIllegal,
	ErrUsernameContainsIllegal,
	ErrPasswordTooWeak,
	ErrInvalidSshKey,
	ErrSshKeyExists,
	ErrInvalidName,
	ErrInstanceNotFound,
	ErrInstanceAlreadyRunning,
	ErrInstanceAlreadyStopped,
	ErrInstanceStatusInvalid,
	ErrInstanceSuspended,
	ErrInstanceNotSuspended,
	ErrInstanceSuspendedExpired,
	ErrInstanceDestroyTrafficLimitExceeded,
	ErrHostNotFound,
	ErrHostOffline,
	ErrHostOnline,
	ErrHostHasInstances,
	ErrHostAlreadyOfficial,
	ErrHostTakeoverPackageBindingConflict,
	ErrNoAvailableHosts,
	ErrImageNotFound,
	ErrImageSyncedOnHosts,
	ErrImageTypeMismatch,
	ErrImageMemoryIncompatible,
	ErrPackageNotFound,
	ErrPackageInUse,
	ErrPackagePrerequisiteMissing,
	ErrQuotaExceeded,
	ErrQuotaCpuExceeded,
	ErrQuotaMemoryExceeded,
	ErrQuotaDiskExceeded,
	ErrQuotaInstanceExceeded,
	ErrQuotaPortExceeded,
	ErrQuotaSnapshotExceeded,
	ErrQuotaBackupExceeded,
	ErrQuotaNotAllocated,
	ErrQuotaIncreaseTypeRequired,
	ErrQuotaInfoNotFound,
	ErrQuotaIncreaseStepInvalid,
	ErrQuotaUsageUnavailable,
	ErrQuotaUsageTooLow,
	ErrSnapshotNotFound,
	ErrSnapshotRestoreRequiresStop,
	ErrBackupNotFound,
	ErrBackupNotReady,
	ErrExportTaskNotFound,
	ErrExportTaskExpired,
	ErrPortInUse,
	ErrPortOutOfRange,
	ErrPortMappingNotFound,
	ErrPortConflict,
	ErrPortRangeMismatch,
	ErrQuotaPortBatchExceeded,
	ErrNotificationChannelNotFound,
	ErrArticleNotFound,
	ErrSlugExists,
	ErrInviteCodeUsed,
	ErrInviteCodeNotFound,
	ErrOauthProviderDisabled,
	ErrOauthAlreadyBound,
	ErrOauthNotBound,
	ErrOauthTokenError,
	ErrSshKeyNotFound,
	ErrSshKeyRequired,
	ErrSshKeyNotOwned,
	ErrPackageUnavailable,
	ErrPlanRequired,
	ErrPlanNotFound,
	ErrPlanInactive,
	ErrPlanSoldOut,
	ErrCannotCreateOwnPaidPackage,
	ErrBalanceInsufficient,
	ErrHostUnavailable,
	ErrHostNoOnline,
	ErrHostResourcesNotSynced,
	ErrHostNodeGroupNoHosts,
	ErrHostResourcesInsufficient,
	ErrHostNameExists,
	ErrHostAddressExists,
	ErrHostAddressUnresolvable,
	ErrHostCertNotConfigured,
	ErrHostInvalidCpuMax,
	ErrHostInvalidMemoryMax,
	ErrHostInvalidIpv6Mode,
	ErrHostIpv6RouteRequiresConfig,
	ErrHostCpuBelowUsed,
	ErrHostMemoryBelowUsed,
	ErrHostInstanceTypeMismatch,
	ErrInstanceStopRequired,
	ErrInstanceImageRequired,
	ErrInstanceImageUnavailable,
	ErrInstanceRebuildFailed,
	ErrInstanceCloneFailed,
	ErrInstanceIpv6NotSupported,
	ErrInstanceIpv6ReassignFailed,
	ErrInstanceNoIpv4,
	ErrHostNoIpv6Subnet,
	ErrIpv6PoolExhausted,
	ErrIpv6ReassignCooldown,
	ErrPortMappingNatOnly,
	ErrPortNoAvailable,
	ErrPortMappingInvalidId,
	ErrImageCreateFailed,
	ErrImageUpdateFailed,
	ErrImageInvalidHostId,
	ErrBackupCreateFailed,
	ErrBackupDeleteFailed,
	ErrBackupExportFailed,
	ErrBackupQuotaNotSet,
	ErrBackupExportStatusInvalid,
	ErrConfigInvalidKey,
	ErrConfigInvalidValue,
	ErrSnapshotQuotaNotSet,
	ErrPackageHasInstances,
	ErrResourceCpuExceedsPackage,
	ErrResourceMemoryExceedsPackage,
	ErrResourceDiskExceedsPackage,
	ErrQuotaCpuInsufficient,
	ErrQuotaMemoryInsufficient,
	ErrQuotaDiskInsufficient,
	ErrQuotaInstanceLimitReached,
	ErrQuotaHostLimitReached,
	ErrQuotaPackageLimitReached,
	ErrQuotaFriendLimitReached,
	ErrQuotaPortBelowUsed,
	ErrQuotaSnapshotBelowUsed,
	ErrQuotaBackupBelowUsed,
	ErrQuotaPortTotalExceeded,
	ErrQuotaSnapshotTotalExceeded,
	ErrQuotaBackupTotalExceeded,
	ErrImageNotSynced,
	ErrImageSyncingCannotDelete,
	ErrImageNoHosts,
	ErrPortRangeInvalid,
	ErrSnapshotManualFull,
	ErrSnapshotRetentionExceeds,
	ErrBackupManualFull,
	ErrBackupRetentionExceeds,
	ErrOauthNotEnabled,
	ErrStorageConfigNotFound,
	ErrStorageConfigCreateFailed,
	ErrStorageConfigUpdateFailed,
	ErrStorageConfigDeleteFailed,
	ErrStorageTypeNotSupported,
	ErrStoragePoolNotConfigured,
	ErrBackupUploadTaskNotFound,
	ErrBackupUploadInProgress,
	ErrBackupUploadFailed,
	ErrConfigDiskCannotShrink,
	ErrConfigNoChanges,
	ErrConfigUpdateFailed,
	ErrTransferNotFound,
	ErrTransferToSelf,
	ErrTransferToBanned,
	ErrTransferAlreadyPending,
	ErrTransferNotPending,
	ErrTransferInvalidStatus,
	ErrTransferQuotaNotFound,
	ErrTransferQuotaInsufficient,
	ErrTransferInstanceLocked,
	ErrTransferInsufficientBalance,
	ErrTransferHostDisabled,
	ErrInvalidParams,
	ErrCannotAddSelf,
	ErrAlreadyFriends,
	ErrFriendRequestPending,
	ErrFriendRequestNotFound,
	ErrFriendRequestNotPending,
	ErrFriendshipNotFound,
	ErrNotFriends,
	ErrTargetFriendQuotaFull,
	ErrCannotShareToSelf,
	ErrPackageAlreadyShared,
	ErrShareNotFound,
	ErrShareQuotaCpuExceeded,
	ErrShareQuotaMemoryExceeded,
	ErrShareQuotaInstancesExceeded,
	ErrEmailVerificationDisabled,
	ErrEmailCodeRequired,
	ErrInvalidEmailCode,
	ErrTooManyVerificationRequests,
	ErrEmailSendFailed,
	ErrEmailAlreadyRegistered,
	ErrEmailDomainNotAllowed,
	ErrMessageNotFound,
	ErrCheckinNoInstance,
	ErrCheckinAlreadyToday,
	ErrRedeemAlreadyToday,
	ErrRedeemCodeNotFound,
	ErrRedeemCodeUsed,
	ErrRedeemCodeExpired,
	ErrRedeemCodeSelfOnly,
	ErrRedeemCodeDisabled,
	ErrRedeemCodeInvalidFormat,
	ErrRedeemCodeExhausted,
	ErrRedeemCodeAlreadyUsedByUser,
	ErrRedeemCodeHostMismatch,
	ErrRedeemCodeBatchLimit,
	ErrRedeemExceedsPackageQuota,
	ErrRedeemAlreadyAtLimit,
	ErrCheckinCodePaidInstance,
	ErrResourcePoolInsufficient,
	ErrResourcePoolKvmCpuMultiple,
	ErrResourcePoolKvmMemoryMultiple,
	ErrResourcePoolKvmDiskMultiple,
	ErrResourcePoolVmMustStop,
	ErrResourcePoolSystemCodeRequiresInstance,
	ErrInternalError,
	ErrValidationError,
	ErrOperationNotAllowed,
	ErrResourceExists,
	ErrUpstreamError,
	ErrInsufficientBalance,
	ErrGiftCardNotFound,
	ErrGiftCardUsed,
	ErrGiftCardExpired,
	ErrGiftCardDisabled,
	ErrGiftCardSelfRedeem,
	ErrGiftCardAlreadyUsedByUser,
	ErrGiftCardRateLimited,
	ErrGiftCardInsufficientBalance,
	ErrGiftCardBatchTooLarge,
	ErrGiftCardInvalidCode,
	ErrGiftCardTurnstileRequired,
}
