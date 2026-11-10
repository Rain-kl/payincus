package lib

import (
	"encoding/json"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// egressPackageDryRunDB builds an offline GORM handle. `sql.Open` does not dial,
// and DisableAutomaticPing keeps gorm.Open from probing the server, so the SQL
// builder can be inspected without a database.
func egressPackageDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()
	handle, err := gorm.Open(postgres.New(postgres.Config{
		DSN: "host=127.0.0.1 user=test dbname=test sslmode=disable",
	}), &gorm.Config{
		DryRun:               true,
		DisableAutomaticPing: true,
		Logger:               gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("open dry-run gorm: %v", err)
	}
	return handle
}

func egressPackageStringPtr(value string) *string { return &value }
func egressPackageInt32Ptr(value int32) *int32    { return &value }

// The package admission rule: every rejection branch is a security control, so
// each one is pinned here. Only the profile lookup needs a database; every
// branch below is decided before that query.
func TestValidatePackageEgressRejections(t *testing.T) {
	cases := []struct {
		name  string
		input EgressPackageValidationInput
		want  string
	}{
		{
			name:  "unknown mode",
			input: EgressPackageValidationInput{EgressMode: egressPackageStringPtr("proxy")},
			want:  "Invalid package egress mode",
		},
		{
			name:  "empty mode is not null, so it is invalid",
			input: EgressPackageValidationInput{EgressMode: egressPackageStringPtr("")},
			want:  "Invalid package egress mode",
		},
		{
			name: "host mode may not select a profile",
			input: EgressPackageValidationInput{
				EgressMode:      egressPackageStringPtr("host"),
				EgressProfileID: egressPackageInt32Ptr(7),
			},
			want: "Only proxy mode may select an egress profile",
		},
		{
			name: "direct mode may not select a profile",
			input: EgressPackageValidationInput{
				EgressMode:      egressPackageStringPtr("direct"),
				EgressProfileID: egressPackageInt32Ptr(7),
			},
			want: "Only proxy mode may select an egress profile",
		},
		{
			name: "direct requires nat",
			input: EgressPackageValidationInput{
				EgressMode:  egressPackageStringPtr("direct"),
				NetworkMode: egressPackageStringPtr("ipv6_only"),
			},
			want: "Package egress currently requires an unprivileged IPv4 NAT container",
		},
		{
			name: "direct requires a container",
			input: EgressPackageValidationInput{
				EgressMode:   egressPackageStringPtr("direct"),
				InstanceType: egressPackageStringPtr("vm"),
			},
			want: "Package egress currently requires an unprivileged IPv4 NAT container",
		},
		{
			name: "direct rejects privileged",
			input: EgressPackageValidationInput{
				EgressMode: egressPackageStringPtr("direct"),
				Privileged: true,
			},
			want: "Package egress currently requires an unprivileged IPv4 NAT container",
		},
		{
			name: "profile mode requires a profile id",
			input: EgressPackageValidationInput{
				EgressMode: egressPackageStringPtr("profile"),
			},
			want: "Egress profile is inactive or not permitted on the selected hosts",
		},
		{
			name: "profile id 0 is falsy in JS",
			input: EgressPackageValidationInput{
				EgressMode:      egressPackageStringPtr("profile"),
				EgressProfileID: egressPackageInt32Ptr(0),
			},
			want: "Egress profile is inactive or not permitted on the selected hosts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePackageEgress(t.Context(), nil, tc.input)
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.want)
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err.Error(), tc.want)
			}
			if _, ok := err.(*NetworkInputError); !ok {
				t.Fatalf("error type = %T, want *NetworkInputError", err)
			}
		})
	}
}

func TestValidatePackageEgressAcceptsHostAndDirect(t *testing.T) {
	// Host mode short-circuits before every other check.
	if err := ValidatePackageEgress(t.Context(), nil, EgressPackageValidationInput{}); err != nil {
		t.Fatalf("default (host) mode: %v", err)
	}
	if err := ValidatePackageEgress(t.Context(), nil, EgressPackageValidationInput{
		EgressMode:  egressPackageStringPtr("host"),
		NetworkMode: egressPackageStringPtr("ipv6_only"),
		Privileged:  true,
	}); err != nil {
		t.Fatalf("host mode must not require NAT/container: %v", err)
	}
	if err := ValidatePackageEgress(t.Context(), nil, EgressPackageValidationInput{
		EgressMode: egressPackageStringPtr("direct"),
	}); err != nil {
		t.Fatalf("direct mode with defaults must pass: %v", err)
	}
	if err := ValidatePackageEgress(t.Context(), nil, EgressPackageValidationInput{
		EgressMode:   egressPackageStringPtr("direct"),
		NetworkMode:  egressPackageStringPtr("nat"),
		InstanceType: egressPackageStringPtr("container"),
	}); err != nil {
		t.Fatalf("explicit direct defaults must pass: %v", err)
	}
}

// The binding lock is the SINGLE-argument form 74201. If this statement ever
// becomes the two-int form, PostgreSQL keeps it in a different lock space and the
// Node and Go backends stop excluding each other during the parallel run.
func TestBindPackageEgressUsesSingleArgumentEgressLock(t *testing.T) {
	handle := egressPackageDryRunDB(t)
	sql := handle.ToSQL(func(tx *gorm.DB) *gorm.DB {
		if err := egressPackageAdvisoryLock(tx); err != nil {
			t.Fatalf("lock: %v", err)
		}
		return tx
	})
	// GORM inlines the namespace argument, which pins both the statement and the
	// single-argument (bigint) form.
	if !strings.Contains(sql, "pg_advisory_xact_lock(74201::bigint)") {
		t.Fatalf("SQL = %q, want the single-argument 74201 lock", sql)
	}
	if db.EgressAdvisoryLockNamespace != 74201 {
		t.Fatalf("db.EgressAdvisoryLockNamespace = %d, want 74201", db.EgressAdvisoryLockNamespace)
	}
}

func TestBindPackageEgressBindingJSON(t *testing.T) {
	// The routes spread the result into the instance create payload, so the keys
	// must stay camelCase and a nil profile must serialise as null.
	payload := EgressBinding{EgressMode: "host"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"egressMode":"host","egressProfileId":null}`
	if string(encoded) != want {
		t.Fatalf("json = %s, want %s", encoded, want)
	}
}

// The db package reaches lib through Register* hooks (db ↛ lib). This proves the
// init() registration actually happened: an unregistered validator would answer
// "package egress validator is not registered" for every input.
func TestPackageEgressValidatorIsRegistered(t *testing.T) {
	if err := db.PackageEgressValidator(t.Context(), nil, db.PackageEgressValidationInput{}); err != nil {
		t.Fatalf("host mode must pass through the registered validator, got %v", err)
	}
	err := db.PackageEgressValidator(t.Context(), nil, db.PackageEgressValidationInput{
		EgressMode: egressPackageStringPtr("bogus"),
	})
	if err == nil || err.Error() != "Invalid package egress mode" {
		t.Fatalf("registered validator error = %v", err)
	}
}

func TestEgressPackageMaxGroups(t *testing.T) {
	if EgressPackageMaxGroups != 16 {
		t.Fatalf("EgressPackageMaxGroups = %d, want 16", EgressPackageMaxGroups)
	}
	if EgressPackageMaxGroups != egressMaxGroups {
		t.Fatalf("MAX_EGRESS_GROUPS drifted from egressMaxGroups: %d vs %d", EgressPackageMaxGroups, egressMaxGroups)
	}
}
