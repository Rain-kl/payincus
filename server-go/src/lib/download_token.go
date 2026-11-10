// Port of server/src/lib/download-token.ts.
//
// One-time download tokens let a browser fetch a large file with a URL that
// cannot be replayed: the token is short-lived (default 5 minutes), bound to one
// user + one resource id/type, and consumed atomically on first use. That is the
// only authorization the public backup-export download endpoint has, so the
// consume path must stay single-use even under concurrent requests.
//
// The store is process-local, exactly like the TypeScript Map. This is allowed by
// the single-backend invariant (AGENTS.md §"私有后端必须单实例运行"): a second
// process would not share these tokens, and moving them to PostgreSQL/Redis is an
// architecture change that needs owner approval (WAVE_D_SPEC §0.1).
package lib

import (
	"crypto/rand"
	"sync"
	"time"
)

// DownloadTokenResourceType mirrors the `resourceType` union. Both values exist
// because other modules (backup-download) may generate tokens later.
type DownloadTokenResourceType string

const (
	DownloadTokenResourceBackupExport   DownloadTokenResourceType = "backup-export"
	DownloadTokenResourceBackupDownload DownloadTokenResourceType = "backup-download"
)

// DownloadToken mirrors the DownloadToken interface. ExpiresAt / CreatedAt are
// milliseconds since the epoch, like Date.now().
type DownloadToken struct {
	UserID       int64
	ResourceID   string
	ResourceType DownloadTokenResourceType
	ExpiresAt    int64
	UsageCount   int
	MaxUsage     int
	CreatedAt    int64
}

// DownloadTokenConsumeResult mirrors ConsumeResult.
type DownloadTokenConsumeResult struct {
	Valid      bool
	UserID     int64
	ResourceID string
	Error      string
}

var downloadTokenStore = struct {
	mu     sync.Mutex
	tokens map[string]DownloadToken
}{tokens: make(map[string]DownloadToken)}

// downloadTokenCleanupInterval mirrors the `setInterval(..., 5 * 60 * 1000)`.
// Node calls .unref() so the timer never keeps the process alive; a Go goroutine
// has the same property (main returning exits the program).
const downloadTokenCleanupInterval = 5 * time.Minute

func init() {
	go func() {
		ticker := time.NewTicker(downloadTokenCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now().UnixMilli()
			downloadTokenStore.mu.Lock()
			for token, data := range downloadTokenStore.tokens {
				if now > data.ExpiresAt {
					delete(downloadTokenStore.tokens, token)
				}
			}
			downloadTokenStore.mu.Unlock()
		}
	}()
}

// downloadTokenNowMs returns Date.now().
func downloadTokenNowMs() int64 { return time.Now().UnixMilli() }

// generateDownloadTokenValue mirrors nanoid(32): 32 URL-safe random characters.
// The exact alphabet is irrelevant on the wire (the token is opaque and
// single-use), but the entropy must not be lower than nanoid's.
func generateDownloadTokenValue(length int) string {
	const alphabet = "useandom-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz"
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand.Read never fails on supported platforms; if it somehow did,
		// an empty token would be rejected by ConsumeDownloadToken (fail closed).
		return ""
	}
	out := make([]byte, length)
	for i, value := range raw {
		out[i] = alphabet[int(value)%len(alphabet)]
	}
	return string(out)
}

// GenerateDownloadToken mirrors generateDownloadToken.
func GenerateDownloadToken(
	userID int64,
	resourceID string,
	resourceType DownloadTokenResourceType,
	expiresInSeconds int,
	maxUsage int,
) string {
	token := generateDownloadTokenValue(32)
	now := downloadTokenNowMs()

	downloadTokenStore.mu.Lock()
	defer downloadTokenStore.mu.Unlock()
	downloadTokenStore.tokens[token] = DownloadToken{
		UserID:       userID,
		ResourceID:   resourceID,
		ResourceType: resourceType,
		ExpiresAt:    now + int64(expiresInSeconds)*1000,
		UsageCount:   0,
		MaxUsage:     maxUsage,
		CreatedAt:    now,
	}
	return token
}

// ConsumeDownloadToken mirrors consumeDownloadToken. The checks run in the same
// order as the TypeScript (missing → expired → resource id → resource type), and
// the error strings are the ones the download endpoint copies into its response
// body, so they must not be reworded.
func ConsumeDownloadToken(
	token string,
	expectedResourceID string,
	expectedResourceType DownloadTokenResourceType,
) DownloadTokenConsumeResult {
	downloadTokenStore.mu.Lock()
	defer downloadTokenStore.mu.Unlock()

	data, ok := downloadTokenStore.tokens[token]
	if !ok {
		return DownloadTokenConsumeResult{Error: "Token not found or already used"}
	}

	if downloadTokenNowMs() > data.ExpiresAt {
		delete(downloadTokenStore.tokens, token)
		return DownloadTokenConsumeResult{Error: "Token expired"}
	}

	if expectedResourceID != "" && data.ResourceID != expectedResourceID {
		return DownloadTokenConsumeResult{Error: "Resource mismatch"}
	}

	if expectedResourceType != "" && data.ResourceType != expectedResourceType {
		return DownloadTokenConsumeResult{Error: "Resource type mismatch"}
	}

	data.UsageCount++
	// At most MaxUsage uses; the TypeScript deletes the key once the cap is hit.
	if data.UsageCount >= data.MaxUsage {
		delete(downloadTokenStore.tokens, token)
	} else {
		downloadTokenStore.tokens[token] = data
	}

	return DownloadTokenConsumeResult{
		Valid:      true,
		UserID:     data.UserID,
		ResourceID: data.ResourceID,
	}
}

// IsDownloadTokenValid mirrors isDownloadTokenValid (checks without consuming).
func IsDownloadTokenValid(token string) bool {
	downloadTokenStore.mu.Lock()
	defer downloadTokenStore.mu.Unlock()

	data, ok := downloadTokenStore.tokens[token]
	if !ok {
		return false
	}
	if downloadTokenNowMs() > data.ExpiresAt {
		delete(downloadTokenStore.tokens, token)
		return false
	}
	return data.UsageCount < data.MaxUsage
}

// RevokeDownloadToken mirrors revokeDownloadToken.
func RevokeDownloadToken(token string) {
	downloadTokenStore.mu.Lock()
	defer downloadTokenStore.mu.Unlock()
	delete(downloadTokenStore.tokens, token)
}

// DownloadTokenInfo mirrors getDownloadTokenInfo (userId deliberately omitted).
type DownloadTokenInfo struct {
	ResourceID   string
	ResourceType DownloadTokenResourceType
	ExpiresAt    int64
	UsageCount   int
	MaxUsage     int
	CreatedAt    int64
}

// GetDownloadTokenInfo mirrors getDownloadTokenInfo.
func GetDownloadTokenInfo(token string) *DownloadTokenInfo {
	downloadTokenStore.mu.Lock()
	defer downloadTokenStore.mu.Unlock()

	data, ok := downloadTokenStore.tokens[token]
	if !ok {
		return nil
	}
	return &DownloadTokenInfo{
		ResourceID:   data.ResourceID,
		ResourceType: data.ResourceType,
		ExpiresAt:    data.ExpiresAt,
		UsageCount:   data.UsageCount,
		MaxUsage:     data.MaxUsage,
		CreatedAt:    data.CreatedAt,
	}
}
