// Port of server/src/services/smartermail.ts — the SmarterMail API client used
// for mailbox account operations.
//
// Same transport rules as cranemail.go: SSRF-guarded URLs, manual redirects,
// 30s timeout, 1 MiB response cap, and transport failures collapsed to Node's
// "fetch failed" text so UPSTREAM_ERROR details stay identical.
//
// One behavioural detail worth calling out: every callApi() re-authenticates
// first (getAuthToken), exactly like the TypeScript — SmarterMail tokens are
// per-request here, there is no cache.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	smarterMailAPITimeoutMS       = 30_000
	smarterMailMaxResponseBytes   = 1024 * 1024
	smarterMailResponseLabel      = "SmarterMail API response"
	smarterMailErrorResponseLabel = "SmarterMail API error response"
	smarterMailSourceURLLabel     = "SmarterMail URL"
)

// SmarterMailAccountInput mirrors the account argument of createAccount().
type SmarterMailAccountInput struct {
	Username    string
	Password    string
	DisplayName string
	DiskLimitMB float64
}

// SmarterMailAccountUpdates mirrors the updates argument of updateAccount().
type SmarterMailAccountUpdates struct {
	DisplayName *string
	DiskLimitMB *float64
}

// SmarterMailAccountInfo mirrors getAccountInfo()'s result.
type SmarterMailAccountInfo struct {
	Email       string
	DisplayName string
	DiskLimitMB float64
	DiskUsedMB  float64
	IsEnabled   bool
}

// SmarterMailAccountSummary mirrors one entry of listAccounts().
type SmarterMailAccountSummary struct {
	Username    string
	Email       string
	DisplayName string
	DiskLimitMB float64
	DiskUsedMB  float64
}

// SmarterMailAccountUsage mirrors one entry of listAccountUsage().
type SmarterMailAccountUsage struct {
	Username   string
	DiskUsedMB int
}

// smarterMailBaseURL mirrors getSmarterMailBaseUrl().
func smarterMailBaseURL(ctx context.Context, source models.MailSource) (string, error) {
	safeBaseURL, err := lib.AssertSafeHTTPURL(ctx, source.SmarterMailURL, smarterMailSourceURLLabel)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(safeBaseURL.String(), "/"), nil
}

// smarterMailStringValue mirrors JS string interpolation of a JSON value.
func smarterMailStringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return craneMailJSNumberString(typed)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// smarterMailNumber mirrors `value || 0` for the numeric fields SmarterMail
// returns: anything that is not a usable number falls back to zero.
func smarterMailNumber(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed
		}
	}
	return 0
}

// smarterMailPayload turns a JSON string into an object; non-objects yield an
// empty map so field lookups behave like the TypeScript's undefined access.
func smarterMailPayload(value any) map[string]any {
	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	return map[string]any{}
}

// smarterMailPostJSON issues a JSON POST through the shared outbound client.
func smarterMailPostJSON(
	ctx context.Context,
	rawURL string,
	payload []byte,
	headers http.Header,
	label string,
) (*http.Response, error) {
	return mailServiceSafeFetch(ctx, rawURL, &lib.SafeFetchInit{
		Method:  http.MethodPost,
		Headers: headers,
		Body:    strings.NewReader(string(payload)),
	}, label)
}

// smarterMailGetAuthToken mirrors getAuthToken(): a per-call login as the domain
// administrator.
func smarterMailGetAuthToken(ctx context.Context, domain string, adminUsername, adminPassword, baseURL string) (string, error) {
	email := adminUsername + "@" + domain

	body, err := json.Marshal(struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{Username: email, Password: adminPassword})
	if err != nil {
		return "", err
	}

	callCtx, cancel := context.WithTimeout(ctx, smarterMailAPITimeoutMS*time.Millisecond)
	defer cancel()

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")

	response, err := smarterMailPostJSON(callCtx, baseURL+"/api/v1/auth/authenticate-user", body, headers, smarterMailSourceURLLabel)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", fmt.Errorf("SmarterMail auth error: %s", craneMailStatusText(response))
	}

	responseText, err := mailServiceReadLimitedText(response, smarterMailResponseLabel, smarterMailMaxResponseBytes)
	if err != nil {
		return "", err
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		return "", err
	}

	accessToken, _ := result["accessToken"].(string)
	if accessToken == "" {
		message, _ := result["message"].(string)
		if message == "" {
			message = "Failed to get SmarterMail token"
		}
		return "", errors.New(lib.SanitizeTokensInString(message))
	}
	return accessToken, nil
}

// smarterMailCallAPI mirrors callApi(source, domain, adminUser, adminPass,
// method, endpoint, data).
func smarterMailCallAPI(
	ctx context.Context,
	source models.MailSource,
	domain string,
	adminUsername string,
	adminPassword string,
	method string,
	endpoint string,
	data any,
) (any, error) {
	baseURL, err := smarterMailBaseURL(ctx, source)
	if err != nil {
		return nil, err
	}
	token, err := smarterMailGetAuthToken(ctx, domain, adminUsername, adminPassword, baseURL)
	if err != nil {
		return nil, err
	}

	var payload []byte
	if data != nil {
		payload, err = json.Marshal(data)
		if err != nil {
			return nil, err
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, smarterMailAPITimeoutMS*time.Millisecond)
	defer cancel()

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+token)

	var response *http.Response
	if payload == nil {
		response, err = mailServiceSafeFetch(callCtx, baseURL+"/api/v1"+endpoint, &lib.SafeFetchInit{
			Method:  method,
			Headers: headers,
		}, smarterMailSourceURLLabel)
	} else {
		response, err = smarterMailPostJSON(callCtx, baseURL+"/api/v1"+endpoint, payload, headers, smarterMailSourceURLLabel)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		errorText, readErr := mailServiceReadLimitedText(response, smarterMailErrorResponseLabel, smarterMailMaxResponseBytes)
		if readErr != nil {
			return nil, readErr
		}
		return nil, fmt.Errorf("SmarterMail API error: %s - %s",
			craneMailStatusText(response), lib.SanitizeTokensInString(errorText))
	}

	responseText, err := mailServiceReadLimitedText(response, smarterMailResponseLabel, smarterMailMaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if responseText == "" {
		return map[string]any{}, nil
	}

	var result any
	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		return nil, err
	}
	return result, nil
}

// SmarterMailCreateAccount mirrors createAccount().
func SmarterMailCreateAccount(
	ctx context.Context,
	source models.MailSource,
	domain string,
	adminUsername string,
	adminPassword string,
	account SmarterMailAccountInput,
) error {
	_, err := smarterMailCallAPI(ctx, source, domain, adminUsername, adminPassword,
		http.MethodPost, "/settings/domain/users/user-put", map[string]any{
			"userData": map[string]any{
				"userName":       account.Username,
				"password":       account.Password,
				"displayName":    account.DisplayName,
				"maxMailboxSize": account.DiskLimitMB,
				"domain":         domain,
				"isEnabled":      true,
			},
		})
	return err
}

// SmarterMailUpdateAccount mirrors updateAccount().
func SmarterMailUpdateAccount(
	ctx context.Context,
	source models.MailSource,
	domain string,
	adminUsername string,
	adminPassword string,
	username string,
	updates SmarterMailAccountUpdates,
) error {
	userData := map[string]any{
		"userName": username,
		"domain":   domain,
	}
	if updates.DisplayName != nil {
		userData["displayName"] = *updates.DisplayName
	}
	if updates.DiskLimitMB != nil {
		userData["maxMailboxSize"] = *updates.DiskLimitMB
	}

	_, err := smarterMailCallAPI(ctx, source, domain, adminUsername, adminPassword,
		http.MethodPost, "/settings/domain/users/user-put", map[string]any{"userData": userData})
	return err
}

// SmarterMailResetPassword mirrors resetPassword().
func SmarterMailResetPassword(
	ctx context.Context,
	source models.MailSource,
	domain string,
	adminUsername string,
	adminPassword string,
	username string,
	newPassword string,
) error {
	_, err := smarterMailCallAPI(ctx, source, domain, adminUsername, adminPassword,
		http.MethodPost, "/settings/domain/users/set-password", map[string]any{
			"userName": username,
			"domain":   domain,
			"password": newPassword,
		})
	return err
}

// SmarterMailDeleteAccount mirrors deleteAccount().
func SmarterMailDeleteAccount(
	ctx context.Context,
	source models.MailSource,
	domain string,
	adminUsername string,
	adminPassword string,
	username string,
) error {
	_, err := smarterMailCallAPI(ctx, source, domain, adminUsername, adminPassword,
		http.MethodPost, "/settings/domain/users/user-delete", map[string]any{
			"userName": username,
			"domain":   domain,
		})
	return err
}

// SmarterMailGetAccountInfo mirrors getAccountInfo(): every failure yields nil,
// never an error.
func SmarterMailGetAccountInfo(
	ctx context.Context,
	source models.MailSource,
	domain string,
	adminUsername string,
	adminPassword string,
	username string,
) *SmarterMailAccountInfo {
	result, err := smarterMailCallAPI(ctx, source, domain, adminUsername, adminPassword,
		http.MethodPost, "/settings/domain/users/user-get", map[string]any{
			"userName": username,
			"domain":   domain,
		})
	if err != nil {
		return nil
	}

	payload := smarterMailPayload(result)
	rawUserData, exists := payload["userData"]
	if !exists || rawUserData == nil {
		return nil
	}
	userData := smarterMailPayload(rawUserData)

	userName := smarterMailStringValue(userData["userName"])
	displayName, _ := userData["displayName"].(string)
	if displayName == "" {
		displayName = userName
	}
	isEnabled := true
	if flag, ok := userData["isEnabled"].(bool); ok {
		isEnabled = flag
	}

	return &SmarterMailAccountInfo{
		Email:       userName + "@" + domain,
		DisplayName: displayName,
		DiskLimitMB: smarterMailNumber(userData["maxMailboxSize"]),
		DiskUsedMB:  smarterMailNumber(userData["mailboxSizeUsed"]),
		IsEnabled:   isEnabled,
	}
}

// SmarterMailListAccounts mirrors listAccounts().
func SmarterMailListAccounts(
	ctx context.Context,
	source models.MailSource,
	domain string,
	adminUsername string,
	adminPassword string,
) ([]SmarterMailAccountSummary, error) {
	result, err := smarterMailCallAPI(ctx, source, domain, adminUsername, adminPassword,
		http.MethodGet, "/settings/domain/users/list?domain="+domain, nil)
	if err != nil {
		return nil, err
	}

	payload := smarterMailPayload(result)
	rawUsers, ok := payload["users"].([]any)
	if !ok {
		return []SmarterMailAccountSummary{}, nil
	}

	accounts := make([]SmarterMailAccountSummary, 0, len(rawUsers))
	for _, rawUser := range rawUsers {
		user := smarterMailPayload(rawUser)
		userName := smarterMailStringValue(user["userName"])
		displayName, _ := user["displayName"].(string)
		if displayName == "" {
			displayName = userName
		}
		accounts = append(accounts, SmarterMailAccountSummary{
			Username:    userName,
			Email:       userName + "@" + domain,
			DisplayName: displayName,
			DiskLimitMB: smarterMailNumber(user["maxMailboxSize"]),
			DiskUsedMB:  smarterMailNumber(user["mailboxSizeUsed"]),
		})
	}
	return accounts, nil
}

// SmarterMailListAccountUsage mirrors listAccountUsage(): upstream disk usage per
// mailbox, skipping entries without a usable username or size.
func SmarterMailListAccountUsage(
	ctx context.Context,
	source models.MailSource,
	domain string,
	adminUsername string,
	adminPassword string,
) ([]SmarterMailAccountUsage, error) {
	result, err := smarterMailCallAPI(ctx, source, domain, adminUsername, adminPassword,
		http.MethodGet, "/settings/domain/users/list?domain="+url.QueryEscape(domain), nil)
	if err != nil {
		return nil, err
	}

	payload := smarterMailPayload(result)
	rawUsers, ok := payload["users"].([]any)
	if !ok {
		return []SmarterMailAccountUsage{}, nil
	}

	usages := make([]SmarterMailAccountUsage, 0, len(rawUsers))
	for _, rawUser := range rawUsers {
		user := smarterMailPayload(rawUser)
		userName, ok := user["userName"].(string)
		if !ok {
			continue
		}
		userName = strings.TrimSpace(userName)
		diskUsedMB := craneMailParseDiskUsageMB(user["mailboxSizeUsed"])
		if userName == "" || diskUsedMB == nil {
			continue
		}
		usages = append(usages, SmarterMailAccountUsage{Username: userName, DiskUsedMB: *diskUsedMB})
	}
	return usages, nil
}
