// Port of server/src/services/cranemail.ts — the CraneMail reseller API client
// used for provisioning and inspecting mail domains.
//
// Cross-cutting behaviour that is easy to lose in a translation:
//
//   - Every outbound request goes through lib.AssertSafeHTTPURL (SSRF guard) with
//     the exact same labels as the TypeScript ("CraneMail API URL"), because the
//     label is interpolated into the validation error and that error text ends up
//     in an UPSTREAM_ERROR response body.
//   - fetch(..., { redirect: 'manual' }) is reproduced with a per-request client
//     whose CheckRedirect returns http.ErrUseLastResponse. Following the redirect
//     instead would turn a 3xx into the redirect target's body.
//   - A transport failure in Node surfaces as `TypeError: fetch failed`, so
//     `${err.message}` is literally "fetch failed". Go's *url.Error carries a
//     much more detailed message, which would change API responses, so transport
//     failures are collapsed to the same string here. Validation failures keep
//     their own message (safeFetch throws them before issuing the request).
//   - Responses are capped at 1 MiB (readLimitedTextResponse) and the request
//     times out after 30s (AbortSignal.timeout).
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	craneMailAPITimeoutMS      = 30_000
	craneMailMaxResponseBytes  = 1024 * 1024
	craneMailMaxResponseLabel  = "CraneMail API response"
	craneMailAPISourceURLLabel = "CraneMail API URL"
)

// CraneMailDNSRecord mirrors one entry of the upstream `dns` array. The
// TypeScript type is `{ type, record, value, prio? }`, but the array is passed
// through to the API response verbatim, so extra upstream keys are preserved.
type CraneMailDNSRecord = map[string]any

// CraneMailDomainInfo mirrors the getDomainInfo() result. Optional values are
// pointers because the TypeScript fields are optional and therefore vanish from
// the JSON response when absent.
type CraneMailDomainInfo struct {
	Verified     bool
	TxtRecord    *string
	DNSRecords   []CraneMailDNSRecord
	MXRecords    []string
	SPFRecord    *string
	DKIMRecord   *string
	CNAMERecords []map[string]any
	DiskUsedMB   *int
}

// CraneMailDomainCredentials mirrors the createDomain() result.
type CraneMailDomainCredentials struct {
	Username *string
	Password *string
	Server   *string
}

// CraneMailDomainUsage mirrors getDomainUsage().
type CraneMailDomainUsage struct {
	DiskUsedMB int
}

// craneMailParseDiskUsageMB mirrors parseDiskUsageMb: numbers and numeric strings
// are accepted, negatived values and non-finite input become undefined.
func craneMailParseDiskUsageMB(value any) *int {
	parsed := math.NaN()
	switch typed := value.(type) {
	case float64:
		parsed = typed
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			// Number("") === 0 (and Number("   ") === 0) in JavaScript.
			parsed = 0
		} else if converted, err := strconv.ParseFloat(trimmed, 64); err == nil {
			parsed = converted
		}
	case json.Number:
		if converted, err := typed.Float64(); err == nil {
			parsed = converted
		}
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
		return nil
	}
	rounded := int(math.Round(parsed))
	return &rounded
}

// craneMailJSNumberString renders a float the way Number#toString does for the
// values this module produces (prices and GB/MB quotients).
func craneMailJSNumberString(value float64) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 1) {
		return "Infinity"
	}
	if math.IsInf(value, -1) {
		return "-Infinity"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// craneMailStatusText renders "<status> <statusText>" the way undici exposes it.
func craneMailStatusText(response *http.Response) string {
	status := strconv.Itoa(response.StatusCode)
	text := strings.TrimSpace(strings.TrimPrefix(response.Status, status))
	if text == "" {
		return status
	}
	return status + " " + text
}

// mailServiceSafeFetch mirrors safeFetch(url, init, label) with
// `redirect: 'manual'`. See the package comment for the transport-error mapping.
func mailServiceSafeFetch(ctx context.Context, rawURL string, init *lib.SafeFetchInit, label string) (*http.Response, error) {
	if _, err := lib.AssertSafeHTTPURL(ctx, rawURL, label); err != nil {
		return nil, err
	}

	method := http.MethodGet
	var body io.Reader
	var headers http.Header
	if init != nil {
		if init.Method != "" {
			method = init.Method
		}
		body = init.Body
		headers = init.Headers
	}

	request, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, errors.New("fetch failed")
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	client := *lib.SafeOutboundHTTPClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	response, err := client.Do(request)
	if err != nil {
		var validationError *lib.OutboundTargetValidationError
		if errors.As(err, &validationError) {
			return nil, err
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("The operation was aborted due to timeout")
		}
		return nil, errors.New("fetch failed")
	}
	return response, nil
}

// mailServiceReadLimitedText mirrors readLimitedTextResponse: it rejects a
// declared or observed body larger than maxBytes and returns the decoded text.
func mailServiceReadLimitedText(response *http.Response, label string, maxBytes int64) (string, error) {
	if label == "" {
		label = "HTTP response"
	}

	if rawLength := response.Header.Get("Content-Length"); rawLength != "" {
		if parsed, err := strconv.ParseInt(strings.TrimSpace(rawLength), 10, 64); err == nil && parsed > maxBytes {
			return "", fmt.Errorf("%s is too large", label)
		}
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxBytes {
		return "", fmt.Errorf("%s is too large", label)
	}
	return string(data), nil
}

// craneMailCallAPI mirrors callApi(source, action, data).
func craneMailCallAPI(ctx context.Context, source models.MailSource, action string, data map[string]string) (any, error) {
	safeBaseURL, err := lib.AssertSafeHTTPURL(ctx, source.APIURL, craneMailAPISourceURLLabel)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(safeBaseURL.String(), "/")
	apiURL := baseURL + "/index.php?m=cranemail&action=api/" + action

	form := url.Values{}
	for key, value := range data {
		form.Set(key, value)
	}

	callCtx, cancel := context.WithTimeout(ctx, craneMailAPITimeoutMS*time.Millisecond)
	defer cancel()

	headers := http.Header{}
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	headers.Set("X-API-KEY", source.APIKey)

	response, err := mailServiceSafeFetch(callCtx, apiURL, &lib.SafeFetchInit{
		Method:  http.MethodPost,
		Headers: headers,
		Body:    strings.NewReader(form.Encode()),
	}, craneMailAPISourceURLLabel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	responseText, err := mailServiceReadLimitedText(response, craneMailMaxResponseLabel, craneMailMaxResponseBytes)
	if err != nil {
		return nil, err
	}

	if response.StatusCode < 200 || response.StatusCode > 299 {
		fmt.Fprintf(os.Stderr, "[CraneMail] API error: %s\n", craneMailStatusText(response))
		return nil, fmt.Errorf("CraneMail API error: %s", craneMailStatusText(response))
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(responseText), &result); err != nil {
		fmt.Fprintf(os.Stderr, "[CraneMail] Invalid JSON response (action=%s status=%d bodyLength=%d)\n",
			action, response.StatusCode, len(responseText))
		return nil, errors.New("CraneMail API returned invalid JSON")
	}

	// New API shape: {status: boolean, errors?: {...}}
	if status, ok := result["status"].(bool); ok && !status {
		errorMessage := "Unknown error"
		if rawErrors, exists := result["errors"]; exists {
			if errorMap, isMap := rawErrors.(map[string]any); isMap {
				parts := make([]string, 0, len(errorMap))
				for key, value := range errorMap {
					parts = append(parts, fmt.Sprintf("%s: %s", key, craneMailValueString(value)))
				}
				errorMessage = strings.Join(parts, "; ")
			}
		}
		fmt.Fprintf(os.Stderr, "[CraneMail] API returned error: %s\n", errorMessage)
		return nil, errors.New(errorMessage)
	}

	// Legacy API shape: {result: 'success'|'error', error?: string}
	if legacy, ok := result["result"].(string); ok && legacy == "error" {
		rawError, _ := result["error"].(string)
		if rawError == "" {
			rawError = "Unknown CraneMail API error"
		}
		sanitized := lib.SanitizeTokensInString(rawError)
		fmt.Fprintf(os.Stderr, "[CraneMail] API returned error: %s\n", sanitized)
		return nil, errors.New(sanitized)
	}

	// New API data lives in `data`, the legacy one in `info`. The TypeScript uses
	// `result.data || result.info || result`, i.e. JS truthiness.
	if value, exists := result["data"]; exists && craneMailTruthy(value) {
		return value, nil
	}
	if value, exists := result["info"]; exists && craneMailTruthy(value) {
		return value, nil
	}
	return result, nil
}

// craneMailValueString mirrors String(value) for the values JSON can produce.
func craneMailValueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
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

// craneMailMap returns value as a JSON object, or an empty map when it is not one.
func craneMailMap(value any) map[string]any {
	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	return map[string]any{}
}

// CraneMailCreateDomain mirrors createDomain(): CraneMail only supports a
// per-domain cap, so the subscription's shared total is split evenly so the
// provisioned caps cannot add up beyond the plan.
func CraneMailCreateDomain(
	ctx context.Context,
	source models.MailSource,
	domain string,
	subscriptionDiskLimitGB float64,
	domainLimit int,
) (CraneMailDomainCredentials, error) {
	sharedDomainDiskLimitGB := subscriptionDiskLimitGB / float64(domainLimit)
	result, err := craneMailCallAPI(ctx, source, "domain/create", map[string]string{
		"domain":                  domain,
		"disklimit":               craneMailJSNumberString(sharedDomainDiskLimitGB),
		"userlimit":               "0", // unlimited users
		"useraliaslimit":          "0",
		"domainaliaslimit":        "0",
		"maxusers":                "0", // alternate parameter name
		"maxuseraliases":          "0", // alternate parameter name
		"spamexperts":             "true",
		"spamexperts_adminaccess": "all", // required: 'primary' | 'all'
		"filestorage":             "true",
		"office":                  "true",
	})
	if err != nil {
		return CraneMailDomainCredentials{}, err
	}

	payload := craneMailMap(result)
	return CraneMailDomainCredentials{
		Username: craneMailOptionalString(payload["username"]),
		Password: craneMailOptionalString(payload["password"]),
		Server:   craneMailOptionalString(payload["server"]),
	}, nil
}

// craneMailOptionalString mirrors `typeof value === 'string' ? value : undefined`.
func craneMailOptionalString(value any) *string {
	typed, ok := value.(string)
	if !ok {
		return nil
	}
	return &typed
}

// CraneMailGetDomainInfo mirrors getDomainInfo().
func CraneMailGetDomainInfo(ctx context.Context, source models.MailSource, domain string) (CraneMailDomainInfo, error) {
	result, err := craneMailCallAPI(ctx, source, "domain/info", map[string]string{"domain": domain})
	if err != nil {
		return CraneMailDomainInfo{}, err
	}
	return CraneMailParseDomainInfo(craneMailMap(result)), nil
}

// CraneMailParseDomainInfo mirrors the parsing half of getDomainInfo(): the DNS
// record assembly (workspace-verification TXT, DMARC, conditional DKIM), the MX /
// SPF / DKIM / CNAME projections and the disk usage. Exported so it can be tested
// without an outbound HTTP call (the SSRF guard rejects loopback test servers).
func CraneMailParseDomainInfo(payload map[string]any) CraneMailDomainInfo {

	verifiedNode := craneMailMap(payload["verified"])
	verifiedStatus := verifiedNode["status"]
	verified := false
	switch typed := verifiedStatus.(type) {
	case bool:
		verified = typed
	case string:
		verified = typed == "true" || typed == "1"
	case float64:
		verified = typed == 1
	}

	records := make([]CraneMailDNSRecord, 0)
	if rawRecords, ok := payload["dns"].([]any); ok {
		for _, rawRecord := range rawRecords {
			if record, isMap := rawRecord.(map[string]any); isMap {
				records = append(records, record)
			}
		}
	}

	info := CraneMailDomainInfo{
		Verified:     verified,
		TxtRecord:    craneMailOptionalString(verifiedNode["txt"]),
		DNSRecords:   records,
		MXRecords:    []string{},
		CNAMERecords: []map[string]any{},
		DiskUsedMB:   craneMailParseDiskUsageMB(payload["diskusage"]),
	}

	// workspace-verification TXT record (from verified.txt)
	if txt := info.TxtRecord; txt != nil && *txt != "" {
		info.DNSRecords = append([]CraneMailDNSRecord{{
			"type":   "TXT",
			"record": "workspace-verification",
			"value":  *txt,
		}}, info.DNSRecords...)
	}

	// DMARC TXT record (fixed value)
	info.DNSRecords = append(info.DNSRecords, CraneMailDNSRecord{
		"type":   "TXT",
		"record": "_dmarc",
		"value":  "v=DMARC1; p=none; sp=none",
	})

	// DKIM TXT record (only after verification succeeded)
	dkim := craneMailMap(payload["dkim"])
	dkimSelector := craneMailOptionalString(dkim["selector"])
	dkimKey := craneMailOptionalString(dkim["key"])
	if verified && dkimSelector != nil && *dkimSelector != "" && dkimKey != nil && *dkimKey != "" {
		info.DNSRecords = append(info.DNSRecords, CraneMailDNSRecord{
			"type":   "TXT",
			"record": *dkimSelector,
			"value":  "v=DKIM1; k=rsa; p=" + *dkimKey,
		})
	}

	for _, record := range info.DNSRecords {
		recordType, _ := record["type"].(string)
		recordValue, _ := record["value"].(string)
		switch {
		case recordType == "MX":
			if prio, exists := record["prio"]; exists && craneMailTruthy(prio) {
				info.MXRecords = append(info.MXRecords, craneMailValueString(prio)+" "+recordValue)
			} else {
				info.MXRecords = append(info.MXRecords, recordValue)
			}
		case recordType == "CNAME":
			recordName, _ := record["record"].(string)
			info.CNAMERecords = append(info.CNAMERecords, map[string]any{
				"record": recordName,
				"value":  recordValue,
			})
		}
	}

	for _, record := range info.DNSRecords {
		recordType, _ := record["type"].(string)
		recordValue, _ := record["value"].(string)
		recordName, _ := record["record"].(string)
		if info.SPFRecord == nil && recordType == "TXT" && strings.Contains(recordValue, "spf") {
			value := recordValue
			info.SPFRecord = &value
		}
		if info.DKIMRecord == nil && recordType == "TXT" && strings.Contains(recordName, "dkim") {
			value := recordValue
			info.DKIMRecord = &value
		}
	}

	return info
}

// craneMailTruthy mirrors JS truthiness for the values JSON can produce.
func craneMailTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	case string:
		return typed != ""
	default:
		return true
	}
}

// CraneMailGetDomainUsage mirrors getDomainUsage().
func CraneMailGetDomainUsage(ctx context.Context, source models.MailSource, domain string) (*CraneMailDomainUsage, error) {
	result, err := craneMailCallAPI(ctx, source, "domain/info", map[string]string{"domain": domain})
	if err != nil {
		return nil, err
	}
	payload := craneMailMap(result)
	diskUsedMB := craneMailParseDiskUsageMB(payload["diskusage"])
	if diskUsedMB == nil {
		return nil, nil
	}
	return &CraneMailDomainUsage{DiskUsedMB: *diskUsedMB}, nil
}

// CraneMailDeleteDomain mirrors deleteDomain().
func CraneMailDeleteDomain(ctx context.Context, source models.MailSource, domain string) error {
	_, err := craneMailCallAPI(ctx, source, "domain/delete", map[string]string{"domain": domain})
	return err
}

// CraneMailSuspendDomain mirrors suspendDomain().
func CraneMailSuspendDomain(ctx context.Context, source models.MailSource, domain string) error {
	_, err := craneMailCallAPI(ctx, source, "domain/suspend", map[string]string{"domain": domain})
	return err
}

// CraneMailResumeDomain mirrors resumeDomain()/unsuspendDomain().
func CraneMailResumeDomain(ctx context.Context, source models.MailSource, domain string) error {
	_, err := craneMailCallAPI(ctx, source, "domain/unsuspend", map[string]string{"domain": domain})
	return err
}

// CraneMailModifyDomainOptions mirrors the options argument of modifyDomain().
type CraneMailModifyDomainOptions struct {
	DiskLimitGB *float64
	UserLimit   *float64
}

// CraneMailModifyDomain mirrors modifyDomain().
func CraneMailModifyDomain(ctx context.Context, source models.MailSource, domain string, options CraneMailModifyDomainOptions) error {
	data := map[string]string{"domain": domain}
	if options.DiskLimitGB != nil {
		data["disklimit"] = craneMailJSNumberString(*options.DiskLimitGB)
	}
	if options.UserLimit != nil {
		data["userlimit"] = craneMailJSNumberString(*options.UserLimit)
	}
	_, err := craneMailCallAPI(ctx, source, "domain/modify", data)
	return err
}
