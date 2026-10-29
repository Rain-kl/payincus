package lib

import (
	"fmt"
	"testing"
	"time"
)

// resetConfigCache clears the shared cache and pins the clock for a test,
// restoring both afterwards.
func resetConfigCache(t *testing.T) {
	t.Helper()
	original := configCacheNow
	InvalidateAllConfigCache()
	t.Cleanup(func() {
		configCacheNow = original
		InvalidateAllConfigCache()
	})
}

func TestConfigCacheSetAndGet(t *testing.T) {
	resetConfigCache(t)

	value := "hello"
	SetCachedConfig("brand_name", &value)

	got, found := GetCachedConfig("brand_name")
	if !found || got == nil || *got != "hello" {
		t.Fatalf("GetCachedConfig = (%v, %v), want (hello, true)", got, found)
	}

	if _, found := GetCachedConfig("missing"); found {
		t.Fatal("an unknown key must not be reported as cached")
	}
}

// A cached nil is a real entry: it means "this key has no value", which lets the
// caller skip the database on repeated lookups.
func TestConfigCacheCachesNullValue(t *testing.T) {
	resetConfigCache(t)

	SetCachedConfig("absent", nil)

	got, found := GetCachedConfig("absent")
	if !found {
		t.Fatal("a cached nil must still be reported as present")
	}
	if got != nil {
		t.Fatalf("cached value = %q, want nil", *got)
	}
}

func TestConfigCacheExpiresAfterTTL(t *testing.T) {
	resetConfigCache(t)

	base := time.Now()
	configCacheNow = func() time.Time { return base }

	value := "v"
	SetCachedConfig("k", &value)

	configCacheNow = func() time.Time { return base.Add(configCacheTTL - time.Second) }
	if _, found := GetCachedConfig("k"); !found {
		t.Fatal("entry must still be cached before the TTL elapses")
	}

	// Node uses `Date.now() > expiresAt`, so the exact boundary is still valid.
	configCacheNow = func() time.Time { return base.Add(configCacheTTL) }
	if _, found := GetCachedConfig("k"); !found {
		t.Fatal("entry must survive the exact TTL boundary")
	}

	configCacheNow = func() time.Time { return base.Add(configCacheTTL + time.Nanosecond) }
	if _, found := GetCachedConfig("k"); found {
		t.Fatal("entry must be dropped once past the TTL")
	}
	if size := GetCacheStats().Size; size != 0 {
		t.Fatalf("expired entry still counted: size = %d", size)
	}
}

func TestConfigCacheReSetReplacesInPlace(t *testing.T) {
	resetConfigCache(t)

	first := "first"
	SetCachedConfig("k", &first)
	second := "second"
	SetCachedConfig("k", &second)

	if size := GetCacheStats().Size; size != 1 {
		t.Fatalf("size = %d, want 1 (re-set must not add a second entry)", size)
	}
	got, found := GetCachedConfig("k")
	if !found || got == nil || *got != "second" {
		t.Fatalf("GetCachedConfig = (%v, %v), want (second, true)", got, found)
	}
}

func TestConfigCacheEvictsOldestWhenFull(t *testing.T) {
	resetConfigCache(t)

	base := time.Now()
	configCacheNow = func() time.Time { return base }

	for i := 0; i < configCacheMaxSize; i++ {
		key := fmt.Sprintf("k%03d", i)
		value := key
		SetCachedConfig(key, &value)
	}
	if size := GetCacheStats().Size; size != configCacheMaxSize {
		t.Fatalf("size = %d, want %d", size, configCacheMaxSize)
	}

	value := "new"
	SetCachedConfig("new", &value)

	// The ten oldest keys are dropped, then the new key is added.
	wantSize := configCacheMaxSize - configCacheEvictBatch + 1
	if size := GetCacheStats().Size; size != wantSize {
		t.Fatalf("size = %d, want %d", size, wantSize)
	}
	for _, evicted := range []string{"k000", "k009"} {
		if _, found := GetCachedConfig(evicted); found {
			t.Fatalf("%s should have been evicted", evicted)
		}
	}
	for _, kept := range []string{"k010", "new"} {
		if _, found := GetCachedConfig(kept); !found {
			t.Fatalf("%s should still be cached", kept)
		}
	}
}

func TestConfigCacheInvalidation(t *testing.T) {
	resetConfigCache(t)

	a, b, c := "a", "b", "c"
	SetCachedConfig("a", &a)
	SetCachedConfig("b", &b)
	SetCachedConfig("c", &c)

	InvalidateCachedConfig("a")
	if _, found := GetCachedConfig("a"); found {
		t.Fatal("a should have been invalidated")
	}
	if _, found := GetCachedConfig("b"); !found {
		t.Fatal("b should have survived single-key invalidation")
	}

	InvalidateCachedConfigs([]string{"b"})
	if _, found := GetCachedConfig("b"); found {
		t.Fatal("b should have been invalidated in bulk")
	}
	if _, found := GetCachedConfig("c"); !found {
		t.Fatal("c should have survived bulk invalidation")
	}

	InvalidateAllConfigCache()
	if size := GetCacheStats().Size; size != 0 {
		t.Fatalf("size = %d, want 0 after InvalidateAllConfigCache", size)
	}
}

func TestConfigCacheStats(t *testing.T) {
	resetConfigCache(t)

	stats := GetCacheStats()
	if stats.MaxSize != 100 {
		t.Fatalf("MaxSize = %d, want 100", stats.MaxSize)
	}
	// TTL is reported in milliseconds to match the Node getCacheStats() shape.
	if stats.TTL != 300000 {
		t.Fatalf("TTL = %d, want 300000", stats.TTL)
	}
}
