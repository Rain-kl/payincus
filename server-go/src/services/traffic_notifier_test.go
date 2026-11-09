// Unit tests for the one helper services/traffic-notifier.ts exposes to the
// traffic route module: the re-exported formatBytes.
package services

import (
	"testing"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

func TestTrafficNotifierFormatBytes(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0.00 B"},
		{1, "1.00 B"},
		{1023, "1023.00 B"},
		{1024, "1.00 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
		{1536 * 1024 * 1024, "1.50 GB"},
		{1024 * 1024 * 1024 * 1024, "1.00 TB"},
		// 257 GB in bytes, the example from calculatePercentage's comment.
		{257 * 1024 * 1024 * 1024, "257.00 GB"},
	}
	for _, tc := range cases {
		if got := FormatBytes(tc.bytes); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
		// The route module imports the notifier's re-export; it must stay identical
		// to the traffic-utils implementation it delegates to.
		if got, want := FormatBytes(tc.bytes), lib.FormatBytes(tc.bytes); got != want {
			t.Errorf("FormatBytes(%d) diverged from lib.FormatBytes: %q vs %q", tc.bytes, got, want)
		}
	}
}
