package services

import (
	"context"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestHostAddressMonitorNormalizationAndNextRun(t *testing.T) {
	in4, err := extractHostInputAddress("https://203.0.113.10:8443")
	if err != nil || in4.Kind != models.HostAddressKindIpv4 || in4.Address != "203.0.113.10" {
		t.Fatalf("ipv4 extract = (%+v, %v)", in4, err)
	}

	in6, err := extractHostInputAddress("https://[2001:0db8::1]:8443")
	if err != nil || in6.Kind != models.HostAddressKindIpv6 || in6.Address != "2001:db8::1" {
		t.Fatalf("ipv6 extract = (%+v, %v)", in6, err)
	}

	dom, err := extractHostInputAddress("https://Node01.Example.COM.:8443")
	if err != nil || dom.Kind != models.HostAddressKindDomain || dom.Address != "node01.example.com" {
		t.Fatalf("domain extract = (%+v, %v)", dom, err)
	}

	snap := buildHostAddressSnapshot(dom, []normalizedHostAddress{
		{Address: "203.0.113.10", Kind: models.HostAddressKindIpv4},
	})
	if len(snap.Aliases) != 2 {
		t.Fatalf("aliases count = %d, want 2", len(snap.Aliases))
	}

	t1 := time.Date(2026, 11, 18, 1, 12, 0, 0, time.UTC)
	n1 := HostAddressMonitorNextRun(t1)
	if n1.Minute() != 30 || !n1.After(t1) {
		t.Fatalf("HostAddressMonitorNextRun(%v) = %v", t1, n1)
	}
	t2 := time.Date(2026, 11, 18, 1, 30, 0, 0, time.UTC)
	n2 := HostAddressMonitorNextRun(t2)
	if n2.Minute() != 0 || n2.Hour() != 2 || !n2.After(t2) {
		t.Fatalf("HostAddressMonitorNextRun(%v) = %v", t2, n2)
	}

	origLookup := hostAddressLookupIPs
	defer func() { hostAddressLookupIPs = origLookup }()
	hostAddressLookupIPs = func(ctx context.Context, domain string) ([]string, error) {
		return []string{"203.0.113.1", "203.0.113.1"}, nil
	}
	res, err := resolveHostDomainAddresses(context.Background(), "example.com")
	if err != nil || len(res) != 1 || res[0].Address != "203.0.113.1" {
		t.Fatalf("resolveHostDomainAddresses = (%+v, %v)", res, err)
	}
}
