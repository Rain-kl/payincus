// Port of server/src/lib/host-agent-credentials.ts.
//
// Credential lifecycle for the host agent (WAVE_D_SPEC §0.2, agent plane):
//
//	rotate  — revoke the current identity and issue a brand-new agentId/secret
//	          (the old agent's signatures stop verifying immediately because the
//	          secret row changed and its nonces are dropped);
//	issue   — mint a 30-minute, single-use installation token WITHOUT touching
//	          the running identity (rotating here would disconnect live nodes);
//	consume — burn the token exactly once and hand back the agent secret.
//
// All time values that reach SQL are UTC (WAVE_D_SPEC §0.2b).
package lib

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// AgentInstallTokenPrefix mirrors agentInstallTokenPrefix.
	AgentInstallTokenPrefix = "ait_"
	// AgentInstallTokenTTLMs mirrors agentInstallTokenTtlMs (30 minutes).
	AgentInstallTokenTTLMs int64 = 30 * 60 * 1000
)

// Sentinels that mirror the TypeScript `throw new Error('…')` codes. Routes map
// them to the same status codes the TypeScript uses.
var (
	ErrHostAgentHostNotFound      = errors.New("HOST_NOT_FOUND")
	ErrAgentInstallTokenInvalid   = errors.New("AGENT_INSTALL_TOKEN_INVALID")
	ErrAgentInstallTokenExpired   = errors.New("AGENT_INSTALL_TOKEN_EXPIRED")
	ErrAgentInstallSecretInvalid  = errors.New("AGENT_SECRET_INVALID")
	errHostAgentUniqueIDExhausted = errors.New("Unable to generate unique Agent ID")
)

// HostAgentHostView mirrors the `{ id, name }` host projection the TypeScript
// selects.
type HostAgentHostView struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// HostAgentRotateResult mirrors rotateHostAgentCredentials' return value.
type HostAgentRotateResult struct {
	Host        HostAgentHostView `json:"host"`
	Agent       models.HostAgent  `json:"agent"`
	AgentID     string            `json:"agentId"`
	AgentSecret string            `json:"agentSecret"`
}

// HostAgentInstallTokenResult mirrors issueHostAgentInstallToken's return value.
type HostAgentInstallTokenResult struct {
	Host                  HostAgentHostView `json:"host"`
	Agent                 models.HostAgent  `json:"agent"`
	InstallToken          string            `json:"installToken"`
	InstallTokenExpiresAt time.Time         `json:"installTokenExpiresAt"`
}

// HostAgentConsumeResult mirrors consumeHostAgentInstallToken's return value.
type HostAgentConsumeResult struct {
	Host        HostAgentHostView `json:"host"`
	Agent       models.HostAgent  `json:"agent"`
	AgentSecret string            `json:"agentSecret"`
}

func hostAgentRequireDB() error {
	if db.DB == nil {
		return errors.New("database not initialized")
	}
	return nil
}

// hostAgentGenerateUniqueID mirrors generateUniqueAgentId: up to 8 probes.
func hostAgentGenerateUniqueID(ctx context.Context) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		candidate := GenerateAgentID()
		var count int64
		if err := db.DB.WithContext(ctx).Model(&models.HostAgent{}).
			Where("agent_id = ?", candidate).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return candidate, nil
		}
	}
	return "", errHostAgentUniqueIDExhausted
}

// hostAgentFindHost mirrors `prisma.host.findUnique({ select: { id, name } })`.
func hostAgentFindHost(ctx context.Context, hostID int32) (*HostAgentHostView, error) {
	var rows []struct {
		ID   int32  `gorm:"column:id"`
		Name string `gorm:"column:name"`
	}
	if err := db.DB.WithContext(ctx).Model(&models.Host{}).
		Select("id", "name").Where("id = ?", hostID).Limit(1).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &HostAgentHostView{ID: rows[0].ID, Name: rows[0].Name}, nil
}

// RotateHostAgentCredentials mirrors rotateHostAgentCredentials.
func RotateHostAgentCredentials(ctx context.Context, hostID int32, enabled bool) (*HostAgentRotateResult, error) {
	if err := hostAgentRequireDB(); err != nil {
		return nil, err
	}
	host, err := hostAgentFindHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, ErrHostAgentHostNotFound
	}

	var existing models.HostAgent
	err = db.DB.WithContext(ctx).Where("host_id = ?", hostID).Limit(1).Find(&existing).Error
	if err != nil {
		return nil, err
	}
	if existing.ID != 0 {
		// Drop the old nonces before the agentId change: host_agent_nonces has a
		// foreign key on agent_id, so leaving them would block the update.
		if err := db.DB.WithContext(ctx).
			Where("agent_id = ?", existing.AgentID).Delete(&models.HostAgentNonce{}).Error; err != nil {
			return nil, err
		}
	}

	agentID, err := hostAgentGenerateUniqueID(ctx)
	if err != nil {
		return nil, err
	}
	agentSecret := GenerateAgentSecret()
	encrypted, err := EncryptSensitiveData(agentSecret)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	create := models.HostAgent{
		HostID:          hostID,
		AgentID:         agentID,
		SecretHash:      HashAgentSecret(agentSecret),
		SecretEncrypted: encrypted,
		Enabled:         enabled,
		Status:          "offline",
		Capabilities:    datatypes.JSON("[]"),
		LastReport:      datatypes.JSON("{}"),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	update := map[string]any{
		"agent_id":                 agentID,
		"secret_hash":              HashAgentSecret(agentSecret),
		"secret_encrypted":         encrypted,
		"install_token_hash":       nil,
		"install_token_expires_at": nil,
		"install_token_used_at":    nil,
		"enabled":                  enabled,
		"status":                   "offline",
		"version":                  nil,
		"capabilities":             "[]",
		"last_report":              "{}",
		"last_seen_at":             nil,
		"last_heartbeat_ip":        nil,
		"updated_at":               now,
	}
	if err := db.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "host_id"}},
		DoUpdates: clause.Assignments(update),
	}).Create(&create).Error; err != nil {
		return nil, err
	}

	var agent models.HostAgent
	if err := db.DB.WithContext(ctx).Where("host_id = ?", hostID).Limit(1).Find(&agent).Error; err != nil {
		return nil, err
	}
	return &HostAgentRotateResult{
		Host:        *host,
		Agent:       agent,
		AgentID:     agentID,
		AgentSecret: agentSecret,
	}, nil
}

// generateAgentInstallToken mirrors generateAgentInstallToken.
func generateAgentInstallToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return AgentInstallTokenPrefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// hashAgentInstallToken mirrors hashAgentInstallToken.
func hashAgentInstallToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IsAgentInstallToken mirrors isAgentInstallToken.
func IsAgentInstallToken(value string) bool {
	return len(value) >= len(AgentInstallTokenPrefix)+32 &&
		value[:len(AgentInstallTokenPrefix)] == AgentInstallTokenPrefix
}

// IssueHostAgentInstallToken mirrors issueHostAgentInstallToken. It never
// revokes a running identity: only RotateHostAgentCredentials does that.
func IssueHostAgentInstallToken(ctx context.Context, hostID int32, enabled bool) (*HostAgentInstallTokenResult, error) {
	if err := hostAgentRequireDB(); err != nil {
		return nil, err
	}
	host, err := hostAgentFindHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, ErrHostAgentHostNotFound
	}

	var identity models.HostAgent
	if err := db.DB.WithContext(ctx).Where("host_id = ?", hostID).Limit(1).Find(&identity).Error; err != nil {
		return nil, err
	}
	if identity.ID == 0 {
		agentID, err := hostAgentGenerateUniqueID(ctx)
		if err != nil {
			return nil, err
		}
		agentSecret := GenerateAgentSecret()
		encrypted, err := EncryptSensitiveData(agentSecret)
		if err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		created := models.HostAgent{
			HostID:          hostID,
			AgentID:         agentID,
			SecretHash:      HashAgentSecret(agentSecret),
			SecretEncrypted: encrypted,
			Enabled:         enabled,
			Status:          "offline",
			Capabilities:    datatypes.JSON("[]"),
			LastReport:      datatypes.JSON("{}"),
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		// `create` only: an existing row is left untouched (`update: {}`).
		if err := db.DB.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "host_id"}},
			DoNothing: true,
		}).Create(&created).Error; err != nil {
			return nil, err
		}
		if err := db.DB.WithContext(ctx).Where("host_id = ?", hostID).Limit(1).Find(&identity).Error; err != nil {
			return nil, err
		}
	}

	installToken, err := generateAgentInstallToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	installTokenExpiresAt := now.Add(time.Duration(AgentInstallTokenTTLMs) * time.Millisecond)
	tokenHash := hashAgentInstallToken(installToken)

	if err := db.DB.WithContext(ctx).Model(&models.HostAgent{}).
		Where("id = ?", identity.ID).
		Updates(map[string]any{
			"install_token_hash":       tokenHash,
			"install_token_expires_at": installTokenExpiresAt,
			"install_token_used_at":    nil,
			"updated_at":               now,
		}).Error; err != nil {
		return nil, err
	}

	var agent models.HostAgent
	if err := db.DB.WithContext(ctx).Where("id = ?", identity.ID).Limit(1).Find(&agent).Error; err != nil {
		return nil, err
	}
	return &HostAgentInstallTokenResult{
		Host:                  *host,
		Agent:                 agent,
		InstallToken:          installToken,
		InstallTokenExpiresAt: installTokenExpiresAt,
	}, nil
}

// ConsumeHostAgentInstallToken mirrors consumeHostAgentInstallToken.
func ConsumeHostAgentInstallToken(ctx context.Context, token string) (*HostAgentConsumeResult, error) {
	// The shape check comes first (the TypeScript checks isAgentInstallToken
	// before touching the database), so a malformed token never reaches a query.
	if !IsAgentInstallToken(token) {
		return nil, ErrAgentInstallTokenInvalid
	}
	if err := hostAgentRequireDB(); err != nil {
		return nil, err
	}

	tokenHash := hashAgentInstallToken(token)
	var agent models.HostAgent
	if err := db.DB.WithContext(ctx).
		Where("install_token_hash = ?", tokenHash).Limit(1).Find(&agent).Error; err != nil {
		return nil, err
	}
	if agent.ID == 0 || agent.InstallTokenExpiresAt == nil || agent.InstallTokenUsedAt != nil {
		return nil, ErrAgentInstallTokenInvalid
	}
	now := time.Now().UTC()
	if agent.InstallTokenExpiresAt.Before(now) {
		return nil, ErrAgentInstallTokenExpired
	}

	// Single-use burn: the WHERE clause is the serialisation point, exactly like
	// the TypeScript updateMany + `updated.count !== 1` check.
	tag := db.DB.WithContext(ctx).Model(&models.HostAgent{}).
		Where("id = ? AND install_token_hash = ? AND install_token_used_at IS NULL AND install_token_expires_at >= ?",
			agent.ID, tokenHash, now).
		Updates(map[string]any{
			"install_token_hash":       nil,
			"install_token_expires_at": nil,
			"install_token_used_at":    now,
			"updated_at":               now,
		})
	if tag.Error != nil {
		return nil, tag.Error
	}
	if tag.RowsAffected != 1 {
		return nil, ErrAgentInstallTokenInvalid
	}

	agentSecret := DecryptSensitiveData(agent.SecretEncrypted)
	if agentSecret == "" {
		return nil, ErrAgentInstallSecretInvalid
	}

	host, err := hostAgentFindHost(ctx, agent.HostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, ErrHostAgentHostNotFound
	}
	// The consumed columns are cleared in the database; reflect that in the
	// returned record (the TypeScript destructures the pre-update row, whose
	// token fields are still set, so keep those as-is).
	return &HostAgentConsumeResult{
		Host:        *host,
		Agent:       agent,
		AgentSecret: agentSecret,
	}, nil
}

// hostAgentErrIs mirrors `error instanceof Error && error.message === code`.
func hostAgentErrIs(err error, target error) bool {
	return err != nil && target != nil && errors.Is(err, target)
}
