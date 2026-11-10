// Port of server/src/db/oauth.ts — the *user login provider* tables
// (`oauth_configs`, `user_oauth_bindings`). This is intentionally NOT
// db/oauth_apps.go / db/oauth_provider.go: those implement the separate
// "PayIncus as an OAuth provider" registry, while this file is the "PayIncus as an
// OAuth client of GitHub/Google" side used by routes/oauth.go.
//
// Contract notes carried over from the TypeScript:
//   - a missed lookup returns (nil, nil), not an error (Prisma findUnique → null);
//   - `client_secret` and the provider access/refresh tokens are stored encrypted by
//     the *route* layer (Prisma has no column-level encryption either: db/oauth.ts
//     calls lib/security's encryptSensitiveData before writing). db cannot import
//     lib (lib imports db), so this module only sees and returns ciphertext.
//   - writes stamp updated_at themselves: the column is NOT NULL with no database
//     default, and Prisma's @updatedAt fills it client-side.
//   - every timestamp that reaches SQL is `.UTC()` (WAVE_D_SPEC §0.2b).
//   - `provider` is a PostgreSQL enum ("OAuthProvider"). Reading or writing an
//     unknown provider therefore raises a database error, which mirrors Prisma's
//     client-side enum validation (both surface as a 500); it never silently
//     degrades to "no rows".
//
// Naming: the db package is flat and shared by parallel ports, so everything is
// prefixed OAuthLogin / oauthLogin.
package db

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// OAuthLoginConfigRecord is one oauth_configs row. ClientSecret is the value as
// stored (AES-GCM ciphertext for rows written by either backend); decrypt with
// lib.DecryptSensitiveData before using or masking it.
type OAuthLoginConfigRecord struct {
	ID           int32
	Provider     string
	ClientID     string
	ClientSecret string
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// OAuthLoginBindingRecord is one user_oauth_bindings row. AccessToken/RefreshToken
// are stored ciphertext.
type OAuthLoginBindingRecord struct {
	ID               int32
	UserID           int32
	Provider         string
	ProviderUserID   string
	ProviderUsername *string
	ProviderEmail    *string
	ProviderAvatar   *string
	AccessToken      *string
	RefreshToken     *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// OAuthLoginBindingInput mirrors the `data` argument of createOAuthBinding.
// AccessToken/RefreshToken are expected to be ciphertext already.
type OAuthLoginBindingInput struct {
	Provider       string
	ProviderUserID string
	// Nil maps to NULL.
	ProviderUsername *string
	ProviderEmail    *string
	ProviderAvatar   *string
	AccessToken      *string
	RefreshToken     *string
}

const oauthLoginConfigSelectColumns = `id, provider, client_id, client_secret, enabled,
	created_at, updated_at`

const oauthLoginBindingSelectColumns = `id, user_id, provider, provider_user_id,
	provider_username, provider_email, provider_avatar, access_token, refresh_token,
	created_at, updated_at`

func oauthLoginRequireDB() error {
	if DB == nil {
		return errors.New("database not initialized")
	}
	return nil
}

func oauthLoginScanConfig(scan func(...any) error) (OAuthLoginConfigRecord, error) {
	var row OAuthLoginConfigRecord
	err := scan(&row.ID, &row.Provider, &row.ClientID, &row.ClientSecret, &row.Enabled,
		&row.CreatedAt, &row.UpdatedAt)
	return row, err
}

func oauthLoginScanBinding(scan func(...any) error) (OAuthLoginBindingRecord, error) {
	var row OAuthLoginBindingRecord
	err := scan(&row.ID, &row.UserID, &row.Provider, &row.ProviderUserID,
		&row.ProviderUsername, &row.ProviderEmail, &row.ProviderAvatar,
		&row.AccessToken, &row.RefreshToken, &row.CreatedAt, &row.UpdatedAt)
	return row, err
}

// ==================== oauth_configs ====================

// OAuthLoginConfigList mirrors getOAuthConfigs(): every config ordered by provider.
func OAuthLoginConfigList(ctx context.Context) ([]OAuthLoginConfigRecord, error) {
	if err := oauthLoginRequireDB(); err != nil {
		return nil, err
	}
	rows, err := DB.WithContext(ctx).Raw(
		"SELECT " + oauthLoginConfigSelectColumns + " FROM oauth_configs ORDER BY provider ASC",
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	configs := make([]OAuthLoginConfigRecord, 0, 2)
	for rows.Next() {
		row, err := oauthLoginScanConfig(rows.Scan)
		if err != nil {
			return nil, err
		}
		configs = append(configs, row)
	}
	return configs, rows.Err()
}

// OAuthLoginConfigGet mirrors getOAuthConfig(provider): (nil, nil) when absent.
//
// The `::"OAuthProvider"` cast is deliberate: an unknown provider makes PostgreSQL
// reject the query, exactly like Prisma's enum validation, instead of answering
// "no row" (which would turn a callback for an unknown provider into
// provider_disabled rather than the TypeScript's oauth_error).
func OAuthLoginConfigGet(ctx context.Context, provider string) (*OAuthLoginConfigRecord, error) {
	if err := oauthLoginRequireDB(); err != nil {
		return nil, err
	}
	row, err := oauthLoginScanConfig(func(dest ...any) error {
		return DB.WithContext(ctx).Raw(
			"SELECT "+oauthLoginConfigSelectColumns+
				` FROM oauth_configs WHERE provider = ?::"OAuthProvider" LIMIT 1`, provider,
		).Row().Scan(dest...)
	})
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// OAuthLoginEnabledProviders mirrors getEnabledOAuthConfigs(). Only the provider
// column is read (the route only maps it to a string list), and no ORDER BY is
// added because the TypeScript has none either.
func OAuthLoginEnabledProviders(ctx context.Context) ([]string, error) {
	if err := oauthLoginRequireDB(); err != nil {
		return nil, err
	}
	rows, err := DB.WithContext(ctx).Raw(
		"SELECT provider FROM oauth_configs WHERE enabled = true",
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	providers := make([]string, 0, 2)
	for rows.Next() {
		var provider string
		if err := rows.Scan(&provider); err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}

// OAuthLoginConfigUpsert mirrors upsertOAuthConfig(provider, data). clientSecret
// must already be encrypted by the caller (see the file header). Returns the row id.
func OAuthLoginConfigUpsert(ctx context.Context, provider, clientID, clientSecret string, enabled bool) (int32, error) {
	if err := oauthLoginRequireDB(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var id int32
	err := DB.WithContext(ctx).Raw(
		`INSERT INTO oauth_configs (provider, client_id, client_secret, enabled, created_at, updated_at)
		 VALUES (?::"OAuthProvider", ?, ?, ?, ?, ?)
		 ON CONFLICT (provider) DO UPDATE
		   SET client_id = EXCLUDED.client_id,
		       client_secret = EXCLUDED.client_secret,
		       enabled = EXCLUDED.enabled,
		       updated_at = EXCLUDED.updated_at
		 RETURNING id`,
		provider, clientID, clientSecret, enabled, now, now,
	).Scan(&id).Error
	return id, err
}

// OAuthLoginConfigDelete mirrors deleteOAuthConfig(provider). A missing row is
// Prisma's P2025 (gorm.ErrRecordNotFound here), which the route answers with 500
// exactly like Node does.
func OAuthLoginConfigDelete(ctx context.Context, provider string) error {
	if err := oauthLoginRequireDB(); err != nil {
		return err
	}
	result := DB.WithContext(ctx).Exec(
		`DELETE FROM oauth_configs WHERE provider = ?::"OAuthProvider"`, provider,
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ==================== user_oauth_bindings ====================

// OAuthLoginBindingListByUser mirrors getUserOAuthBindings(userId): provider order.
func OAuthLoginBindingListByUser(ctx context.Context, userID int32) ([]OAuthLoginBindingRecord, error) {
	if err := oauthLoginRequireDB(); err != nil {
		return nil, err
	}
	rows, err := DB.WithContext(ctx).Raw(
		"SELECT "+oauthLoginBindingSelectColumns+
			" FROM user_oauth_bindings WHERE user_id = ? ORDER BY provider ASC", userID,
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bindings := make([]OAuthLoginBindingRecord, 0, 2)
	for rows.Next() {
		row, err := oauthLoginScanBinding(rows.Scan)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, row)
	}
	return bindings, rows.Err()
}

// OAuthLoginBindingFind mirrors findOAuthBinding(provider, providerUserId) — the
// @@unique([provider, providerUserId]) lookup. (nil, nil) when absent.
func OAuthLoginBindingFind(ctx context.Context, provider, providerUserID string) (*OAuthLoginBindingRecord, error) {
	if err := oauthLoginRequireDB(); err != nil {
		return nil, err
	}
	row, err := oauthLoginScanBinding(func(dest ...any) error {
		return DB.WithContext(ctx).Raw(
			"SELECT "+oauthLoginBindingSelectColumns+
				` FROM user_oauth_bindings WHERE provider = ?::"OAuthProvider" AND provider_user_id = ? LIMIT 1`,
			provider, providerUserID,
		).Row().Scan(dest...)
	})
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// OAuthLoginBindingCreate mirrors createOAuthBinding(userId, data).
func OAuthLoginBindingCreate(ctx context.Context, userID int32, input OAuthLoginBindingInput) (int32, error) {
	if err := oauthLoginRequireDB(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var id int32
	err := DB.WithContext(ctx).Raw(
		`INSERT INTO user_oauth_bindings
			(user_id, provider, provider_user_id, provider_username, provider_email,
			 provider_avatar, access_token, refresh_token, created_at, updated_at)
		 VALUES (?, ?::"OAuthProvider", ?, ?, ?, ?, ?, ?, ?, ?)
		 RETURNING id`,
		userID, input.Provider, input.ProviderUserID, input.ProviderUsername,
		input.ProviderEmail, input.ProviderAvatar, input.AccessToken, input.RefreshToken,
		now, now,
	).Scan(&id).Error
	return id, err
}

// OAuthLoginBindingUpdateToken mirrors updateOAuthBindingToken(userId, provider,
// accessToken, refreshToken).
//
// Prisma's `refreshToken: refreshToken ? enc(...) : undefined` means "leave the
// stored refresh token untouched" when the caller passes null — so the Go signature
// treats a nil pointer as "do not touch" rather than "set NULL". A missing row is
// P2025 (500), matching Node.
func OAuthLoginBindingUpdateToken(ctx context.Context, userID int32, provider, accessToken string, refreshToken *string) error {
	if err := oauthLoginRequireDB(); err != nil {
		return err
	}
	now := time.Now().UTC()
	var result *gorm.DB
	if refreshToken != nil {
		result = DB.WithContext(ctx).Exec(
			`UPDATE user_oauth_bindings
			 SET access_token = ?, refresh_token = ?, updated_at = ?
			 WHERE user_id = ? AND provider = ?::"OAuthProvider"`,
			accessToken, *refreshToken, now, userID, provider,
		)
	} else {
		result = DB.WithContext(ctx).Exec(
			`UPDATE user_oauth_bindings
			 SET access_token = ?, updated_at = ?
			 WHERE user_id = ? AND provider = ?::"OAuthProvider"`,
			accessToken, now, userID, provider,
		)
	}
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
