// Access-token JWT handling, matching fastify-jwt defaults (HS256, JWT_SECRET).
package lib

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessTokenTTL matches `{ expiresIn: '7d' }` used by the auth routes.
const AccessTokenTTL = 7 * 24 * time.Hour

// AccessTokenClaims is the payload signed by fastify.jwt.sign in the Node backend.
type AccessTokenClaims struct {
	ID       int32  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Status   string `json:"status"`
	SID      string `json:"sid,omitempty"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

// GetJwtSigningSecret mirrors getJwtSigningSecret().
func GetJwtSigningSecret(purpose string) (string, error) {
	if secret := os.Getenv("JWT_SECRET"); secret != "" {
		return secret, nil
	}
	if IsProductionEnv() {
		return "", fmt.Errorf("JWT_SECRET is required in production for %s", purpose)
	}
	return "dev-secret-change-in-production", nil
}

// SignAccessToken issues an access token with the same claims as the Node backend.
func SignAccessToken(claims AccessTokenClaims) (string, error) {
	secret, err := GetJwtSigningSecret("token signing")
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims.IssuedAt = now.Unix()
	claims.Expires = now.Add(AccessTokenTTL).Unix()

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"id":       claims.ID,
		"username": claims.Username,
		"role":     claims.Role,
		"status":   claims.Status,
		"sid":      claims.SID,
		"iat":      claims.IssuedAt,
		"exp":      claims.Expires,
	})
	return token.SignedString([]byte(secret))
}

// VerifyAccessToken validates the signature and expiry and returns the claims.
func VerifyAccessToken(tokenString string) (*AccessTokenClaims, error) {
	secret, err := GetJwtSigningSecret("token verification")
	if err != nil {
		return nil, err
	}

	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		return nil, errors.New("invalid token")
	}

	raw, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid token claims")
	}

	claims := &AccessTokenClaims{
		Username: asString(raw["username"]),
		Role:     asString(raw["role"]),
		Status:   asString(raw["status"]),
		SID:      asString(raw["sid"]),
		IssuedAt: asInt64(raw["iat"]),
		Expires:  asInt64(raw["exp"]),
	}
	if id, ok := raw["id"].(float64); ok {
		claims.ID = int32(id)
	} else if id, ok := raw["id"].(int64); ok {
		claims.ID = int32(id)
	}
	if claims.ID == 0 || claims.IssuedAt == 0 {
		return nil, errors.New("invalid token claims")
	}
	return claims, nil
}

func asString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func asInt64(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}
