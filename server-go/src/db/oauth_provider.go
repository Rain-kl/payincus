// Port of server/src/lib/oauth-provider.ts (the data/авторизация layer behind
// routes/oauth-provider.ts).
//
// The module lives in db/ rather than lib/ because every function here talks to
// PostgreSQL and lib already imports db (lib/public_api_auth.go reads tokens), so a
// lib implementation would create an import cycle. Naming follows the same rule as
// db/gpus.go: every package-level identifier is `OAuthProvider`/`oauthProvider`
// prefixed so parallel ports never collide.
//
// Security-relevant invariants, all identical to the TypeScript:
//   - client secrets and every token are stored ONLY as sha256 hex digests; the raw
//     value exists only in the HTTP response;
//   - the client-secret comparison is constant time (OAuthAppsVerifyClientSecret);
//   - an authorization code is single use (used_at is stamped inside the exchange
//     transaction) and expires after 600 s;
//   - a refresh token is rotated (old row revoked_at + last_used_at) on every
//     exchange, and the user must still be active and the grant unrevoked;
//   - revoking a grant also revokes every access and refresh token of that grant.
//
// Reused from db/oauth_apps.go: secret hashing, URL/scopes normalization, the scope
// catalog and the timing-safe secret comparison — they are the same TypeScript
// helpers (normalizeOAuthRedirectUris / normalizeOAuthScopes / hashOAuthSecret).
package db

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// OAuth provider token prefixes and TTLs, mirroring the TypeScript constants.
const (
	OAuthProviderClientIDPrefix           = "pocli_"
	OAuthProviderClientSecretPrefix       = "posec_"
	OAuthProviderAuthorizationCodePrefix  = "poc_"
	OAuthProviderAccessTokenPrefix        = "poa_"
	OAuthProviderRefreshTokenPrefix       = "por_"
	OAuthProviderAccessTokenPrefixLength  = 16
	OAuthProviderRefreshTokenPrefixLength = 16
	OAuthProviderAuthorizationCodeTTL     = 10 * 60
	OAuthProviderAccessTokenTTL           = 60 * 60
	OAuthProviderRefreshTokenTTL          = 30 * 24 * 60 * 60
)

// OAuthProviderAppRef is the trimmed app projection embedded in a grant.
type OAuthProviderAppRef struct {
	ID       int32  `json:"id"`
	Name     string `json:"name"`
	ClientID string `json:"clientId"`
	Enabled  bool   `json:"enabled"`
}

// OAuthProviderAuthorization mirrors SerializedOAuthAuthorization.
type OAuthProviderAuthorization struct {
	ID               int32               `json:"id"`
	App              OAuthProviderAppRef `json:"app"`
	Scopes           []string            `json:"scopes"`
	Active           bool                `json:"active"`
	LastAuthorizedAt string              `json:"lastAuthorizedAt"`
	RevokedAt        *string             `json:"revokedAt"`
	CreatedAt        string              `json:"createdAt"`
	UpdatedAt        string              `json:"updatedAt"`
}

// OAuthProviderApp is the app row prepareOAuthAuthorizationRequest needs.
type OAuthProviderApp struct {
	ID               int32
	Name             string
	ClientID         string
	ClientSecretHash string
	Enabled          bool
	RedirectURIs     datatypes.JSON
	Scopes           datatypes.JSON
}

// OAuthProviderGrant is the grant row (serialized scopes are decoded lazily).
type OAuthProviderGrant struct {
	ID               int32
	AppID            int32
	UserID           int32
	Scopes           datatypes.JSON
	LastAuthorizedAt time.Time
	RevokedAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// OAuthProviderPrepared mirrors prepareOAuthAuthorizationRequest's return value.
type OAuthProviderPrepared struct {
	App             OAuthProviderApp
	RequestedScopes []string
	ExistingGrant   *OAuthProviderGrant
	ConsentRequired bool
}

// OAuthProviderAuthorizationResult mirrors createOAuthAuthorizationCode's result.
type OAuthProviderAuthorizationResult struct {
	RedirectTo string `json:"redirectTo"`
	ExpiresIn  int    `json:"expiresIn"`
}

// OAuthProviderAccessTokenRecord is the inserted access-token row (the route only
// needs UserID, but Scopes/ID keep the shape usable).
type OAuthProviderAccessTokenRecord struct {
	ID     int32
	UserID int32
	Scopes []string
}

// OAuthProviderTokenResult mirrors the token exchange response.
type OAuthProviderTokenResult struct {
	AccessToken       string
	RefreshToken      string
	TokenType         string
	ExpiresIn         int
	RefreshExpiresIn  int
	Scope             string
	AccessTokenRecord OAuthProviderAccessTokenRecord
}

// OAuthProviderCodeExchangeInput mirrors exchangeOAuthAuthorizationCode's input.
type OAuthProviderCodeExchangeInput struct {
	ClientID     string
	ClientSecret string
	Code         string
	RedirectURI  string
}

// OAuthProviderRefreshExchangeInput mirrors exchangeOAuthRefreshToken's input.
type OAuthProviderRefreshExchangeInput struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
}

// OAuthProviderAuthorizationRequestInput mirrors the prepared request input.
type OAuthProviderAuthorizationRequestInput struct {
	ClientID    string
	RedirectURI string
	Scopes      []string
	UserID      int32
}

// OAuthProviderCreateCodeInput mirrors createOAuthAuthorizationCode's input.
type OAuthProviderCreateCodeInput struct {
	ClientID    string
	RedirectURI string
	Scopes      []string
	UserID      int32
	State       *string
}

// ==================== pure helpers ====================

// OAuthProviderHashSecret mirrors hashOAuthSecret.
func OAuthProviderHashSecret(value string) string { return OAuthAppsHashSecret(value) }

// oauthProviderGenerateToken mirrors the randomBytes(...).toString('base64url')
// token generators.
func oauthProviderGenerateToken(prefix string, randomBytesLength int) (string, error) {
	buffer := make([]byte, randomBytesLength)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// OAuthProviderAccessTokenPrefixValue mirrors getOAuthAccessTokenPrefix: the raw
// token is stored only as sha256, this prefix is for display/diagnostics.
func OAuthProviderAccessTokenPrefixValue(token string) string {
	if len(token) <= OAuthProviderAccessTokenPrefixLength {
		return token
	}
	return token[:OAuthProviderAccessTokenPrefixLength]
}

// OAuthProviderRefreshTokenPrefixValue mirrors getOAuthRefreshTokenPrefix.
func OAuthProviderRefreshTokenPrefixValue(token string) string {
	if len(token) <= OAuthProviderRefreshTokenPrefixLength {
		return token
	}
	return token[:OAuthProviderRefreshTokenPrefixLength]
}

// OAuthProviderSerializeScopes mirrors serializeScopes: invalid stored JSON
// degrades to an empty array, never null.
func OAuthProviderSerializeScopes(raw datatypes.JSON) []string {
	return OAuthAppsSerializeScopes([]byte(raw))
}

// OAuthProviderSerializeAuthorization mirrors serializeOAuthAuthorization.
func OAuthProviderSerializeAuthorization(grant OAuthProviderGrant, app OAuthProviderAppRef) OAuthProviderAuthorization {
	var revokedAt *string
	if grant.RevokedAt != nil {
		formatted := oauthProviderISOTime(*grant.RevokedAt)
		revokedAt = &formatted
	}
	return OAuthProviderAuthorization{
		ID:               grant.ID,
		App:              app,
		Scopes:           OAuthProviderSerializeScopes(grant.Scopes),
		Active:           grant.RevokedAt == nil && app.Enabled,
		LastAuthorizedAt: oauthProviderISOTime(grant.LastAuthorizedAt),
		RevokedAt:        revokedAt,
		CreatedAt:        oauthProviderISOTime(grant.CreatedAt),
		UpdatedAt:        oauthProviderISOTime(grant.UpdatedAt),
	}
}

// oauthProviderISOTime mirrors Date.prototype.toISOString().
func oauthProviderISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// OAuthProviderReadRequestBody mirrors readOAuthProviderRequestBody.
func OAuthProviderReadRequestBody(body any) map[string]any {
	if record, ok := body.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

// ==================== query helpers ====================

func oauthProviderJSONValue(raw datatypes.JSON) any {
	if len(raw) == 0 {
		return nil
	}
	return jsonAny(raw)
}

// oauthProviderAppByClientID mirrors prisma.oAuthClientApp.findUnique({clientId}).
func oauthProviderAppByClientID(ctx context.Context, client *gorm.DB, clientID string) (*OAuthProviderApp, error) {
	if client == nil {
		client = DB
	}
	var row models.OAuthClientApp
	err := client.WithContext(ctx).Where("client_id = ?", clientID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &OAuthProviderApp{
		ID:               row.ID,
		Name:             row.Name,
		ClientID:         row.ClientID,
		ClientSecretHash: row.ClientSecretHash,
		Enabled:          row.Enabled,
		RedirectURIs:     row.RedirectUris,
		Scopes:           row.Scopes,
	}, nil
}

// oauthProviderAppRefByID loads the trimmed app projection.
func oauthProviderAppRefByID(ctx context.Context, client *gorm.DB, appID int32) (OAuthProviderAppRef, error) {
	var row models.OAuthClientApp
	err := client.WithContext(ctx).Select("id", "name", "client_id", "enabled").
		Where("id = ?", appID).First(&row).Error
	if err != nil {
		return OAuthProviderAppRef{}, err
	}
	return OAuthProviderAppRef{ID: row.ID, Name: row.Name, ClientID: row.ClientID, Enabled: row.Enabled}, nil
}

// oauthProviderGrantByAppUser mirrors findUnique({appId_userId}).
func oauthProviderGrantByAppUser(ctx context.Context, client *gorm.DB, appID, userID int32) (*OAuthProviderGrant, error) {
	var row models.OAuthGrant
	err := client.WithContext(ctx).Where("app_id = ? AND user_id = ?", appID, userID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &OAuthProviderGrant{
		ID: row.ID, AppID: row.AppID, UserID: row.UserID, Scopes: row.Scopes,
		LastAuthorizedAt: row.LastAuthorizedAt, RevokedAt: row.RevokedAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// ==================== authorization request ====================

// OAuthProviderPrepareAuthorizationRequest mirrors prepareOAuthAuthorizationRequest.
func OAuthProviderPrepareAuthorizationRequest(ctx context.Context, input OAuthProviderAuthorizationRequestInput) (OAuthProviderPrepared, error) {
	app, err := oauthProviderAppByClientID(ctx, nil, input.ClientID)
	if err != nil {
		return OAuthProviderPrepared{}, err
	}
	if app == nil || !app.Enabled {
		return OAuthProviderPrepared{}, errors.New("OAuth app is disabled or missing")
	}

	redirectUris, ok := OAuthAppsNormalizeRedirectURIs(oauthProviderJSONValue(app.RedirectURIs))
	if !ok {
		return OAuthProviderPrepared{}, errors.New("redirect_uri is not registered for this OAuth app")
	}
	registered := false
	for _, candidate := range redirectUris {
		if candidate == input.RedirectURI {
			registered = true
			break
		}
	}
	if !registered {
		return OAuthProviderPrepared{}, errors.New("redirect_uri is not registered for this OAuth app")
	}

	appScopes := map[string]bool{}
	for _, scope := range OAuthAppsSerializeScopes([]byte(app.Scopes)) {
		appScopes[scope] = true
	}
	for _, scope := range input.Scopes {
		if !appScopes[scope] {
			return OAuthProviderPrepared{}, errors.New("Requested scope exceeds OAuth app grant")
		}
	}

	grant, err := oauthProviderGrantByAppUser(ctx, nil, app.ID, input.UserID)
	if err != nil {
		return OAuthProviderPrepared{}, err
	}
	grantedScopes := map[string]bool{}
	if grant != nil && grant.RevokedAt == nil {
		for _, scope := range OAuthProviderSerializeScopes(grant.Scopes) {
			grantedScopes[scope] = true
		}
	}
	consentRequired := false
	for _, scope := range input.Scopes {
		if !grantedScopes[scope] {
			consentRequired = true
			break
		}
	}

	return OAuthProviderPrepared{
		App:             *app,
		RequestedScopes: input.Scopes,
		ExistingGrant:   grant,
		ConsentRequired: consentRequired,
	}, nil
}

// ==================== authorization code ====================

// OAuthProviderCreateAuthorizationCode mirrors createOAuthAuthorizationCode.
func OAuthProviderCreateAuthorizationCode(ctx context.Context, input OAuthProviderCreateCodeInput) (OAuthProviderAuthorizationResult, error) {
	prepared, err := OAuthProviderPrepareAuthorizationRequest(ctx, OAuthProviderAuthorizationRequestInput{
		ClientID:    input.ClientID,
		RedirectURI: input.RedirectURI,
		Scopes:      input.Scopes,
		UserID:      input.UserID,
	})
	if err != nil {
		return OAuthProviderAuthorizationResult{}, err
	}

	code, err := oauthProviderGenerateToken(OAuthProviderAuthorizationCodePrefix, 32)
	if err != nil {
		return OAuthProviderAuthorizationResult{}, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(OAuthProviderAuthorizationCodeTTL * time.Second)
	scopesJSON, err := jsonMarshalStrings(input.Scopes)
	if err != nil {
		return OAuthProviderAuthorizationResult{}, err
	}

	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		grant := models.OAuthGrant{
			AppID:            prepared.App.ID,
			UserID:           input.UserID,
			Scopes:           datatypes.JSON(scopesJSON),
			LastAuthorizedAt: now,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "app_id"}, {Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"scopes":             datatypes.JSON(scopesJSON),
				"revoked_at":         nil,
				"last_authorized_at": now,
				"updated_at":         now,
			}),
		}).Create(&grant).Error; err != nil {
			return err
		}

		authorizationCode := models.OAuthAuthorizationCode{
			AppID:       prepared.App.ID,
			UserID:      input.UserID,
			CodeHash:    OAuthProviderHashSecret(code),
			RedirectURI: input.RedirectURI,
			Scopes:      datatypes.JSON(scopesJSON),
			ExpiresAt:   expiresAt,
			CreatedAt:   now,
		}
		return tx.Create(&authorizationCode).Error
	})
	if err != nil {
		return OAuthProviderAuthorizationResult{}, err
	}

	params := [][2]string{{"code", code}}
	if input.State != nil && *input.State != "" {
		params = append(params, [2]string{"state", *input.State})
	}
	redirectTo, err := OAuthProviderSetQueryParams(input.RedirectURI, params)
	if err != nil {
		return OAuthProviderAuthorizationResult{}, err
	}
	return OAuthProviderAuthorizationResult{RedirectTo: redirectTo, ExpiresIn: OAuthProviderAuthorizationCodeTTL}, nil
}

// ==================== token exchange ====================

// oauthProviderVerifyClient mirrors the app+secret check shared by both grants.
func oauthProviderVerifyClient(ctx context.Context, inputClientID, inputSecret string) (*OAuthProviderApp, error) {
	app, err := oauthProviderAppByClientID(ctx, nil, inputClientID)
	if err != nil {
		return nil, err
	}
	if app == nil || !app.Enabled || !OAuthAppsVerifyClientSecret(app.ClientSecretHash, inputSecret) {
		return nil, errors.New("Invalid OAuth client credentials")
	}
	return app, nil
}

// oauthProviderInsertAccessToken creates the access-token row and returns its record.
func oauthProviderInsertAccessToken(tx *gorm.DB, appID, userID, grantID int32, rawToken string, scopes datatypes.JSON, expiresAt, now time.Time) (OAuthProviderAccessTokenRecord, error) {
	row := models.OAuthAccessToken{
		AppID:       appID,
		UserID:      userID,
		GrantID:     &grantID,
		TokenPrefix: OAuthProviderAccessTokenPrefixValue(rawToken),
		TokenHash:   OAuthProviderHashSecret(rawToken),
		Scopes:      scopes,
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := tx.Clauses(clause.Returning{}).Create(&row).Error; err != nil {
		return OAuthProviderAccessTokenRecord{}, err
	}
	return OAuthProviderAccessTokenRecord{ID: row.ID, UserID: row.UserID, Scopes: OAuthProviderSerializeScopes(row.Scopes)}, nil
}

// oauthProviderInsertRefreshToken creates the refresh-token row.
func oauthProviderInsertRefreshToken(tx *gorm.DB, appID, userID, grantID int32, rawToken string, scopes datatypes.JSON, expiresAt, now time.Time) error {
	row := models.OAuthRefreshToken{
		AppID:       appID,
		UserID:      userID,
		GrantID:     &grantID,
		TokenPrefix: OAuthProviderRefreshTokenPrefixValue(rawToken),
		TokenHash:   OAuthProviderHashSecret(rawToken),
		Scopes:      scopes,
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return tx.Create(&row).Error
}

// OAuthProviderExchangeAuthorizationCode mirrors exchangeOAuthAuthorizationCode.
func OAuthProviderExchangeAuthorizationCode(ctx context.Context, input OAuthProviderCodeExchangeInput) (OAuthProviderTokenResult, error) {
	app, err := oauthProviderVerifyClient(ctx, input.ClientID, input.ClientSecret)
	if err != nil {
		return OAuthProviderTokenResult{}, err
	}

	accessTokenRaw, err := oauthProviderGenerateToken(OAuthProviderAccessTokenPrefix, 32)
	if err != nil {
		return OAuthProviderTokenResult{}, err
	}
	refreshTokenRaw, err := oauthProviderGenerateToken(OAuthProviderRefreshTokenPrefix, 32)
	if err != nil {
		return OAuthProviderTokenResult{}, err
	}
	now := time.Now().UTC()
	accessTokenExpiresAt := now.Add(OAuthProviderAccessTokenTTL * time.Second)
	refreshTokenExpiresAt := now.Add(OAuthProviderRefreshTokenTTL * time.Second)
	codeHash := OAuthProviderHashSecret(input.Code)

	var accessRecord OAuthProviderAccessTokenRecord
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var code models.OAuthAuthorizationCode
		err := tx.Where("code_hash = ?", codeHash).First(&code).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New("Invalid authorization code")
		}
		if err != nil {
			return err
		}
		if code.AppID != app.ID {
			return errors.New("Invalid authorization code")
		}
		if code.UsedAt != nil {
			return errors.New("Authorization code has already been used")
		}
		if !code.ExpiresAt.After(time.Now().UTC()) {
			return errors.New("Authorization code has expired")
		}
		if code.RedirectURI != input.RedirectURI {
			return errors.New("redirect_uri does not match authorization request")
		}

		if err := tx.Model(&models.OAuthAuthorizationCode{}).Where("id = ?", code.ID).
			Update("used_at", now).Error; err != nil {
			return err
		}

		grant, err := oauthProviderGrantByAppUser(ctx, tx, app.ID, code.UserID)
		if err != nil {
			return err
		}
		if grant == nil || grant.RevokedAt != nil {
			return errors.New("OAuth authorization has been revoked")
		}

		scopes := code.Scopes
		record, err := oauthProviderInsertAccessToken(tx, app.ID, code.UserID, grant.ID, accessTokenRaw, scopes, accessTokenExpiresAt, now)
		if err != nil {
			return err
		}
		if err := oauthProviderInsertRefreshToken(tx, app.ID, code.UserID, grant.ID, refreshTokenRaw, scopes, refreshTokenExpiresAt, now); err != nil {
			return err
		}
		accessRecord = record
		return nil
	})
	if err != nil {
		return OAuthProviderTokenResult{}, err
	}

	return OAuthProviderTokenResult{
		AccessToken:       accessTokenRaw,
		RefreshToken:      refreshTokenRaw,
		TokenType:         "Bearer",
		ExpiresIn:         OAuthProviderAccessTokenTTL,
		RefreshExpiresIn:  OAuthProviderRefreshTokenTTL,
		Scope:             strings.Join(accessRecord.Scopes, " "),
		AccessTokenRecord: accessRecord,
	}, nil
}

// OAuthProviderExchangeRefreshToken mirrors exchangeOAuthRefreshToken.
func OAuthProviderExchangeRefreshToken(ctx context.Context, input OAuthProviderRefreshExchangeInput) (OAuthProviderTokenResult, error) {
	app, err := oauthProviderVerifyClient(ctx, input.ClientID, input.ClientSecret)
	if err != nil {
		return OAuthProviderTokenResult{}, err
	}

	accessTokenRaw, err := oauthProviderGenerateToken(OAuthProviderAccessTokenPrefix, 32)
	if err != nil {
		return OAuthProviderTokenResult{}, err
	}
	nextRefreshTokenRaw, err := oauthProviderGenerateToken(OAuthProviderRefreshTokenPrefix, 32)
	if err != nil {
		return OAuthProviderTokenResult{}, err
	}
	now := time.Now().UTC()
	accessTokenExpiresAt := now.Add(OAuthProviderAccessTokenTTL * time.Second)
	refreshTokenExpiresAt := now.Add(OAuthProviderRefreshTokenTTL * time.Second)
	refreshTokenHash := OAuthProviderHashSecret(input.RefreshToken)

	var accessRecord OAuthProviderAccessTokenRecord
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var refresh models.OAuthRefreshToken
		err := tx.Where("token_hash = ?", refreshTokenHash).First(&refresh).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New("Invalid refresh token")
		}
		if err != nil {
			return err
		}
		if refresh.AppID != app.ID {
			return errors.New("Invalid refresh token")
		}
		if refresh.RevokedAt != nil {
			return errors.New("Refresh token has been revoked")
		}
		if !refresh.ExpiresAt.After(time.Now().UTC()) {
			return errors.New("Refresh token has expired")
		}

		var user models.User
		if err := tx.Select("id", "status").Where("id = ?", refresh.UserID).First(&user).Error; err != nil {
			return err
		}
		if string(user.Status) != "active" {
			return errors.New("Account is not active")
		}
		if refresh.GrantID == nil {
			return errors.New("OAuth authorization has been revoked")
		}
		var grant models.OAuthGrant
		err = tx.Select("id", "revoked_at").Where("id = ?", *refresh.GrantID).First(&grant).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New("OAuth authorization has been revoked")
		}
		if err != nil {
			return err
		}
		if grant.RevokedAt != nil {
			return errors.New("OAuth authorization has been revoked")
		}

		if err := tx.Model(&models.OAuthRefreshToken{}).Where("id = ?", refresh.ID).
			Updates(map[string]any{"revoked_at": now, "last_used_at": now, "updated_at": now}).Error; err != nil {
			return err
		}

		scopes := refresh.Scopes
		record, err := oauthProviderInsertAccessToken(tx, app.ID, refresh.UserID, grant.ID, accessTokenRaw, scopes, accessTokenExpiresAt, now)
		if err != nil {
			return err
		}
		if err := oauthProviderInsertRefreshToken(tx, app.ID, refresh.UserID, grant.ID, nextRefreshTokenRaw, scopes, refreshTokenExpiresAt, now); err != nil {
			return err
		}
		accessRecord = record
		return nil
	})
	if err != nil {
		return OAuthProviderTokenResult{}, err
	}

	return OAuthProviderTokenResult{
		AccessToken:       accessTokenRaw,
		RefreshToken:      nextRefreshTokenRaw,
		TokenType:         "Bearer",
		ExpiresIn:         OAuthProviderAccessTokenTTL,
		RefreshExpiresIn:  OAuthProviderRefreshTokenTTL,
		Scope:             strings.Join(accessRecord.Scopes, " "),
		AccessTokenRecord: accessRecord,
	}, nil
}

// ==================== revocation / listing ====================

// oauthProviderRevokeAccessToken mirrors revokeOAuthAccessToken.
func oauthProviderRevokeAccessToken(ctx context.Context, rawToken string) (*OAuthProviderAccessTokenRecord, error) {
	hash := OAuthProviderHashSecret(rawToken)
	var token models.OAuthAccessToken
	err := DB.WithContext(ctx).Where("token_hash = ?", hash).First(&token).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record := &OAuthProviderAccessTokenRecord{ID: token.ID, UserID: token.UserID, Scopes: OAuthProviderSerializeScopes(token.Scopes)}
	if token.RevokedAt != nil {
		return record, nil
	}
	if err := DB.WithContext(ctx).Model(&models.OAuthAccessToken{}).Where("id = ?", token.ID).
		Update("revoked_at", time.Now().UTC()).Error; err != nil {
		return nil, err
	}
	return record, nil
}

// OAuthProviderRevokeToken mirrors revokeOAuthToken: an access token wins, then a
// refresh token; nil means the raw value matched nothing.
func OAuthProviderRevokeToken(ctx context.Context, rawToken string) (*int32, error) {
	access, err := oauthProviderRevokeAccessToken(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	if access != nil {
		return &access.UserID, nil
	}

	hash := OAuthProviderHashSecret(rawToken)
	var refresh models.OAuthRefreshToken
	err = DB.WithContext(ctx).Where("token_hash = ?", hash).First(&refresh).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if refresh.RevokedAt == nil {
		if err := DB.WithContext(ctx).Model(&models.OAuthRefreshToken{}).Where("id = ?", refresh.ID).
			Update("revoked_at", time.Now().UTC()).Error; err != nil {
			return nil, err
		}
	}
	return &refresh.UserID, nil
}

// OAuthProviderListAuthorizations mirrors listOAuthAuthorizations.
func OAuthProviderListAuthorizations(ctx context.Context, userID int32) ([]OAuthProviderAuthorization, error) {
	var grants []models.OAuthGrant
	if err := DB.WithContext(ctx).Where("user_id = ?", userID).
		Order("last_authorized_at DESC").Find(&grants).Error; err != nil {
		return nil, err
	}

	authorizations := make([]OAuthProviderAuthorization, 0, len(grants))
	for _, row := range grants {
		app, err := oauthProviderAppRefByID(ctx, nil, row.AppID)
		if err != nil {
			return nil, err
		}
		authorizations = append(authorizations, OAuthProviderSerializeAuthorization(OAuthProviderGrant{
			ID: row.ID, AppID: row.AppID, UserID: row.UserID, Scopes: row.Scopes,
			LastAuthorizedAt: row.LastAuthorizedAt, RevokedAt: row.RevokedAt,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}, app))
	}
	return authorizations, nil
}

// OAuthProviderRevokeAuthorization mirrors revokeOAuthAuthorization: revoke the
// grant and every token that belongs to it, returning the revoked grant.
func OAuthProviderRevokeAuthorization(ctx context.Context, userID, grantID int32) (*OAuthProviderAuthorization, error) {
	var result *OAuthProviderAuthorization
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var grant models.OAuthGrant
		err := tx.Where("id = ? AND user_id = ?", grantID, userID).First(&grant).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		app, err := oauthProviderAppRefByID(ctx, tx, grant.AppID)
		if err != nil {
			return err
		}

		revokedAt := time.Now().UTC()
		if err := tx.Model(&models.OAuthGrant{}).Where("id = ?", grant.ID).
			Updates(map[string]any{"revoked_at": revokedAt, "updated_at": revokedAt}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.OAuthAccessToken{}).
			Where("grant_id = ? AND revoked_at IS NULL", grant.ID).
			Update("revoked_at", revokedAt).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.OAuthRefreshToken{}).
			Where("grant_id = ? AND revoked_at IS NULL", grant.ID).
			Update("revoked_at", revokedAt).Error; err != nil {
			return err
		}

		grant.RevokedAt = &revokedAt
		grant.UpdatedAt = revokedAt
		authorization := OAuthProviderSerializeAuthorization(OAuthProviderGrant{
			ID: grant.ID, AppID: grant.AppID, UserID: grant.UserID, Scopes: grant.Scopes,
			LastAuthorizedAt: grant.LastAuthorizedAt, RevokedAt: grant.RevokedAt,
			CreatedAt: grant.CreatedAt, UpdatedAt: grant.UpdatedAt,
		}, app)
		result = &authorization
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ==================== URL query manipulation (WHATWG URLSearchParams) ====================

// oauthProviderQueryParam keeps query parameters in insertion order, which is how
// URLSearchParams serializes them. Go's url.Values.Encode() sorts keys, so it cannot
// be used without changing the redirect string.
type oauthProviderQueryParam struct {
	key   string
	value string
}

// oauthProviderQuery is an ordered parameter list.
type oauthProviderQuery []oauthProviderQueryParam

// oauthProviderIsFormSafe reports the application/x-www-form-urlencoded percent-set
// complement: alphanumerics plus `*`, `-`, `.`, `_`.
func oauthProviderIsFormSafe(value byte) bool {
	switch {
	case value >= 'a' && value <= 'z', value >= 'A' && value <= 'Z', value >= '0' && value <= '9':
		return true
	case value == '*', value == '-', value == '.', value == '_':
		return true
	}
	return false
}

// oauthProviderFormEncode mirrors URLSearchParams serialization (space becomes `+`).
func oauthProviderFormEncode(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		current := value[index]
		switch {
		case current == ' ':
			builder.WriteByte('+')
		case oauthProviderIsFormSafe(current):
			builder.WriteByte(current)
		default:
			builder.WriteByte('%')
			builder.WriteString(strings.ToUpper(hex.EncodeToString([]byte{current})))
		}
	}
	return builder.String()
}

func oauthProviderHexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	}
	return 0, false
}

// oauthProviderFormDecode decodes a query component. Invalid percent sequences are
// kept verbatim, matching URLSearchParams' lenient parsing.
func oauthProviderFormDecode(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		current := value[index]
		if current == '+' {
			builder.WriteByte(' ')
			continue
		}
		if current == '%' && index+2 < len(value) {
			high, highOK := oauthProviderHexValue(value[index+1])
			low, lowOK := oauthProviderHexValue(value[index+2])
			if highOK && lowOK {
				builder.WriteByte(high<<4 | low)
				index += 2
				continue
			}
		}
		builder.WriteByte(current)
	}
	return builder.String()
}

// oauthProviderParseQuery mirrors `new URLSearchParams(query)`.
func oauthProviderParseQuery(raw string) oauthProviderQuery {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "&")
	params := make(oauthProviderQuery, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		key := part
		value := ""
		if separator := strings.IndexByte(part, '='); separator >= 0 {
			key = part[:separator]
			value = part[separator+1:]
		}
		params = append(params, oauthProviderQueryParam{key: oauthProviderFormDecode(key), value: oauthProviderFormDecode(value)})
	}
	return params
}

// set mirrors URLSearchParams.prototype.set: replace in place, drop duplicates,
// append when absent.
func (params *oauthProviderQuery) set(key, value string) {
	current := *params
	next := make([]oauthProviderQueryParam, 0, len(current)+1)
	replaced := false
	for _, param := range current {
		if param.key != key {
			next = append(next, param)
			continue
		}
		if !replaced {
			next = append(next, oauthProviderQueryParam{key: key, value: value})
			replaced = true
		}
	}
	if !replaced {
		next = append(next, oauthProviderQueryParam{key: key, value: value})
	}
	*params = next
}

// OAuthProviderSetQueryParams mirrors
// `const u = new URL(uri); u.searchParams.set(k, v)...; u.toString()`.
func OAuthProviderSetQueryParams(rawURL string, params [][2]string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" {
		return "", errors.New("Invalid URL")
	}
	query := oauthProviderParseQuery(parsed.RawQuery)
	for _, param := range params {
		query.set(param[0], param[1])
	}

	parts := make([]string, 0, len(query))
	for _, param := range query {
		parts = append(parts, oauthProviderFormEncode(param.key)+"="+oauthProviderFormEncode(param.value))
	}
	parsed.RawQuery = strings.Join(parts, "&")
	return parsed.String(), nil
}

// ==================== small shared helpers ====================

// jsonAny decodes a JSON document into a generic value.
func jsonAny(raw datatypes.JSON) any {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	return decoded
}

// jsonMarshalStrings marshals a scope/redirect URI list for a JSON column.
func jsonMarshalStrings(values []string) ([]byte, error) {
	if values == nil {
		values = []string{}
	}
	return json.Marshal(values)
}
