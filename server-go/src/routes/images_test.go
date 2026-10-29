package routes

import (
	"net/url"
	"testing"
)

// TestParseOptionalImageQuery pins the strict positive-integer query parsing the
// image route guard requires (reject 0, -1, 01, non-numeric, empty).
func TestParseOptionalImageQuery(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		present bool
		want    *int
		wantOK  bool
	}{
		{"absent", "", false, nil, true},
		{"valid", "128", true, imageIntPtr(128), true},
		{"valid large", "9007199254740991", true, imageIntPtr(9007199254740991), true},
		{"zero", "0", true, nil, false},
		{"negative", "-1", true, nil, false},
		{"leading zero", "01", true, nil, false},
		{"empty value", "", true, nil, false},
		{"non numeric", "abc", true, nil, false},
		{"float", "1.5", true, nil, false},
		{"exponent", "1e3", true, nil, false},
		{"beyond safe integer", "9007199254740993", true, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := url.Values{}
			if tc.present {
				query.Set("memory", tc.raw)
			}
			got, ok := parseOptionalImageQuery(query, "memory")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("value = %d, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("value = %#v, want %d", got, *tc.want)
			}
		})
	}
}

func TestDecodeSystemImageInput(t *testing.T) {
	input, err := decodeSystemImageInput([]byte(`{"name":"a","remoteAlias":"r","osType":"Linux","architecture":"amd64","instanceType":"vm","icon":"i","sortOrder":3,"hidden":true}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if input.Name == nil || *input.Name != "a" {
		t.Fatalf("name = %#v", input.Name)
	}
	if input.RemoteAlias == nil || *input.RemoteAlias != "r" {
		t.Fatalf("remoteAlias = %#v", input.RemoteAlias)
	}
	if input.OSType == nil || *input.OSType != "Linux" {
		t.Fatalf("osType = %#v", input.OSType)
	}
	if input.Architecture == nil || *input.Architecture != "amd64" {
		t.Fatalf("architecture = %#v", input.Architecture)
	}
	if input.InstanceType == nil || *input.InstanceType != "vm" {
		t.Fatalf("instanceType = %#v", input.InstanceType)
	}
	if input.Icon == nil || *input.Icon != "i" {
		t.Fatalf("icon = %#v", input.Icon)
	}
	if input.SortOrder == nil || *input.SortOrder != 3 {
		t.Fatalf("sortOrder = %#v", input.SortOrder)
	}
	if input.Hidden == nil || !*input.Hidden {
		t.Fatalf("hidden = %#v", input.Hidden)
	}

	// Unknown keys are dropped (the TypeScript destructures only known fields).
	input, err = decodeSystemImageInput([]byte(`{"name":"a","extra":1}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if input.Name == nil || *input.Name != "a" {
		t.Fatalf("name = %#v", input.Name)
	}
	if input.Icon != nil || input.SortOrder != nil || input.Hidden != nil {
		t.Fatalf("unknown keys leaked into %#v", input)
	}

	// An empty object leaves every field absent.
	input, err = decodeSystemImageInput([]byte(`{}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if input.Name != nil || input.RemoteAlias != nil || input.Icon != nil {
		t.Fatalf("empty body produced %#v", input)
	}

	// JSON null for a string field decodes to the empty string, which the POST
	// route then rejects through its `!name` truthiness check.
	input, err = decodeSystemImageInput([]byte(`{"name":null}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if input.Name == nil || *input.Name != "" {
		t.Fatalf("null name = %#v, want pointer to empty string", input.Name)
	}

	// A mistyped known field is a hard failure (Prisma would reject it too).
	if _, err := decodeSystemImageInput([]byte(`{"sortOrder":"3"}`)); err == nil {
		t.Fatal("string sortOrder must fail to decode")
	}
	if _, err := decodeSystemImageInput([]byte(`{"name":123}`)); err == nil {
		t.Fatal("numeric name must fail to decode")
	}
}

func imageIntPtr(value int) *int { return &value }
