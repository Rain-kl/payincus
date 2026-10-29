package routes

import (
	"strings"
	"testing"
)

// TestValidateTerminalCommandBodyPost pins the POST schema behaviour to what the
// real Fastify server produces (verified against fastify@5 with its default AJV
// options: allErrors:false, removeAdditional:true, coerceTypes:'array').
func TestValidateTerminalCommandBodyPost(t *testing.T) {
	cases := []struct {
		name     string
		body     any
		want     string
		wantName string
	}{
		{"missing everything reports the first required field", map[string]any{}, "name is required", ""},
		{"missing command wins over a bad name", map[string]any{"name": ""}, "command is required", ""},
		{"missing command", map[string]any{"name": "a"}, "command is required", ""},
		{"missing name", map[string]any{"command": "x"}, "name is required", ""},
		{"null name coerces to empty then fails minLength", map[string]any{"name": nil, "command": "b"}, "name is too short", ""},
		{"empty name", map[string]any{"name": "", "command": "b"}, "name is too short", ""},
		{"name over the 80 char limit", map[string]any{"name": strings.Repeat("x", 81), "command": "b"}, "name is too long", ""},
		{"name of exactly 80 chars is accepted", map[string]any{"name": strings.Repeat("x", 80), "command": "b"}, "", strings.Repeat("x", 80)},
		{"number name is stringified", map[string]any{"name": float64(123), "command": "b"}, "", "123"},
		{"boolean name is stringified", map[string]any{"name": true, "command": "b"}, "", "true"},
		{"single-element array unwraps", map[string]any{"name": []any{"x"}, "command": "b"}, "", "x"},
		{"multi-element array is not a string", map[string]any{"name": []any{"x", "y"}, "command": "b"}, "must be string", ""},
		{"object name is not a string", map[string]any{"name": map[string]any{"a": 1}, "command": "b"}, "must be string", ""},
		{"unknown keys are removed, not rejected", map[string]any{"name": "a", "command": "b", "extra": float64(1)}, "", "a"},
		{"non-object body", []any{float64(1)}, "must be object", ""},
		{"null body", nil, "must be object", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, message := validateTerminalCommandBody(tc.body, true)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
			if tc.want != "" {
				return
			}
			if body.Name == nil || *body.Name != tc.wantName {
				t.Fatalf("name = %#v, want %q", body.Name, tc.wantName)
			}
		})
	}
}

func TestValidateTerminalCommandBodyDescription(t *testing.T) {
	body, message := validateTerminalCommandBody(map[string]any{
		"name": "a", "command": "b", "description": float64(123),
	}, true)
	if message != "" {
		t.Fatalf("number description must be coerced, got %q", message)
	}
	if body.Description == nil || *body.Description != "123" {
		t.Fatalf("description = %#v, want \"123\"", body.Description)
	}

	body, message = validateTerminalCommandBody(map[string]any{
		"name": "a", "command": "b", "description": nil,
	}, true)
	if message != "" {
		t.Fatalf("explicit null description must be accepted, got %q", message)
	}
	if !body.HasDescription || body.Description != nil {
		t.Fatalf("null description = %#v (present=%v), want present+nil", body.Description, body.HasDescription)
	}

	_, message = validateTerminalCommandBody(map[string]any{
		"name": "a", "command": "b", "description": map[string]any{},
	}, true)
	if message != "must be string,null" {
		t.Fatalf("object description message = %q", message)
	}

	_, message = validateTerminalCommandBody(map[string]any{
		"name": "a", "command": "b", "description": strings.Repeat("d", 301),
	}, true)
	if message != "description is too long" {
		t.Fatalf("long description message = %q", message)
	}
}

// TestValidateTerminalCommandBodyPut covers the PUT schema, which has no
// `required` list and therefore accepts an empty object.
func TestValidateTerminalCommandBodyPut(t *testing.T) {
	body, message := validateTerminalCommandBody(map[string]any{}, false)
	if message != "" {
		t.Fatalf("empty PUT body must be valid, got %q", message)
	}
	if body.Name != nil || body.Command != nil || body.HasDescription {
		t.Fatalf("empty PUT body produced %#v", body)
	}

	_, message = validateTerminalCommandBody(map[string]any{"foo": float64(1)}, false)
	if message != "" {
		t.Fatalf("unknown PUT keys must be removed, got %q", message)
	}

	_, message = validateTerminalCommandBody(map[string]any{"name": ""}, false)
	if message != "name is too short" {
		t.Fatalf("empty PUT name message = %q", message)
	}

	_, message = validateTerminalCommandBody(map[string]any{"command": strings.Repeat("x", 16385)}, false)
	if message != "command is too long" {
		t.Fatalf("long PUT command message = %q", message)
	}

	body, message = validateTerminalCommandBody(map[string]any{"description": nil}, false)
	if message != "" || !body.HasDescription || body.Description != nil {
		t.Fatalf("PUT description null = %#v / %q", body, message)
	}
}

func TestFormatTerminalCommandNumber(t *testing.T) {
	cases := []struct {
		input float64
		want  string
	}{
		{0, "0"},
		{123, "123"},
		{1.5, "1.5"},
		{-7, "-7"},
	}

	for _, tc := range cases {
		if got := formatTerminalCommandNumber(tc.input); got != tc.want {
			t.Errorf("formatTerminalCommandNumber(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestRespondTerminalCommandErrorFallback(t *testing.T) {
	target, ok := terminalCommandErrorMap["NOT_FOUND"]
	if !ok || target.Status != 404 || target.Message != "Saved command not found" {
		t.Fatalf("NOT_FOUND mapping = %#v (present=%v)", target, ok)
	}
	target, ok = terminalCommandErrorMap["MAX_COMMANDS_REACHED"]
	if !ok || target.Status != 400 || target.Message != "Maximum saved terminal commands reached (100)" {
		t.Fatalf("MAX_COMMANDS_REACHED mapping = %#v (present=%v)", target, ok)
	}
	if _, ok := terminalCommandErrorMap["SOMETHING_ELSE"]; ok {
		t.Fatal("unknown codes must fall back to the 400/echo behaviour")
	}
}
