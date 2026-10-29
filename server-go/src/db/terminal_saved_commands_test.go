package db

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeTerminalCommandLabel(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"  ls -la  ", "ls -la"},
		{"a\r\nb\tc", "a b c"},
		{"a\x00b\x1fc\x7f", "abc"},
		{"\n\n", ""},
		{"中文命令", "中文命令"},
	}

	for _, tc := range cases {
		if got := normalizeTerminalCommandLabel(tc.input); got != tc.want {
			t.Errorf("normalizeTerminalCommandLabel(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizeTerminalDescription(t *testing.T) {
	empty := ""
	blank := "   "
	crlf := "line1\r\nline2"
	controls := "a\x00b"
	spaces := "  keep  "

	if got := normalizeTerminalDescription(nil); got != nil {
		t.Errorf("nil description = %#v, want nil", got)
	}
	if got := normalizeTerminalDescription(&empty); got != nil {
		t.Errorf("empty description = %#v, want nil", got)
	}
	if got := normalizeTerminalDescription(&blank); got != nil {
		t.Errorf("whitespace-only description = %#v, want nil", got)
	}
	// The \r\n → \n replacement is immediately undone by the control-character
	// strip (\n is \u000A), exactly like the TypeScript.
	if got := normalizeTerminalDescription(&crlf); got == nil || *got != "line1line2" {
		t.Errorf("CRLF description = %#v, want \"line1line2\"", got)
	}
	if got := normalizeTerminalDescription(&controls); got == nil || *got != "ab" {
		t.Errorf("control-char description = %#v, want \"ab\"", got)
	}
	if got := normalizeTerminalDescription(&spaces); got == nil || *got != "keep" {
		t.Errorf("description = %#v, want \"keep\"", got)
	}
}

func TestIsEncryptedSensitiveData(t *testing.T) {
	hex32 := strings.Repeat("a", 32)
	cases := []struct {
		input string
		want  bool
	}{
		{hex32 + ":" + hex32 + ":" + "deadbeef", true},
		{hex32 + ":" + hex32, false},
		{hex32 + ":" + hex32 + ":" + "aa" + ":" + "bb", false},
		{strings.Repeat("a", 31) + ":" + hex32 + ":cc", false},
		{hex32 + ":" + strings.Repeat("b", 31) + ":cc", false},
		{"plain text command", false},
		{"", false},
	}

	for _, tc := range cases {
		if got := isEncryptedSensitiveData(tc.input); got != tc.want {
			t.Errorf("isEncryptedSensitiveData(%.40q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestMapTerminalSavedCommand(t *testing.T) {
	description := "some description"
	createdAt := time.Date(2026, 10, 29, 12, 34, 56, 789000000, time.UTC)
	updatedAt := time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC)

	mapped := mapTerminalSavedCommand(terminalSavedCommandRow{
		ID:          7,
		UserID:      3,
		Name:        "deploy",
		Command:     "make deploy",
		Description: &description,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	})

	if mapped.ID != 7 || mapped.UserID != 3 {
		t.Errorf("ids = %d/%d, want 7/3", mapped.ID, mapped.UserID)
	}
	if mapped.Name != "deploy" || mapped.Command != "make deploy" {
		t.Errorf("plaintext values were not preserved: %#v", mapped)
	}
	if mapped.Description == nil || *mapped.Description != description {
		t.Errorf("description = %#v, want %q", mapped.Description, description)
	}
	if mapped.CreatedAt != "2026-10-29T12:34:56.789Z" {
		t.Errorf("createdAt = %q, want Date#toISOString() form", mapped.CreatedAt)
	}
	if mapped.UpdatedAt != "2026-10-29T13:00:00.000Z" {
		t.Errorf("updatedAt = %q, want Date#toISOString() form", mapped.UpdatedAt)
	}

	withoutDescription := mapTerminalSavedCommand(terminalSavedCommandRow{
		ID:        8,
		UserID:    3,
		Name:      "x",
		Command:   "y",
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	})
	if withoutDescription.Description != nil {
		t.Errorf("NULL description = %#v, want nil", withoutDescription.Description)
	}
}

func TestTerminalSavedCommandConstants(t *testing.T) {
	if maxTerminalCommandsPerUser != 100 {
		t.Errorf("maxTerminalCommandsPerUser = %d, want 100", maxTerminalCommandsPerUser)
	}
	if maxTerminalCommandSize != 16*1024 {
		t.Errorf("maxTerminalCommandSize = %d, want %d", maxTerminalCommandSize, 16*1024)
	}
	if terminalSavedCommandLockNamespace != 4124 {
		t.Errorf("terminalSavedCommandLockNamespace = %d, want 4124", terminalSavedCommandLockNamespace)
	}
}
