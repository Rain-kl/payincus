// Tests for the two mail upstream clients' pure parsing half.
//
// The HTTP half cannot be exercised in-process: every outbound request goes
// through lib.AssertSafeHTTPURL, which rejects loopback/private targets by design
// (that is the SSRF guard, not a limitation to work around). The response assembly
// is therefore split out and pinned here; the transport behaviour (labels, manual
// redirects, "fetch failed" mapping) is covered by the conformance suite's
// upstream-failure cases, which compare the resulting 502 bodies with Node.
package services

import (
	"encoding/json"
	"testing"
)

func TestCraneMailParseDomainInfo(t *testing.T) {
	payload := map[string]any{
		"verified": map[string]any{
			"status": "true",
			"txt":    "workspace-verify-token",
		},
		"dns": []any{
			map[string]any{"type": "MX", "record": "@", "value": "mx1.example.com", "prio": float64(10)},
			map[string]any{"type": "MX", "record": "@", "value": "mx2.example.com"},
			map[string]any{"type": "TXT", "record": "@", "value": "v=spf1 include:example.com ~all"},
			map[string]any{"type": "CNAME", "record": "mail", "value": "target.example.com"},
			map[string]any{"type": "TXT", "record": "dkim._domainkey", "value": "v=DKIM1; k=rsa; p=abc"},
		},
		"dkim": map[string]any{
			"selector": "s1",
			"key":      "abc",
		},
		"diskusage": "12.6",
	}

	info := CraneMailParseDomainInfo(payload)

	if !info.Verified {
		t.Fatalf("verified = false, want true")
	}
	if info.TxtRecord == nil || *info.TxtRecord != "workspace-verify-token" {
		t.Fatalf("txtRecord = %v", info.TxtRecord)
	}
	if info.DiskUsedMB == nil || *info.DiskUsedMB != 13 {
		t.Fatalf("diskUsedMb = %v, want rounded 13", info.DiskUsedMB)
	}

	// The workspace TXT is unshifted to the front, then DMARC and DKIM are appended.
	if len(info.DNSRecords) != 8 {
		t.Fatalf("dnsRecords length = %d, want 8", len(info.DNSRecords))
	}
	first := info.DNSRecords[0]
	if first["type"] != "TXT" || first["record"] != "workspace-verification" || first["value"] != "workspace-verify-token" {
		t.Fatalf("first record = %v", first)
	}
	secondToLast := info.DNSRecords[len(info.DNSRecords)-2]
	if secondToLast["record"] != "_dmarc" || secondToLast["value"] != "v=DMARC1; p=none; sp=none" {
		t.Fatalf("dmarc record = %v", secondToLast)
	}
	last := info.DNSRecords[len(info.DNSRecords)-1]
	if last["record"] != "s1" || last["value"] != "v=DKIM1; k=rsa; p=abc" {
		t.Fatalf("dkim record = %v", last)
	}

	// MX: prio is truthy -> "<prio> <value>", absent prio -> bare value.
	if len(info.MXRecords) != 2 || info.MXRecords[0] != "10 mx1.example.com" || info.MXRecords[1] != "mx2.example.com" {
		t.Fatalf("mxRecords = %v", info.MXRecords)
	}
	if info.SPFRecord == nil || *info.SPFRecord != "v=spf1 include:example.com ~all" {
		t.Fatalf("spfRecord = %v", info.SPFRecord)
	}
	if info.DKIMRecord == nil || *info.DKIMRecord != "v=DKIM1; k=rsa; p=abc" {
		t.Fatalf("dkimRecord = %v", info.DKIMRecord)
	}
	if len(info.CNAMERecords) != 1 || info.CNAMERecords[0]["record"] != "mail" || info.CNAMERecords[0]["value"] != "target.example.com" {
		t.Fatalf("cnameRecords = %v", info.CNAMERecords)
	}
}

func TestCraneMailParseDomainInfoUnverified(t *testing.T) {
	payload := map[string]any{
		"verified": map[string]any{"status": float64(0), "txt": "token"},
		"dns": []any{
			map[string]any{"type": "MX", "record": "@", "value": "mx1.example.com", "prio": float64(0)},
		},
		"dkim": map[string]any{"selector": "s1", "key": "abc"},
	}

	info := CraneMailParseDomainInfo(payload)
	if info.Verified {
		t.Fatalf("verified = true, want false")
	}
	// DKIM is only advertised after verification succeeded.
	for _, record := range info.DNSRecords {
		if record["record"] == "s1" {
			t.Fatalf("DKIM record must not be advertised before verification")
		}
	}
	// prio 0 is falsy in JavaScript -> the bare value is used.
	if len(info.MXRecords) != 1 || info.MXRecords[0] != "mx1.example.com" {
		t.Fatalf("mxRecords = %v", info.MXRecords)
	}
	// No workspace TXT value -> only the DMARC record was appended.
	if info.TxtRecord == nil || *info.TxtRecord != "token" {
		t.Fatalf("txtRecord = %v", info.TxtRecord)
	}
}

func TestCraneMailParseDiskUsageMB(t *testing.T) {
	cases := []struct {
		name   string
		value  any
		want   int
		absent bool
	}{
		{name: "number", value: float64(12.4), want: 12},
		{name: "string", value: "12.6", want: 13},
		{name: "empty string", value: "", want: 0}, // Number("") === 0
		{name: "blank string", value: "   ", want: 0},
		{name: "numeric json", value: json.Number("7"), want: 7},
		{name: "negative", value: float64(-1), absent: true},
		{name: "nan", value: "abc", absent: true},
		{name: "null", value: nil, absent: true},
		{name: "bool", value: true, absent: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := craneMailParseDiskUsageMB(testCase.value)
			if testCase.absent {
				if got != nil {
					t.Fatalf("expected undefined, got %d", *got)
				}
				return
			}
			if got == nil || *got != testCase.want {
				t.Fatalf("got %v, want %d", got, testCase.want)
			}
		})
	}
}

func TestCraneMailJSNumberString(t *testing.T) {
	cases := map[float64]string{
		10.0 / 3.0: "3.3333333333333335",
		5:          "5",
		0.5:        "0.5",
		120:        "120",
	}
	for value, want := range cases {
		if got := craneMailJSNumberString(value); got != want {
			t.Fatalf("craneMailJSNumberString(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestSmarterMailNumberFallbacks(t *testing.T) {
	if got := smarterMailNumber(nil); got != 0 {
		t.Fatalf("nil = %v", got)
	}
	if got := smarterMailNumber("2048"); got != 0 {
		t.Fatalf("string = %v, want 0 (the API returns numbers)", got)
	}
	if got := smarterMailNumber(float64(512)); got != 512 {
		t.Fatalf("number = %v", got)
	}
}
