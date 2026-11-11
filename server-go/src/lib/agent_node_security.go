// Port of server/src/lib/node-security/startup.ts.
//
// POST /api/agent/startup-keys. The node is asked to prove it holds the shared
// agent secret inside an encrypted envelope (AES-256-GCM keyed by the secret),
// so an attacker replaying the outer HMAC request cannot obtain the storage
// keys. A failed proof deliberately LOCKS the node (recoveryBlocked) and bumps
// the recovery generation; it never answers "unavailable" in a way that would
// let the node keep running unverified.
//
// Every time value that reaches SQL is UTC (WAVE_D_SPEC §0.2b).
package lib

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// StartupWindowMs mirrors startupWindowMs (3 minutes).
const StartupWindowMs int64 = 180_000

// StartupEnvelope mirrors the StartupEnvelope interface.
type StartupEnvelope struct {
	IV         string `json:"iv"`
	Ciphertext string `json:"ciphertext"`
}

// StartupProof mirrors verifyStartupProof's return value.
type StartupProof struct {
	Nonce     string
	Timestamp int64
}

// StartupGrantResult mirrors the union grantStartupKeys returns: exactly one of
// Unavailable / Invalid / Replay is set, or Envelope carries the sealed answer.
type StartupGrantResult struct {
	Unavailable bool
	Invalid     bool
	Replay      bool
	Generation  int32
	Envelope    *StartupEnvelope
}

var errStartupInvalidEnvelope = errors.New("invalid envelope")

// startupAAD mirrors aad(agentId, direction).
func startupAAD(agentID string, direction string) []byte {
	return []byte("incus-startup-v1:" + direction + ":" + agentID)
}

// startupKey mirrors key(secret): sha256("incus-startup-v1\0" + secret).
func startupKey(secret string) []byte {
	sum := sha256.Sum256([]byte("incus-startup-v1\x00" + secret))
	return sum[:]
}

// SealStartup mirrors sealStartup: AES-256-GCM with a 12-byte random IV and the
// authentication tag appended to the ciphertext, both base64 (standard alphabet).
func SealStartup(secret, agentID, direction string, value any) (*StartupEnvelope, error) {
	plaintext, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	material := startupKey(secret)
	defer wipeBytes(material)

	block, err := aes.NewCipher(material)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, iv, plaintext, startupAAD(agentID, direction))
	return &StartupEnvelope{
		IV:         base64.StdEncoding.EncodeToString(iv),
		Ciphertext: base64.StdEncoding.EncodeToString(sealed),
	}, nil
}

// OpenStartup mirrors openStartup. Every malformed input is rejected with
// `invalid envelope`; GCM authentication failures propagate as errors.
func OpenStartup(secret, agentID, direction string, input any) (any, error) {
	envelope, ok := input.(map[string]any)
	if !ok {
		if typed, typedOK := input.(*StartupEnvelope); typedOK && typed != nil {
			envelope = map[string]any{"iv": typed.IV, "ciphertext": typed.Ciphertext}
		} else {
			return nil, errStartupInvalidEnvelope
		}
	}
	ivText, ivOK := envelope["iv"].(string)
	ciphertextText, ciphertextOK := envelope["ciphertext"].(string)
	if !ivOK || !ciphertextOK || len(ciphertextText) > 90000 {
		return nil, errStartupInvalidEnvelope
	}
	iv, err := base64.StdEncoding.DecodeString(ivText)
	if err != nil {
		return nil, errStartupInvalidEnvelope
	}
	data, err := base64.StdEncoding.DecodeString(ciphertextText)
	if err != nil {
		return nil, errStartupInvalidEnvelope
	}
	if len(iv) != 12 || len(data) < 16 {
		return nil, errStartupInvalidEnvelope
	}

	material := startupKey(secret)
	defer wipeBytes(material)
	block, err := aes.NewCipher(material)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, iv, data, startupAAD(agentID, direction))
	if err != nil {
		return nil, err
	}
	defer wipeBytes(plaintext)

	var value any
	if err := json.Unmarshal(plaintext, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// VerifyStartupProof mirrors verifyStartupProof: the decrypted envelope must
// carry the agent secret itself, a fresh timestamp and a valid nonce.
func VerifyStartupProof(secret, agentID string, envelope any, nowMs int64) (*StartupProof, error) {
	value, err := OpenStartup(secret, agentID, "request", envelope)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("invalid proof")
	}
	token, _ := object["token"].(string)
	expected := []byte(secret)
	actual := []byte(token)
	if len(actual) != len(expected) || subtle.ConstantTimeCompare(actual, expected) != 1 {
		return nil, errors.New("invalid proof")
	}
	timestamp, ok := startupSafeInteger(object["timestamp"])
	if !ok {
		return nil, errors.New("invalid proof")
	}
	delta := nowMs - timestamp
	if delta < 0 {
		delta = -delta
	}
	if delta > StartupWindowMs {
		return nil, errors.New("invalid proof")
	}
	nonce, _ := object["nonce"].(string)
	if !agentNoncePattern.MatchString(nonce) {
		return nil, errors.New("invalid proof")
	}
	return &StartupProof{Nonce: nonce, Timestamp: timestamp}, nil
}

// startupSafeInteger mirrors Number.isSafeInteger over a decoded JSON value.
func startupSafeInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		parsed := int64(typed)
		if parsed > 9007199254740991 || parsed < -9007199254740991 {
			return 0, false
		}
		return parsed, true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	default:
		return 0, false
	}
}

// wipeBytes zeroes key material after use (the TypeScript fills the buffers).
func wipeBytes(buffer []byte) {
	for index := range buffer {
		buffer[index] = 0
	}
}

// GrantStartupKeys mirrors grantStartupKeys. It must only be called after the
// ordinary agent HMAC identity and its outer nonce were verified.
func GrantStartupKeys(ctx context.Context, hostID int32, agentID, secret string, input any) (*StartupGrantResult, error) {
	if db.Pool() == nil {
		return nil, errors.New("database not initialized")
	}

	proof, proofErr := VerifyStartupProof(secret, agentID, input, time.Now().UTC().UnixMilli())
	if proofErr != nil {
		generation, err := startupLockNode(ctx, hostID)
		if err != nil {
			return nil, err
		}
		return &StartupGrantResult{Invalid: true, Generation: generation}, nil
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Same namespace + key as recordReport: one node's security writes serialise
	// against each other.
	if err := db.AdvisoryTransactionLock(ctx, tx, db.NodeSecurityReportLockNamespace, hostID); err != nil {
		return nil, err
	}

	var (
		recoveryBlocked    bool
		recoveryGeneration int32
		reportJSON         []byte
	)
	err = tx.QueryRow(ctx,
		`SELECT recovery_blocked, recovery_generation, report FROM node_security_states WHERE host_id = $1`,
		hostID,
	).Scan(&recoveryBlocked, &recoveryGeneration, &reportJSON)
	stateExists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var report map[string]any
	if stateExists {
		if unmarshalErr := json.Unmarshal(reportJSON, &report); unmarshalErr != nil || report == nil {
			report = map[string]any{}
		}
	}
	lockedValue, lockedIsBool := report["locked"].(bool)
	if !stateExists || !lockedIsBool {
		return &StartupGrantResult{Unavailable: true}, nil
	}

	if recoveryBlocked || lockedValue {
		envelope, sealErr := SealStartup(secret, agentID, "response", map[string]any{
			"nonce":      proof.Nonce,
			"locked":     true,
			"generation": recoveryGeneration,
			"keys":       []any{},
		})
		if sealErr != nil {
			return nil, sealErr
		}
		return &StartupGrantResult{Envelope: envelope}, nil
	}

	now := time.Now().UTC()
	expiresAt := now.Add(2 * time.Duration(StartupWindowMs) * time.Millisecond)
	if _, insertErr := tx.Exec(ctx,
		`INSERT INTO host_agent_nonces (agent_id, nonce, expires_at) VALUES ($1, $2, $3)`,
		agentID, "startup:"+proof.Nonce, expiresAt,
	); insertErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(insertErr, &pgErr) && pgErr.Code == "23505" {
			return &StartupGrantResult{Replay: true}, nil
		}
		return nil, insertErr
	}

	rows, err := tx.Query(ctx,
		`SELECT dataset, ciphertext, version FROM node_storage_keys WHERE host_id = $1 ORDER BY id ASC LIMIT 257`,
		hostID,
	)
	if err != nil {
		return nil, err
	}
	type storageKeyRow struct {
		dataset    string
		ciphertext string
		version    int32
	}
	records := make([]storageKeyRow, 0, 8)
	for rows.Next() {
		var record storageKeyRow
		if err := rows.Scan(&record.dataset, &record.ciphertext, &record.version); err != nil {
			rows.Close()
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(records) > 256 {
		return &StartupGrantResult{Unavailable: true}, nil
	}

	keys := make([]map[string]any, 0, len(records))
	for _, record := range records {
		key, unsealErr := UnsealNodeStorageKey(record.ciphertext, NodeStorageKeyIdentity{
			HostID:  hostID,
			Dataset: record.dataset,
			Version: record.version,
		})
		if unsealErr != nil {
			return nil, fmt.Errorf("unseal storage key %s: %w", record.dataset, unsealErr)
		}
		keys = append(keys, map[string]any{"dataset": record.dataset, "key": key})
	}

	envelope, err := SealStartup(secret, agentID, "response", map[string]any{
		"nonce":      proof.Nonce,
		"locked":     false,
		"generation": recoveryGeneration,
		"keys":       keys,
	})
	if err != nil {
		return nil, err
	}
	// Drop the plaintext keys from memory once they are sealed (the TypeScript
	// clears `item.key` in a finally block).
	for _, item := range keys {
		item["key"] = ""
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &StartupGrantResult{Envelope: envelope}, nil
}

// startupLockNode is the failed-proof branch: durably lock the node, bump the
// recovery generation and leave a security log entry, all in one transaction.
func startupLockNode(ctx context.Context, hostID int32) (int32, error) {
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := db.AdvisoryTransactionLock(ctx, tx, db.NodeSecurityReportLockNamespace, hostID); err != nil {
		return 0, err
	}

	var previousReport []byte
	err = tx.QueryRow(ctx,
		`SELECT report FROM node_security_states WHERE host_id = $1`, hostID,
	).Scan(&previousReport)
	hasPrevious := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	merged := map[string]any{}
	if hasPrevious {
		var decoded map[string]any
		if unmarshalErr := json.Unmarshal(previousReport, &decoded); unmarshalErr == nil && decoded != nil {
			merged = decoded
		}
	}
	merged["locked"] = true
	merged["reason"] = "startup_verification"
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return 0, err
	}
	defaultJSON, err := json.Marshal(map[string]any{
		"locked":   true,
		"enabled":  true,
		"revision": 0,
		"reason":   "startup_verification",
		"results":  []any{},
	})
	if err != nil {
		return 0, err
	}

	var generation int32
	err = tx.QueryRow(ctx, `
		INSERT INTO node_security_states (host_id, recovery_blocked, recovery_generation, report)
		VALUES ($1, true, 1, $3::jsonb)
		ON CONFLICT (host_id) DO UPDATE
		SET recovery_blocked = true,
		    recovery_generation = node_security_states.recovery_generation + 1,
		    report = $2::jsonb
		RETURNING recovery_generation`,
		hostID, string(mergedJSON), string(defaultJSON),
	).Scan(&generation)
	if err != nil {
		return 0, err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO logs (user_id, instance_id, module, action, content, result)
		 VALUES (NULL, NULL, 'security', 'node.startup.rejected', $1, 'warning')`,
		fmt.Sprintf("节点 #%d 启动凭据验证失败，已锁定并等待管理员核验", hostID),
	); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return generation, nil
}
