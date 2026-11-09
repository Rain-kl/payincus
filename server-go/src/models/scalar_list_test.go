package models

import (
	"testing"

	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// TestScalarListModelsAreParsable pins that GORM can build a schema for every model
// that carries a Prisma scalar list (Int[] → PostgreSQL array).
//
// This is not cosmetic: without the `serializer:json` tag the generator now emits,
// GORM fails with
//
//	failed to parse field: GPUDeviceIds, error: unsupported data type: &[]
//
// on any model-based Find/First, so the failure only surfaces at runtime against a
// real database. Parsing the schema here catches it in a unit test instead.
func TestScalarListModelsAreParsable(t *testing.T) {
	// A dialector without a live connection is enough: schema parsing is offline.
	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN:                  "host=127.0.0.1 user=none dbname=none sslmode=disable",
		PreferSimpleProtocol: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	cases := []struct {
		name  string
		model any
		field string
	}{
		{"Package", &Package{}, "GPUDeviceIds"},
		{"PackagePlan", &PackagePlan{}, "GPUDeviceIds"},
		{"EgressProfile", &EgressProfile{}, "AllowedHostIds"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := schema.Parse(tc.model, &sync.Map{}, db.NamingStrategy)
			if err != nil {
				t.Fatalf("schema.Parse(%s): %v", tc.name, err)
			}
			field, ok := parsed.FieldsByName[tc.field]
			if !ok {
				t.Fatalf("%s has no field %s", tc.name, tc.field)
			}
			// The serializer is what makes the array column usable.
			if field.Serializer == nil {
				t.Fatalf("%s.%s has no serializer; GORM will fail on a real query",
					tc.name, tc.field)
			}
		})
	}
}
