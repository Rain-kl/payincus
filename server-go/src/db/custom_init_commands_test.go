package db

import (
	"strings"
	"testing"
	"time"
)

// mustParseISOTestTime parses an RFC3339 timestamp for the row fixtures.
func mustParseISOTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// TestCustomInitParseDistros mirrors `JSON.parse(cmd.distros) as string[]`.
func TestCustomInitParseDistros(t *testing.T) {
	distros, err := customInitParseDistros(`["ubuntu","all"]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(distros, ",") != "ubuntu,all" {
		t.Fatalf("distros = %#v", distros)
	}

	empty, err := customInitParseDistros(`[]`)
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty distros = %#v", empty)
	}

	if _, err := customInitParseDistros(`not json`); err == nil {
		t.Fatal("invalid JSON must error (the TypeScript would throw → 500)")
	}
}

// TestCustomInitJSONString mirrors JSON.stringify for the stored distro column.
func TestCustomInitJSONString(t *testing.T) {
	cases := []struct {
		input []string
		want  string
	}{
		{[]string{}, "[]"},
		{[]string{"ubuntu"}, `["ubuntu"]`},
		{[]string{"ubuntu", "all"}, `["ubuntu","all"]`},
		// HTML characters are not escaped (JSON.stringify does not escape them).
		{[]string{"a<b>", "c&d"}, `["a<b>","c&d"]`},
		{[]string{`quote"`}, `["quote\""]`},
	}
	for _, tc := range cases {
		if got := customInitJSONString(tc.input); got != tc.want {
			t.Errorf("jsonString(%#v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestCustomInitNormalizeDescription mirrors `value?.trim() || null`.
func TestCustomInitNormalizeDescription(t *testing.T) {
	if got := customInitNormalizeDescription(nil); got != nil {
		t.Fatalf("nil description = %#v", got)
	}
	empty := ""
	if got := customInitNormalizeDescription(&empty); got != nil {
		t.Fatalf("empty description = %#v, want nil", got)
	}
	blank := "   "
	if got := customInitNormalizeDescription(&blank); got != nil {
		t.Fatalf("blank description = %#v, want nil", got)
	}
	padded := "  note  "
	got := customInitNormalizeDescription(&padded)
	if got == nil || *got != "note" {
		t.Fatalf("padded description = %#v, want \"note\"", got)
	}
}

// TestCustomInitInvalidDistros mirrors the db-layer validation helper.
func TestCustomInitInvalidDistros(t *testing.T) {
	if got := customInitInvalidDistros([]string{"ubuntu", "alpine", "all"}); len(got) != 0 {
		t.Fatalf("valid list flagged: %#v", got)
	}
	got := customInitInvalidDistros([]string{"other", "Ubuntu", "all"})
	if strings.Join(got, ",") != "other,Ubuntu" {
		t.Fatalf("invalid distros = %#v", got)
	}
	// The db and route modules must agree on the write-side list.
	if len(customInitValidDistros) != 7 {
		t.Fatalf("valid distro count = %d, want 7", len(customInitValidDistros))
	}
}

// TestCustomInitLimits pins MAX_COMMANDS_PER_USER and MAX_COMMAND_SIZE.
func TestCustomInitLimits(t *testing.T) {
	if customInitMaxCommandsPerUser != 50 {
		t.Fatalf("max commands = %d, want 50", customInitMaxCommandsPerUser)
	}
	if customInitMaxCommandSize != 64*1024 {
		t.Fatalf("max command size = %d, want 65536", customInitMaxCommandSize)
	}
}

// TestMergeCustomInitCommandContents mirrors mergeCommandContents: joined by a
// single newline, in the supplied order, empty for no commands.
func TestMergeCustomInitCommandContents(t *testing.T) {
	if got := MergeCustomInitCommandContents(nil); got != "" {
		t.Fatalf("empty merge = %q", got)
	}
	got := MergeCustomInitCommandContents([]CustomInitCommandData{
		{ID: 2, Command: "b"},
		{ID: 1, Command: "a"},
	})
	if got != "b\na" {
		t.Fatalf("merge = %q, want %q", got, "b\na")
	}
}

// TestGetImageDistroFromAlias mirrors getImageDistroFromAlias, including the
// order of the checks.
func TestGetImageDistroFromAlias(t *testing.T) {
	cases := []struct {
		alias string
		want  string
	}{
		{"ubuntu/24.04", "ubuntu"},
		{"Ubuntu22", "ubuntu"},
		{"ubuntu:24.04", "ubuntu"},
		{"debian/12", "debian"},
		{"kali-rolling", "debian"},
		{"alpine/3.19", "alpine"},
		{"archlinux", "arch"},
		{"opensuse/leap", "suse"},
		{"tumbleweed", "suse"},
		{"leap", "suse"},
		{"almalinux/9", "rhel"},
		{"rocky/9", "rhel"},
		{"oracle-8", "rhel"},
		{"centos/7", "rhel"},
		{"rhel9", "rhel"},
		{"fedora/40", "rhel"},
		{"windows", "other"},
		{"", "other"},
	}
	for _, tc := range cases {
		if got := GetImageDistroFromAlias(tc.alias); got != tc.want {
			t.Errorf("getImageDistroFromAlias(%q) = %q, want %q", tc.alias, got, tc.want)
		}
	}
}

// TestCustomInitMapRow verifies the wire projection: parsed distros plus
// ISO-8601 millisecond timestamps.
func TestCustomInitMapRow(t *testing.T) {
	description := "note"
	row := customInitCommandRow{
		ID:          7,
		UserID:      3,
		Name:        "init",
		Command:     "apt update",
		Distros:     `["ubuntu","all"]`,
		Description: &description,
		Enabled:     true,
		CreatedAt:   mustParseISOTestTime(t, "2026-01-02T03:04:05Z"),
		UpdatedAt:   mustParseISOTestTime(t, "2026-01-02T03:04:06Z"),
	}

	mapped, err := customInitMapRow(row)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if mapped.ID != 7 || mapped.UserID != 3 || mapped.Name != "init" || mapped.Command != "apt update" {
		t.Fatalf("mapped scalars = %#v", mapped)
	}
	if strings.Join(mapped.Distros, ",") != "ubuntu,all" {
		t.Fatalf("mapped distros = %#v", mapped.Distros)
	}
	if mapped.CreatedAt != "2026-01-02T03:04:05.000Z" || mapped.UpdatedAt != "2026-01-02T03:04:06.000Z" {
		t.Fatalf("mapped timestamps = %q / %q", mapped.CreatedAt, mapped.UpdatedAt)
	}
	if !mapped.Enabled {
		t.Fatal("enabled must round-trip")
	}
}

// TestCustomInitMapRowInvalidDistros documents that a corrupt column is an error
// rather than a silent empty list (the TypeScript JSON.parse throws → 500).
func TestCustomInitMapRowInvalidDistros(t *testing.T) {
	if _, err := customInitMapRow(customInitCommandRow{Distros: `oops`}); err == nil {
		t.Fatal("corrupt distros column must error")
	}
}

// TestCustomInitUpdateDataTriState pins the presence flags that stand in for the
// TypeScript `data.x !== undefined` tests.
func TestCustomInitUpdateDataTriState(t *testing.T) {
	enabled := false
	description := ""

	data := CustomInitCommandUpdateData{
		Distros:        []string{"ubuntu"},
		DistrosSet:     true,
		Description:    &description,
		DescriptionSet: true,
		Enabled:        &enabled,
	}

	// An empty distro list is a *provided* empty list, which the db layer rejects.
	if !data.DistrosSet || len(data.Distros) != 1 {
		t.Fatalf("distros = %#v", data)
	}
	// An empty description is provided-but-null after normalization.
	if !data.DescriptionSet || customInitNormalizeDescription(data.Description) != nil {
		t.Fatal("empty description must normalize to NULL")
	}
	// An explicit false must stay distinguishable from "absent".
	if data.Enabled == nil || *data.Enabled {
		t.Fatal("enabled tri-state lost")
	}
}
