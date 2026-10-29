// Port of server/src/db/images.ts.
//
// The TypeScript module imports normalizeArchitecture() from lib/architecture.ts.
// server-go/src/lib already imports server-go/src/db, so db must not import lib;
// the helper is therefore mirrored locally as normalizeImageArchitecture (see
// lib/architecture.ts for the canonical implementation). If a shared helper is
// ever introduced it should be wired in through a registration hook, like
// db/logs.go does for the log normalizer.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== 常量 ====================

// ImageType values mirror the TypeScript ImageType union.
const (
	imageTypeContainer = "container"
	imageTypeVM        = "vm"
	imageTypeBoth      = "both"
)

// lowMemoryThreshold mirrors LOW_MEMORY_THRESHOLD (MB).
const lowMemoryThreshold = 128

// lowMemoryAllowedDistros mirrors LOW_MEMORY_ALLOWED_DISTROS. The comparison is
// case-sensitive in the TypeScript (it filters the `icon` column directly).
var lowMemoryAllowedDistros = []string{"alpine", "debian"}

// ImageAvailabilityFailureReason values mirror the TypeScript union.
const (
	ImageAvailabilityHostNotFound             = "host_not_found"
	ImageAvailabilityImageNotFound            = "image_not_found"
	ImageAvailabilityArchitectureMismatch     = "architecture_mismatch"
	ImageAvailabilityHostInstanceTypeMismatch = "host_instance_type_mismatch"
	ImageAvailabilityInstanceTypeMismatch     = "instance_type_mismatch"
	ImageAvailabilityMemoryIncompatible       = "memory_incompatible"
	ImageAvailabilityNotAllowed               = "not_allowed"
)

// ==================== 类型 ====================

// HostImageSelectionState mirrors the TypeScript HostImageSelectionState.
type HostImageSelectionState struct {
	HostID          int32                `json:"hostId"`
	HostName        string               `json:"hostName"`
	Architecture    string               `json:"architecture"`
	InstanceType    string               `json:"instanceType"`
	UseDefault      bool                 `json:"useDefault"`
	AllowedImageIDs []int32              `json:"allowedImageIds"`
	Images          []models.SystemImage `json:"images"`
}

// HostImageAvailability mirrors the TypeScript discriminated union
// HostImageAvailability: OK=false carries Reason only, OK=true carries the rest.
type HostImageAvailability struct {
	OK           bool
	HostID       int32
	ImageID      int32
	Architecture string
	UseDefault   bool
	Reason       string
}

// SystemImageCreateData mirrors the createSystemImage() argument object.
type SystemImageCreateData struct {
	Name         string
	RemoteAlias  string
	OSType       *string
	Architecture *string
	InstanceType *string
	Icon         string
	SortOrder    *int32
	Hidden       *bool
}

// SystemImageUpdateData mirrors the updateSystemImage() argument object: a nil
// pointer means "field absent", which Prisma skips.
type SystemImageUpdateData struct {
	Name         *string
	RemoteAlias  *string
	OSType       *string
	Architecture *string
	InstanceType *string
	Icon         *string
	SortOrder    *int32
	Hidden       *bool
}

// SystemImageAvailabilityOptions mirrors the third argument of
// getSystemImageAvailabilityForHost().
type SystemImageAvailabilityOptions struct {
	InstanceType *string
	Memory       *int
}

// InvalidImageForHostError mirrors the `throw new Error('INVALID_IMAGE_FOR_HOST:<id>')`
// raised by saveHostAllowedImages().
type InvalidImageForHostError struct {
	ImageID int32
}

func (e InvalidImageForHostError) Error() string {
	return fmt.Sprintf("INVALID_IMAGE_FOR_HOST:%d", e.ImageID)
}

// ==================== helpers ====================

// normalizeImageArchitecture mirrors normalizeArchitecture() in
// lib/architecture.ts: anything that is not a known arm64/x86 alias becomes
// x86_64.
func normalizeImageArchitecture(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "aarch64", "arm64":
		return "aarch64"
	case "x86_64", "amd64", "x64", "x86":
		return "x86_64"
	}
	return "x86_64"
}

// getImageInstanceType mirrors getImageInstanceType().
func getImageInstanceType(instanceType string) string {
	if instanceType == "" {
		return imageTypeBoth
	}
	return instanceType
}

// getHostInstanceType mirrors getHostInstanceType().
func getHostInstanceType(instanceType string) string {
	if instanceType == "" {
		return imageTypeContainer
	}
	return instanceType
}

// isImageCompatibleWithTargetType mirrors isImageCompatibleWithTargetType().
func isImageCompatibleWithTargetType(imageType, targetType string) bool {
	if targetType == imageTypeBoth {
		return true
	}
	return imageType == imageTypeBoth || imageType == targetType
}

// isImageCompatibleWithLowMemory mirrors isImageCompatibleWithLowMemory().
func isImageCompatibleWithLowMemory(icon string, memory *int) bool {
	if memory == nil || *memory > lowMemoryThreshold {
		return true
	}
	lowered := strings.ToLower(icon)
	for _, distro := range lowMemoryAllowedDistros {
		if lowered == distro {
			return true
		}
	}
	return false
}

// nonNilImages guarantees the JSON shape Prisma produces for an empty findMany
// (an empty array, never null).
func nonNilImages(images []models.SystemImage) []models.SystemImage {
	if images == nil {
		return []models.SystemImage{}
	}
	return images
}

// findSystemImages runs the shared "visible images, ordered" query.
func findSystemImages(ctx context.Context, query *gorm.DB) ([]models.SystemImage, error) {
	var images []models.SystemImage
	if err := query.Order("sort_order ASC, id ASC").Find(&images).Error; err != nil {
		return nil, err
	}
	return nonNilImages(images), nil
}

// ==================== 系统镜像管理 ====================

// GetAllSystemImages mirrors getAllSystemImages().
func GetAllSystemImages(ctx context.Context) ([]models.SystemImage, error) {
	return findSystemImages(ctx, DB.WithContext(ctx).Model(&models.SystemImage{}))
}

// GetVisibleSystemImages mirrors getVisibleSystemImages().
//
// instanceType is the optional container/vm filter, memory the optional memory
// size in MB (128 MB restricts the list to Alpine/Debian) and hostID the optional
// host whose architecture and image whitelist constrain the result.
func GetVisibleSystemImages(ctx context.Context, instanceType *string, memory *int, hostID *int) ([]models.SystemImage, error) {
	if hostID == nil || *hostID == 0 {
		query := DB.WithContext(ctx).Model(&models.SystemImage{}).Where("hidden = ?", false)
		if instanceType != nil && *instanceType != "" {
			query = query.Where("instance_type IN ?", []string{*instanceType, imageTypeBoth})
		}
		if memory != nil && *memory <= lowMemoryThreshold {
			query = query.Where("icon IN ?", lowMemoryAllowedDistros)
		}
		return findSystemImages(ctx, query)
	}

	hostContext, err := loadHostImageContext(ctx, int32(*hostID))
	if err != nil {
		return nil, err
	}
	if hostContext == nil {
		return []models.SystemImage{}, nil
	}

	query := DB.WithContext(ctx).Model(&models.SystemImage{}).
		Where("hidden = ?", false).
		Where("architecture = ?", hostContext.Architecture)
	if memory != nil && *memory <= lowMemoryThreshold {
		query = query.Where("icon IN ?", lowMemoryAllowedDistros)
	}
	if len(hostContext.AllowedImageIDs) > 0 {
		query = query.Where("id IN ?", hostContext.AllowedImageIDs)
	}

	images, err := findSystemImages(ctx, query)
	if err != nil {
		return nil, err
	}

	filtered := make([]models.SystemImage, 0, len(images))
	for _, image := range images {
		imageType := getImageInstanceType(image.InstanceType)
		if !isImageCompatibleWithTargetType(imageType, hostContext.InstanceType) {
			continue
		}
		if instanceType != nil && *instanceType != "" && !isImageCompatibleWithTargetType(imageType, *instanceType) {
			continue
		}
		filtered = append(filtered, image)
	}
	return filtered, nil
}

// hostImageContext is the subset of a host that image filtering needs.
type hostImageContext struct {
	Architecture    string
	InstanceType    string
	AllowedImageIDs []int32
}

// loadHostImageContext mirrors the `prisma.host.findUnique({ select: { architecture,
// instanceType, allowedImages: { select: { imageId } } } })` projection. It returns
// nil when the host does not exist.
func loadHostImageContext(ctx context.Context, hostID int32) (*hostImageContext, error) {
	var host struct {
		ID           int32  `gorm:"column:id"`
		Architecture string `gorm:"column:architecture"`
		InstanceType string `gorm:"column:instance_type"`
	}
	err := DB.WithContext(ctx).
		Raw("SELECT id, architecture, instance_type FROM hosts WHERE id = ? LIMIT 1", hostID).
		Scan(&host).Error
	if err != nil {
		return nil, err
	}
	if host.ID == 0 {
		return nil, nil
	}

	allowed, err := loadHostAllowedImageIDs(ctx, hostID)
	if err != nil {
		return nil, err
	}

	return &hostImageContext{
		Architecture:    normalizeImageArchitecture(host.Architecture),
		InstanceType:    getHostInstanceType(host.InstanceType),
		AllowedImageIDs: allowed,
	}, nil
}

// loadHostAllowedImageIDs mirrors `host.allowedImages.map(item => item.imageId)`.
// Prisma issues the relation query without an ORDER BY, so neither does this one.
func loadHostAllowedImageIDs(ctx context.Context, hostID int32) ([]int32, error) {
	var rows []struct {
		ImageID int32 `gorm:"column:image_id"`
	}
	if err := DB.WithContext(ctx).
		Raw("SELECT image_id FROM host_allowed_images WHERE host_id = ?", hostID).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	ids := make([]int32, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ImageID)
	}
	return ids, nil
}

// GetHostImageSelectionState mirrors getHostImageSelectionState().
func GetHostImageSelectionState(ctx context.Context, hostID int32) (*HostImageSelectionState, error) {
	var host struct {
		ID           int32  `gorm:"column:id"`
		Name         string `gorm:"column:name"`
		Architecture string `gorm:"column:architecture"`
		InstanceType string `gorm:"column:instance_type"`
	}
	err := DB.WithContext(ctx).
		Raw("SELECT id, name, architecture, instance_type FROM hosts WHERE id = ? LIMIT 1", hostID).
		Scan(&host).Error
	if err != nil {
		return nil, err
	}
	if host.ID == 0 {
		return nil, nil
	}

	hostArchitecture := normalizeImageArchitecture(host.Architecture)
	hostInstanceType := getHostInstanceType(host.InstanceType)
	rawAllowedImageIDs, err := loadHostAllowedImageIDs(ctx, hostID)
	if err != nil {
		return nil, err
	}

	images, err := findSystemImages(ctx, DB.WithContext(ctx).Model(&models.SystemImage{}).
		Where("hidden = ?", false).
		Where("architecture = ?", hostArchitecture))
	if err != nil {
		return nil, err
	}

	compatibleImages := make([]models.SystemImage, 0, len(images))
	compatibleImageIDs := map[int32]bool{}
	for _, image := range images {
		if !isImageCompatibleWithTargetType(getImageInstanceType(image.InstanceType), hostInstanceType) {
			continue
		}
		compatibleImages = append(compatibleImages, image)
		compatibleImageIDs[image.ID] = true
	}

	allowedImageIDs := make([]int32, 0, len(rawAllowedImageIDs))
	for _, imageID := range rawAllowedImageIDs {
		if compatibleImageIDs[imageID] {
			allowedImageIDs = append(allowedImageIDs, imageID)
		}
	}

	return &HostImageSelectionState{
		HostID:          host.ID,
		HostName:        host.Name,
		Architecture:    hostArchitecture,
		InstanceType:    hostInstanceType,
		UseDefault:      len(rawAllowedImageIDs) == 0,
		AllowedImageIDs: allowedImageIDs,
		Images:          compatibleImages,
	}, nil
}

// SaveHostAllowedImages mirrors saveHostAllowedImages(): it replaces the host's
// whitelist, rejecting ids that are not valid candidates for the host.
func SaveHostAllowedImages(ctx context.Context, hostID int32, useDefault bool, imageIDs []int32) (*HostImageSelectionState, error) {
	selectionState, err := GetHostImageSelectionState(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if selectionState == nil {
		return nil, nil
	}

	allowedCandidates := map[int32]bool{}
	for _, image := range selectionState.Images {
		allowedCandidates[image.ID] = true
	}

	normalizedImageIDs := make([]int32, 0, len(imageIDs))
	seen := map[int32]bool{}
	for _, imageID := range imageIDs {
		if seen[imageID] {
			continue
		}
		seen[imageID] = true
		if !allowedCandidates[imageID] {
			return nil, InvalidImageForHostError{ImageID: imageID}
		}
		normalizedImageIDs = append(normalizedImageIDs, imageID)
	}

	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM host_allowed_images WHERE host_id = ?", hostID).Error; err != nil {
			return err
		}
		if useDefault || len(normalizedImageIDs) == 0 {
			return nil
		}

		placeholders := make([]string, 0, len(normalizedImageIDs))
		args := make([]any, 0, len(normalizedImageIDs)*2)
		for _, imageID := range normalizedImageIDs {
			placeholders = append(placeholders, "(?, ?)")
			args = append(args, hostID, imageID)
		}
		// createMany({ skipDuplicates: true }) → ON CONFLICT DO NOTHING.
		sql := "INSERT INTO host_allowed_images (host_id, image_id) VALUES " +
			strings.Join(placeholders, ", ") +
			" ON CONFLICT (host_id, image_id) DO NOTHING"
		return tx.Exec(sql, args...).Error
	})
	if err != nil {
		return nil, err
	}

	return GetHostImageSelectionState(ctx, hostID)
}

// GetSystemImageAvailabilityForHost mirrors getSystemImageAvailabilityForHost().
func GetSystemImageAvailabilityForHost(ctx context.Context, remoteAlias string, hostID int32, options SystemImageAvailabilityOptions) (HostImageAvailability, error) {
	var host struct {
		ID           int32  `gorm:"column:id"`
		Architecture string `gorm:"column:architecture"`
		InstanceType string `gorm:"column:instance_type"`
	}
	err := DB.WithContext(ctx).
		Raw("SELECT id, architecture, instance_type FROM hosts WHERE id = ? LIMIT 1", hostID).
		Scan(&host).Error
	if err != nil {
		return HostImageAvailability{}, err
	}
	if host.ID == 0 {
		return HostImageAvailability{OK: false, Reason: ImageAvailabilityHostNotFound}, nil
	}

	var image struct {
		ID           int32  `gorm:"column:id"`
		Hidden       bool   `gorm:"column:hidden"`
		Architecture string `gorm:"column:architecture"`
		InstanceType string `gorm:"column:instance_type"`
		Icon         string `gorm:"column:icon"`
	}
	err = DB.WithContext(ctx).
		Raw("SELECT id, hidden, architecture, instance_type, icon FROM system_images WHERE remote_alias = ? LIMIT 1", remoteAlias).
		Scan(&image).Error
	if err != nil {
		return HostImageAvailability{}, err
	}
	if image.ID == 0 || image.Hidden {
		return HostImageAvailability{OK: false, Reason: ImageAvailabilityImageNotFound}, nil
	}

	hostArchitecture := normalizeImageArchitecture(host.Architecture)
	imageArchitecture := normalizeImageArchitecture(image.Architecture)
	if imageArchitecture != hostArchitecture {
		return HostImageAvailability{OK: false, Reason: ImageAvailabilityArchitectureMismatch}, nil
	}

	hostInstanceType := getHostInstanceType(host.InstanceType)
	imageType := getImageInstanceType(image.InstanceType)

	if !isImageCompatibleWithTargetType(imageType, hostInstanceType) {
		return HostImageAvailability{OK: false, Reason: ImageAvailabilityHostInstanceTypeMismatch}, nil
	}
	if options.InstanceType != nil && *options.InstanceType != "" && !isImageCompatibleWithTargetType(imageType, *options.InstanceType) {
		return HostImageAvailability{OK: false, Reason: ImageAvailabilityInstanceTypeMismatch}, nil
	}
	if !isImageCompatibleWithLowMemory(image.Icon, options.Memory) {
		return HostImageAvailability{OK: false, Reason: ImageAvailabilityMemoryIncompatible}, nil
	}

	allowedImageIDs, err := loadHostAllowedImageIDs(ctx, hostID)
	if err != nil {
		return HostImageAvailability{}, err
	}
	if len(allowedImageIDs) > 0 {
		found := false
		for _, imageID := range allowedImageIDs {
			if imageID == image.ID {
				found = true
				break
			}
		}
		if !found {
			return HostImageAvailability{OK: false, Reason: ImageAvailabilityNotAllowed}, nil
		}
	}

	return HostImageAvailability{
		OK:           true,
		HostID:       host.ID,
		ImageID:      image.ID,
		Architecture: hostArchitecture,
		UseDefault:   len(allowedImageIDs) == 0,
	}, nil
}

// GetSystemImageByID mirrors getSystemImageById(); it returns nil when absent.
func GetSystemImageByID(ctx context.Context, id int32) (*models.SystemImage, error) {
	var image models.SystemImage
	err := DB.WithContext(ctx).Where("id = ?", id).First(&image).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &image, nil
}

// GetSystemImageByRemoteAlias mirrors getSystemImageByRemoteAlias().
func GetSystemImageByRemoteAlias(ctx context.Context, remoteAlias string) (*models.SystemImage, error) {
	var image models.SystemImage
	err := DB.WithContext(ctx).Where("remote_alias = ?", remoteAlias).First(&image).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &image, nil
}

// GetImageDisplayName mirrors getImageDisplayName(); it accepts aliases with or
// without the `images:` prefix and returns nil when no image matches.
func GetImageDisplayName(ctx context.Context, imageAlias string) (*string, error) {
	withoutPrefix := strings.TrimPrefix(imageAlias, "images:")

	var images []models.SystemImage
	err := DB.WithContext(ctx).
		Where("remote_alias = ? OR remote_alias = ?", withoutPrefix, imageAlias).
		Limit(1).
		Find(&images).Error
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return nil, nil
	}
	name := images[0].Name
	return &name, nil
}

// GetImageDisplayNames mirrors getImageDisplayNames(): a map from the original
// alias to the display name, for aliases that resolve to a known image.
func GetImageDisplayNames(ctx context.Context, imageAliases []string) (map[string]string, error) {
	uniqueAliases := make([]string, 0, len(imageAliases))
	seenAlias := map[string]bool{}
	for _, alias := range imageAliases {
		if seenAlias[alias] {
			continue
		}
		seenAlias[alias] = true
		uniqueAliases = append(uniqueAliases, alias)
	}

	allPossibleAliases := make([]string, 0, len(uniqueAliases)*2)
	for _, alias := range uniqueAliases {
		withoutPrefix := strings.TrimPrefix(alias, "images:")
		allPossibleAliases = append(allPossibleAliases, withoutPrefix)
		if withoutPrefix != alias {
			allPossibleAliases = append(allPossibleAliases, alias)
		}
	}

	result := map[string]string{}
	if len(allPossibleAliases) == 0 {
		return result, nil
	}

	var images []models.SystemImage
	if err := DB.WithContext(ctx).Where("remote_alias IN ?", allPossibleAliases).Find(&images).Error; err != nil {
		return nil, err
	}

	for _, alias := range uniqueAliases {
		withoutPrefix := strings.TrimPrefix(alias, "images:")
		for _, image := range images {
			if image.RemoteAlias == withoutPrefix || image.RemoteAlias == alias {
				result[alias] = image.Name
				break
			}
		}
	}
	return result, nil
}

// CreateSystemImage mirrors createSystemImage().
func CreateSystemImage(ctx context.Context, data SystemImageCreateData) (models.SystemImage, error) {
	osType := "Linux"
	if data.OSType != nil && *data.OSType != "" {
		osType = *data.OSType
	}
	instanceType := imageTypeBoth
	if data.InstanceType != nil && *data.InstanceType != "" {
		instanceType = *data.InstanceType
	}
	architecture := ""
	if data.Architecture != nil {
		architecture = *data.Architecture
	}
	sortOrder := int32(0)
	if data.SortOrder != nil {
		sortOrder = *data.SortOrder
	}

	image := models.SystemImage{
		Name:         data.Name,
		RemoteAlias:  data.RemoteAlias,
		OSType:       osType,
		Architecture: normalizeImageArchitecture(architecture),
		InstanceType: instanceType,
		Icon:         data.Icon,
		SortOrder:    sortOrder,
		Hidden:       data.Hidden != nil && *data.Hidden,
	}
	if err := DB.WithContext(ctx).Create(&image).Error; err != nil {
		return models.SystemImage{}, err
	}
	return image, nil
}

// UpdateSystemImage mirrors updateSystemImage(): only the provided fields are
// written, and architecture is normalized. The @updatedAt column is always
// bumped, exactly like Prisma.
func UpdateSystemImage(ctx context.Context, id int32, data SystemImageUpdateData) (models.SystemImage, error) {
	updates := map[string]any{"updated_at": time.Now()}
	if data.Name != nil {
		updates["name"] = *data.Name
	}
	if data.RemoteAlias != nil {
		updates["remote_alias"] = *data.RemoteAlias
	}
	if data.OSType != nil {
		updates["os_type"] = *data.OSType
	}
	if data.Architecture != nil {
		updates["architecture"] = normalizeImageArchitecture(*data.Architecture)
	}
	if data.InstanceType != nil {
		updates["instance_type"] = *data.InstanceType
	}
	if data.Icon != nil {
		updates["icon"] = *data.Icon
	}
	if data.SortOrder != nil {
		updates["sort_order"] = *data.SortOrder
	}
	if data.Hidden != nil {
		updates["hidden"] = *data.Hidden
	}

	var image models.SystemImage
	err := DB.WithContext(ctx).
		Model(&image).
		Clauses(clause.Returning{}).
		Where("id = ?", id).
		Updates(updates).Error
	if err != nil {
		return models.SystemImage{}, err
	}
	return image, nil
}

// DeleteSystemImage mirrors deleteSystemImage(): it returns the deleted row.
func DeleteSystemImage(ctx context.Context, id int32) (models.SystemImage, error) {
	var image models.SystemImage
	err := DB.WithContext(ctx).
		Clauses(clause.Returning{}).
		Where("id = ?", id).
		Delete(&image).Error
	if err != nil {
		return models.SystemImage{}, err
	}
	return image, nil
}

// IsValidSystemImage mirrors isValidSystemImage(): the alias exists and is not
// hidden.
func IsValidSystemImage(ctx context.Context, remoteAlias string) (bool, error) {
	image, err := GetSystemImageByRemoteAlias(ctx, remoteAlias)
	if err != nil {
		return false, err
	}
	return image != nil && !image.Hidden, nil
}

// IsImageCompatibleWithInstanceType mirrors isImageCompatibleWithInstanceType().
func IsImageCompatibleWithInstanceType(ctx context.Context, remoteAlias, instanceType string) (bool, error) {
	image, err := GetSystemImageByRemoteAlias(ctx, remoteAlias)
	if err != nil {
		return false, err
	}
	if image == nil || image.Hidden {
		return false, nil
	}
	return isImageCompatibleWithTargetType(getImageInstanceType(image.InstanceType), instanceType), nil
}

// IsImageCompatibleWithMemory mirrors isImageCompatibleWithMemory().
func IsImageCompatibleWithMemory(ctx context.Context, remoteAlias string, memory int) (bool, error) {
	image, err := GetSystemImageByRemoteAlias(ctx, remoteAlias)
	if err != nil {
		return false, err
	}
	if image == nil || image.Hidden {
		return false, nil
	}
	return isImageCompatibleWithLowMemory(image.Icon, &memory), nil
}

// ==================== 镜像使用检查 ====================

// CheckImageInUse mirrors checkImageInUse(): how many live instances of the host
// use the alias.
func CheckImageInUse(ctx context.Context, hostID int32, remoteAlias string) (int64, error) {
	var count int64
	err := DB.WithContext(ctx).
		Model(&models.Instance{}).
		Where("host_id = ?", hostID).
		Where("status <> ?", string(models.InstanceStatusDeleted)).
		Where("image = ?", remoteAlias).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return count, nil
}
