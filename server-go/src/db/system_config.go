// Port of server/src/db/system-config.ts.
//
// System-configuration persistence plus a small amount of value coercion
// (boolean/number/float) and secret masking/encryption. Reads of non-sensitive
// keys are served from the process-local cache in package lib (PERF001); because
// lib imports db, the cache is injected here instead of imported directly.
package db

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// sensitiveConfigKeys mirrors SENSITIVE_CONFIG_KEYS: values are encrypted at rest
// and masked in admin listings.
var sensitiveConfigKeys = map[string]bool{
	"smtp_password":           true,
	"turnstile_secret_key":    true,
	"ticket_image_lsky_token": true,
	"telegram_bot_token":      true,
	"telegram_webhook_secret": true,
}

// maskedSecretPlaceholder mirrors MASKED_SECRET_PLACEHOLDER.
const maskedSecretPlaceholder = "********"

// encryptionIVLength mirrors ENCRYPTION_CONFIG.ivLength (aes-256-gcm, 16-byte IV).
const encryptionIVLength = 16

// DefaultQuotaConfigDefaults mirrors DEFAULT_QUOTA_CONFIG. Hosts, friends and
// packages are all closed by default (0 = not granted) and must be enabled by an
// administrator; instances are intentionally unlimited.
var DefaultQuotaConfigDefaults = map[string]int{
	"default_quota_host":    0,
	"default_quota_friend":  0,
	"default_quota_package": 0,
}

// SystemConfigItem mirrors the getAllSystemConfigs() projection. Field names keep
// the Node snake_case shape.
type SystemConfigItem struct {
	ID          int32   `json:"id"`
	Key         string  `json:"key"`
	Value       string  `json:"value"`
	Type        string  `json:"type"`
	Label       *string `json:"label"`
	Description *string `json:"description"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// DefaultQuotaConfig mirrors the getDefaultQuotaConfig() projection.
type DefaultQuotaConfig struct {
	HostLimit    int `json:"hostLimit"`
	FriendLimit  int `json:"friendLimit"`
	PackageLimit int `json:"packageLimit"`
}

// SystemConfigValueWithUpdatedAt mirrors getSystemConfigValueWithUpdatedAt().
type SystemConfigValueWithUpdatedAt struct {
	Value     *string `json:"value"`
	UpdatedAt *string `json:"updatedAt"`
}

// SystemConfigUpdate is one {key, value} pair for UpdateSystemConfigs.
type SystemConfigUpdate struct {
	Key   string
	Value string
}

type systemConfigSeed struct {
	key         string
	value       string
	configType  string
	label       string
	description string
}

// defaultSystemConfigs mirrors the `configs` array in initSystemConfig().
var defaultSystemConfigs = []systemConfigSeed{
	{"default_quota_host", "0", "number", "默认宿主机配额", "新用户默认可创建宿主机数量（0 = 未授权，需管理员开启）"},
	{"default_quota_friend", "0", "number", "默认好友配额", "新用户默认可添加好友数量（0 = 未授权，需管理员开启）"},
	{"default_quota_package", "0", "number", "默认套餐配额", "新用户默认可创建套餐数量（0 = 未授权，需管理员开启）"},
	{"registration_enabled", "true", "boolean", "开放注册", "是否允许新用户注册账号"},
	{"require_invite_code", "true", "boolean", "邀请码注册", "是否需要邀请码才能注册"},
	{"invite_generation_costs", `[{"resource":"balance","amount":0,"enabled":false},{"resource":"points","amount":0,"enabled":false}]`, "json", "邀请码生成定价", "用户生成邀请码可选择的成本项，当前支持 balance 和 points"},
	{"invite_default_expire_days", "0", "number", "用户邀请码有效期", "用户生成邀请码的默认有效天数，0 表示永不过期"},
	{"user_vip_metric", "totalRecharge", "string", "用户 VIP 统计口径", "用户 VIP 等级全局计算口径：totalRecharge=累计充值，totalConsume=累计消费"},
	{"checkin_enabled", "true", "boolean", "每日签到", "是否开放用户每日签到领取积分"},
	{"checkin_min_points", "1", "number", "签到最小积分", "每日签到随机积分下限"},
	{"checkin_max_points", "500", "number", "签到最大积分", "每日签到随机积分上限"},
	{"checkin_require_instance", "false", "boolean", "签到要求实例", "开启后，用户需要至少拥有一个未删除实例才能签到"},
	{"ticket_enabled", "true", "boolean", "工单开关", "关闭后，普通用户无法发起工单，用户端隐藏工单入口"},
	{"instance_auto_remote_port", "true", "boolean", "自动远程端口转发", "创建实例时自动下发一条远程端口映射（Linux=22/SSH，Windows=3389/RDP）。该映射不计入用户端口配额；用户可在创建实例时取消勾选"},
	{"free_site_mode", "false", "boolean", "白嫖站", "开启后，用户端隐藏充值入口、充值记录和推荐计划"},
	{"free_site_register_gift_enabled", "false", "boolean", "注册自动赠送", "白嫖站开启后，新用户注册成功自动赠送余额和积分"},
	{"free_site_register_gift_balance", "0", "number", "注册赠送余额", "白嫖站注册自动赠送的账户余额（元）"},
	{"free_site_register_gift_points", "0", "number", "注册赠送积分", "白嫖站注册自动赠送的积分数量"},
	{"brand_name", "Incudal", "string", "系统名称", "站点顶部、登录页、SEO 等位置展示的系统名称，留空则使用默认值"},
	{"brand_subtitle", "基于 Incus 的低价 NAT VPS", "string", "网站副标题", "站点顶部、公共页、SEO 默认描述等位置展示的品牌副标题，留空则使用默认值"},
	{"brand_logo_url", "/logo.svg", "string", "系统 Logo 地址", "站点顶部、登录页、SEO 等位置展示的 Logo 图片地址，留空则使用默认值"},
	{"brand_copyright", "版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。", "string", "版权所有", "站点页脚展示的版权所有文案，留空则使用默认值"},
	{"popup_announcement", "", "string", "弹窗公告", "用户访问网站时弹出的公告内容，留空则不广播"},
	{"popup_promo_image_url", "", "string", "图片推广弹窗图片", "用户访问网站时弹出的套餐推广图片 URL，留空则不显示图片推广弹窗"},
	{"popup_promo_package_id", "", "string", "图片推广弹窗套餐", "图片推广弹窗的目标套餐 ID，需配合图片 URL 使用"},
	{"turnstile_enabled", "false", "boolean", "Turnstile 验证", "是否启用 Cloudflare Turnstile 人机验证"},
	{"turnstile_site_key", "", "string", "Turnstile Site Key", "Cloudflare Turnstile 站点密钥（前端使用）"},
	{"turnstile_secret_key", "", "secret", "Turnstile Secret Key", "Cloudflare Turnstile 密钥（后端验证使用）"},
	{"avatar_api_base", "https://api.dicebear.com/9.x", "string", "头像 API 地址", "DiceBear 头像 API 基础地址，可自建服务"},
	{"footer_contact_email", "ryan@io0.cc", "string", "底部联系邮箱", "侧边栏底部邮箱按钮显示的邮箱地址或 mailto 链接"},
	{"footer_telegram_link", "", "string", "底部 Telegram 群链接", "侧边栏底部 Telegram 按钮跳转地址"},
	{"telegram_bot_enabled", "false", "boolean", "Telegram 专用机器人", "是否启用 Telegram 账号绑定机器人"},
	{"telegram_bot_username", "", "string", "Telegram Bot 用户名", "机器人用户名，不含 @，用于生成绑定链接"},
	{"telegram_bot_token", "", "secret", "Telegram Bot Token", "BotFather 分配的专用机器人 Token"},
	{"telegram_webhook_secret", "", "secret", "Telegram Webhook Secret", "Telegram webhook 请求头 X-Telegram-Bot-Api-Secret-Token 校验值"},
	{"telegram_group_join_enabled", "false", "boolean", "Telegram 入群申请", "是否允许绑定用户通过机器人申请私有群邀请链接"},
	{"telegram_group_chat_id", "", "string", "Telegram 私有群 Chat ID", "用于 createChatInviteLink 的私有群或超级群 chat_id"},
	{"telegram_group_join_mode", "any", "string", "Telegram 入群门槛模式", "any=充值或消费任一达标，all=充值和消费同时达标"},
	{"telegram_group_min_recharge", "0", "number", "Telegram 入群累计充值门槛", "累计充值金额达到该值后可申请入群，0 表示不要求"},
	{"telegram_group_min_consume", "0", "number", "Telegram 入群累计消费门槛", "累计消费金额达到该值后可申请入群，0 表示不要求"},
	{"telegram_group_invite_expire_minutes", "30", "number", "Telegram 邀请链接有效期", "机器人生成的一次性邀请链接有效分钟数"},
	{"telegram_vip_group_join_enabled", "false", "boolean", "Telegram 高级群入群申请", "是否允许绑定用户通过机器人申请高级用户群邀请链接"},
	{"telegram_vip_group_chat_id", "", "string", "Telegram 高级群 Chat ID", "用于 createChatInviteLink 的高级私有群或超级群 chat_id"},
	{"telegram_vip_group_join_mode", "all", "string", "Telegram 高级群门槛模式", "any=充值或消费任一达标，all=充值和消费同时达标"},
	{"telegram_vip_group_min_recharge", "0", "number", "Telegram 高级群累计充值门槛", "累计充值金额达到该值后可申请高级群，0 表示不要求"},
	{"telegram_vip_group_min_consume", "0", "number", "Telegram 高级群累计消费门槛", "累计消费金额达到该值后可申请高级群，0 表示不要求"},
	{"telegram_vip_group_invite_expire_minutes", "30", "number", "Telegram 高级群邀请链接有效期", "机器人生成的高级群一次性邀请链接有效分钟数"},
	{"smtp_enabled", "false", "boolean", "SMTP 邮件验证", "是否启用邮件验证码"},
	{"smtp_host", "", "string", "SMTP 服务器", "SMTP 服务器地址"},
	{"smtp_port", "587", "number", "SMTP 端口", "SMTP 服务器端口"},
	{"smtp_secure", "false", "boolean", "SMTP SSL/TLS", "是否使用 SSL/TLS 加密（端口 465 通常启用）"},
	{"smtp_username", "", "string", "SMTP 用户名", "SMTP 认证用户名"},
	{"smtp_password", "", "secret", "SMTP 密码", "SMTP 认证密码"},
	{"smtp_from_email", "", "string", "发件人邮箱", "邮件发件人地址"},
	{"smtp_from_name", "Incudal", "string", "发件人名称", "邮件发件人显示名称"},
	{"email_domain_whitelist_enabled", "false", "boolean", "邮箱域名白名单", "是否启用邮箱域名白名单限制"},
	{"email_allowed_domains", "", "string", "允许的邮箱域名", "允许注册的邮箱域名列表，逗号分隔"},
	{"transfer_fee", "0", "number", "转移手续费", "实例转移手续费（元），0表示免费，发起时扣除，拒绝时退还"},
	{"ticket_image_lsky_base_url", "", "string", "Lsky 地址", "兰空图床站点地址，例如 https://img.example.com"},
	{"ticket_image_lsky_token", "", "secret", "Lsky Token", "兰空图床 API Token（仅后端使用）"},
	{"ticket_image_lsky_api_version", "v1", "string", "Lsky API 版本", "上传时使用的 API 版本：v1 或 v2"},
	{"ticket_image_lsky_target_id", "", "string", "策略/存储 ID", "v1 填 strategy_id，v2 填 storage_id，可留空使用默认策略"},
	{"system_update_allowed_admin_ids", "", "string", "OTA 管理员白名单", "允许执行系统 OTA 更新的管理员 UID，多个用英文逗号分隔；留空时仅 admin 用户名可操作"},
	{"payincus_gift_card_admin_ids", "", "string", "礼品卡管理员白名单", "允许管理礼品卡的管理员 UID，多个用英文逗号分隔；留空时所有管理员可操作"},
}

// ==================== config cache bridge ====================
//
// The cache itself lives in lib/config_cache.go. Because lib already imports db,
// db cannot import lib; lib registers its cache implementation at init time.

// SystemConfigCacheBackend is the process-local config cache used by the db layer.
type SystemConfigCacheBackend interface {
	Get(key string) (*string, bool)
	Set(key string, value *string)
	Invalidate(key string)
	InvalidateAll()
	InvalidateMany(keys []string)
}

var systemConfigCache SystemConfigCacheBackend

// RegisterSystemConfigCache wires lib's config cache into the db layer. It is
// called from lib's package initialization and is not intended for route code.
func RegisterSystemConfigCache(backend SystemConfigCacheBackend) {
	systemConfigCache = backend
}

func configCacheGet(key string) (*string, bool) {
	if systemConfigCache == nil {
		return nil, false
	}
	return systemConfigCache.Get(key)
}

func configCacheSet(key string, value *string) {
	if systemConfigCache != nil {
		systemConfigCache.Set(key, value)
	}
}

func invalidateCachedConfig(key string) {
	if systemConfigCache != nil {
		systemConfigCache.Invalidate(key)
	}
}

func invalidateCachedConfigs(keys []string) {
	if systemConfigCache != nil {
		systemConfigCache.InvalidateMany(keys)
	}
}

func invalidateAllConfigCache() {
	if systemConfigCache != nil {
		systemConfigCache.InvalidateAll()
	}
}

// ==================== init ====================

// InitSystemConfig mirrors initSystemConfig: it seeds missing defaults without
// touching existing rows, then clears the legacy Telegram group link.
func InitSystemConfig(ctx context.Context) error {
	if DB == nil {
		return errors.New("system config: database not initialized")
	}

	for _, seed := range defaultSystemConfigs {
		label := seed.label
		description := seed.description
		row := models.SystemConfig{
			Key:         seed.key,
			Value:       seed.value,
			Type:        seed.configType,
			Label:       &label,
			Description: &description,
		}
		err := DB.WithContext(ctx).
			Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).
			Create(&row).Error
		if err != nil {
			return err
		}
	}

	result := DB.WithContext(ctx).
		Model(&models.SystemConfig{}).
		Where("key = ? AND value = ?", "footer_telegram_link", "https://t.me/incudal_com").
		Update("value", "")
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		invalidateCachedConfigs([]string{"footer_telegram_link"})
	}

	return nil
}

// ==================== typed getters ====================

// GetSystemConfigBoolean mirrors getSystemConfigBoolean.
func GetSystemConfigBoolean(ctx context.Context, key string, defaultValue bool) (bool, error) {
	value, err := GetSystemConfig(ctx, key, true)
	if err != nil {
		return false, err
	}
	if value == nil {
		return defaultValue, nil
	}
	return *value == "true" || *value == "1", nil
}

// IsInviteCodeRequired mirrors isInviteCodeRequired.
func IsInviteCodeRequired(ctx context.Context) (bool, error) {
	return GetSystemConfigBoolean(ctx, "require_invite_code", true)
}

// IsRegistrationEnabled mirrors isRegistrationEnabled.
func IsRegistrationEnabled(ctx context.Context) (bool, error) {
	return GetSystemConfigBoolean(ctx, "registration_enabled", true)
}

// IsInstanceAutoRemotePortEnabled mirrors isInstanceAutoRemotePortEnabled.
func IsInstanceAutoRemotePortEnabled(ctx context.Context) (bool, error) {
	return GetSystemConfigBoolean(ctx, "instance_auto_remote_port", true)
}

// GetSystemConfigNumber mirrors getSystemConfigNumber (JS parseInt base 10).
func GetSystemConfigNumber(ctx context.Context, key string, defaultValue int) (int, error) {
	value, err := GetSystemConfig(ctx, key, true)
	if err != nil {
		return 0, err
	}
	if value == nil {
		return defaultValue, nil
	}
	parsed, ok := parseIntJS(*value)
	if !ok {
		return defaultValue, nil
	}
	return int(parsed), nil
}

// GetSystemConfigFloat mirrors getSystemConfigFloat (JS parseFloat + isFinite).
func GetSystemConfigFloat(ctx context.Context, key string, defaultValue float64) (float64, error) {
	value, err := GetSystemConfig(ctx, key, true)
	if err != nil {
		return 0, err
	}
	if value == nil {
		return defaultValue, nil
	}
	parsed, ok := parseFloatJS(*value)
	if !ok {
		return defaultValue, nil
	}
	return parsed, nil
}

// ==================== read ====================

// GetAllSystemConfigs mirrors getAllSystemConfigs: every row ordered by id, with
// secret values masked.
func GetAllSystemConfigs(ctx context.Context) ([]SystemConfigItem, error) {
	var configs []models.SystemConfig
	if err := DB.WithContext(ctx).Order("id ASC").Find(&configs).Error; err != nil {
		return nil, err
	}

	items := make([]SystemConfigItem, 0, len(configs))
	for _, config := range configs {
		value := config.Value
		if sensitiveConfigKeys[config.Key] && config.Value != "" {
			value = maskedSecretPlaceholder
		}
		items = append(items, SystemConfigItem{
			ID:          config.ID,
			Key:         config.Key,
			Value:       value,
			Type:        config.Type,
			Label:       config.Label,
			Description: config.Description,
			CreatedAt:   isoTimestamp(config.CreatedAt),
			UpdatedAt:   isoTimestamp(config.UpdatedAt),
		})
	}
	return items, nil
}

// GetSystemConfig mirrors getSystemConfig. It returns nil for a missing row (or a
// missing value) and transparently decrypts sensitive values. Non-sensitive keys
// are cached, including negative results.
func GetSystemConfig(ctx context.Context, key string, decrypt bool) (*string, error) {
	isSensitive := sensitiveConfigKeys[key]

	if !isSensitive {
		if cached, found := configCacheGet(key); found {
			return cached, nil
		}
	}

	var config models.SystemConfig
	err := DB.WithContext(ctx).Where("key = ?", key).First(&config).Error
	found := true
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		found = false
	case err != nil:
		return nil, err
	}

	if !found || config.Value == "" {
		var result *string
		if found {
			value := config.Value
			result = &value
		}
		if !isSensitive {
			configCacheSet(key, result)
		}
		return result, nil
	}

	if decrypt && isSensitive && strings.Contains(config.Value, ":") {
		decrypted := decryptSensitiveData(config.Value)
		return &decrypted, nil
	}

	if !isSensitive {
		value := config.Value
		configCacheSet(key, &value)
	}
	value := config.Value
	return &value, nil
}

// GetSystemConfigValueWithUpdatedAt mirrors getSystemConfigValueWithUpdatedAt: it
// returns the raw (undecrypted) value plus the ISO-8601 update timestamp.
func GetSystemConfigValueWithUpdatedAt(ctx context.Context, key string) (SystemConfigValueWithUpdatedAt, error) {
	var config models.SystemConfig
	err := DB.WithContext(ctx).Select("value", "updated_at").Where("key = ?", key).First(&config).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return SystemConfigValueWithUpdatedAt{}, nil
	}
	if err != nil {
		return SystemConfigValueWithUpdatedAt{}, err
	}

	value := config.Value
	updatedAt := isoTimestamp(config.UpdatedAt)
	return SystemConfigValueWithUpdatedAt{Value: &value, UpdatedAt: &updatedAt}, nil
}

// GetDefaultQuotaConfig mirrors getDefaultQuotaConfig.
func GetDefaultQuotaConfig(ctx context.Context) (DefaultQuotaConfig, error) {
	host, err := GetSystemConfigNumber(ctx, "default_quota_host", DefaultQuotaConfigDefaults["default_quota_host"])
	if err != nil {
		return DefaultQuotaConfig{}, err
	}
	friend, err := GetSystemConfigNumber(ctx, "default_quota_friend", DefaultQuotaConfigDefaults["default_quota_friend"])
	if err != nil {
		return DefaultQuotaConfig{}, err
	}
	packageLimit, err := GetSystemConfigNumber(ctx, "default_quota_package", DefaultQuotaConfigDefaults["default_quota_package"])
	if err != nil {
		return DefaultQuotaConfig{}, err
	}
	return DefaultQuotaConfig{HostLimit: host, FriendLimit: friend, PackageLimit: packageLimit}, nil
}

// ==================== write ====================

// UpdateSystemConfig mirrors updateSystemConfig: update one value and drop its
// cache entry. A missing key is an error, like Prisma's P2025.
func UpdateSystemConfig(ctx context.Context, key, value string) error {
	result := DB.WithContext(ctx).Model(&models.SystemConfig{}).Where("key = ?", key).Update("value", value)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("system config %q not found", key)
	}
	invalidateCachedConfig(key)
	return nil
}

// UpdateSystemConfigs mirrors updateSystemConfigs: sensitive values are encrypted
// unless they are the mask placeholder (meaning "keep as is"), all updates run in
// one transaction, and the whole cache is dropped afterwards.
func UpdateSystemConfigs(ctx context.Context, configs []SystemConfigUpdate) error {
	updates := make([]SystemConfigUpdate, 0, len(configs))
	for _, config := range configs {
		if sensitiveConfigKeys[config.Key] && config.Value == maskedSecretPlaceholder {
			continue
		}

		finalValue := config.Value
		if sensitiveConfigKeys[config.Key] && config.Value != "" && !strings.Contains(config.Value, ":") {
			encrypted, err := encryptSensitiveData(config.Value)
			if err != nil {
				return err
			}
			finalValue = encrypted
		}
		updates = append(updates, SystemConfigUpdate{Key: config.Key, Value: finalValue})
	}

	if len(updates) > 0 {
		err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for _, update := range updates {
				if err := tx.Model(&models.SystemConfig{}).Where("key = ?", update.Key).Update("value", update.Value).Error; err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	invalidateAllConfigCache()
	return nil
}

// ==================== helpers ====================

// sensitiveDataKey derives the 32-byte AES key from ENCRYPTION_KEY (falling back
// to JWT_SECRET outside production), mirroring getEncryptionKey() in lib/security.ts.
func sensitiveDataKey() ([]byte, error) {
	secret := os.Getenv("ENCRYPTION_KEY")
	if secret == "" {
		if os.Getenv("NODE_ENV") == "production" {
			return nil, errors.New("ENCRYPTION_KEY is required in production")
		}
		secret = os.Getenv("JWT_SECRET")
		if secret == "" {
			secret = "default-encryption-key"
		}
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], nil
}

// decryptSensitiveData mirrors decryptSensitiveData() in lib/security.ts. It never
// fails hard: malformed input or legacy plaintext is returned unchanged.
func decryptSensitiveData(ciphertext string) string {
	parts := strings.Split(ciphertext, ":")
	if len(parts) != 3 {
		return ciphertext
	}

	key, err := sensitiveDataKey()
	if err != nil {
		return ciphertext
	}
	iv, err := hex.DecodeString(parts[0])
	if err != nil {
		return ciphertext
	}
	tag, err := hex.DecodeString(parts[1])
	if err != nil {
		return ciphertext
	}
	encrypted, err := hex.DecodeString(parts[2])
	if err != nil {
		return ciphertext
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return ciphertext
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		return ciphertext
	}
	plaintext, err := gcm.Open(nil, iv, append(encrypted, tag...), nil)
	if err != nil {
		return ciphertext
	}
	return string(plaintext)
}

// encryptSensitiveData mirrors encryptSensitiveData() in lib/security.ts, emitting
// the same iv:tag:ciphertext hex format.
func encryptSensitiveData(plaintext string) (string, error) {
	key, err := sensitiveDataKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, encryptionIVLength)
	if err != nil {
		return "", err
	}
	iv := make([]byte, encryptionIVLength)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}

	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	tagStart := len(sealed) - gcm.Overhead()
	return hex.EncodeToString(iv) + ":" +
		hex.EncodeToString(sealed[tagStart:]) + ":" +
		hex.EncodeToString(sealed[:tagStart]), nil
}

// isoTimestamp renders a time the way Date#toISOString does (UTC, 3 fractional
// digits, trailing Z).
func isoTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// parseIntJS mirrors parseInt(value, 10): leading whitespace and sign are
// consumed, then digits until the first non-digit; no digits at all is NaN.
func parseIntJS(value string) (int64, bool) {
	trimmed := strings.TrimLeft(value, " \t\n\r\v\f")
	if trimmed == "" {
		return 0, false
	}

	end := 0
	if trimmed[0] == '+' || trimmed[0] == '-' {
		end = 1
	}
	digitsStart := end
	for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
		end++
	}
	if end == digitsStart {
		return 0, false
	}

	parsed, err := strconv.ParseInt(trimmed[:end], 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// parseFloatJS mirrors parseFloat: the longest numeric prefix (sign, digits,
// optional fraction, optional exponent) is parsed; non-finite results are NaN-like
// and reported as not-ok, matching the Number.isFinite guard in the caller.
func parseFloatJS(value string) (float64, bool) {
	trimmed := strings.TrimLeft(value, " \t\n\r\v\f")
	if trimmed == "" {
		return 0, false
	}

	end := 0
	if trimmed[0] == '+' || trimmed[0] == '-' {
		end = 1
	}
	intStart := end
	for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
		end++
	}
	intDigits := end - intStart

	fracDigits := 0
	if end < len(trimmed) && trimmed[end] == '.' {
		end++
		fracStart := end
		for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
			end++
		}
		fracDigits = end - fracStart
	}

	if intDigits == 0 && fracDigits == 0 {
		return 0, false
	}

	if end < len(trimmed) && (trimmed[end] == 'e' || trimmed[end] == 'E') {
		exponentStart := end
		end++
		if end < len(trimmed) && (trimmed[end] == '+' || trimmed[end] == '-') {
			end++
		}
		exponentDigitsStart := end
		for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
			end++
		}
		if end == exponentDigitsStart {
			end = exponentStart
		}
	}

	parsed, err := strconv.ParseFloat(trimmed[:end], 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}
