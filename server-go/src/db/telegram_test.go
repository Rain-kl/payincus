// Unit tests for the Telegram-binding persistence helpers. Only the pure pieces
// are covered here (LIKE escaping and the admin search clause builder); the SQL
// itself is exercised by the dual-run conformance harness.
package db

import (
	"errors"
	"strings"
	"testing"
)

// TestTelegramEscapeLike pins Prisma's LIKE-pattern escaping for `contains`:
// backslash, percent and underscore become literals; everything else passes
// through, including multi-byte runes.
func TestTelegramEscapeLike(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "alice", "alice"},
		{"percent", "100%", `100\%`},
		{"underscore", "a_b", `a\_b`},
		{"backslash", `a\b`, `a\\b`},
		{"all three", `\%_`, `\\\%\_`},
		{"empty", "", ""},
		{"unicode untouched", "张三", "张三"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := telegramEscapeLike(tc.input); got != tc.want {
				t.Fatalf("telegramEscapeLike(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestTelegramAllDigits pins /^\d+$/: non-empty, ASCII digits only.
func TestTelegramAllDigits(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     true,
		"12345": true,
		"01":    true,
		" 1":    false,
		"1a":    false,
		"-1":    false,
		"1.0":   false,
		"١٢٣":   false, // Arabic-Indic digits are not \d in JS
	}
	for input, want := range cases {
		if got := telegramAllDigits(input); got != want {
			t.Fatalf("telegramAllDigits(%q) = %v, want %v", input, got, want)
		}
	}
}

// TestTelegramBindingSearchConditions pins the `where.OR` array: six `contains`
// clauses (in source order) plus the id/userId equality pair for digit terms.
func TestTelegramBindingSearchConditions(t *testing.T) {
	t.Run("text search", func(t *testing.T) {
		clauses, args, err := telegramBindingSearchConditions("a_b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{
			"b.telegram_user_id LIKE ?",
			"b.telegram_username LIKE ?",
			"b.first_name LIKE ?",
			"b.last_name LIKE ?",
			"u.username LIKE ?",
			"u.email LIKE ?",
		}
		if strings.Join(clauses, "|") != strings.Join(want, "|") {
			t.Fatalf("clauses = %v, want %v", clauses, want)
		}
		if len(args) != 6 {
			t.Fatalf("args = %v, want 6 entries", args)
		}
		for _, arg := range args {
			if arg != `%a\_b%` {
				t.Fatalf("arg = %v, want escaped pattern", arg)
			}
		}
	})

	t.Run("numeric search adds equality clauses", func(t *testing.T) {
		clauses, args, err := telegramBindingSearchConditions("42")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(clauses) != 8 {
			t.Fatalf("clauses = %v, want 8 entries", clauses)
		}
		if clauses[6] != "b.id = ?" || clauses[7] != "b.user_id = ?" {
			t.Fatalf("trailing clauses = %v, want id/user_id equality", clauses[6:])
		}
		if args[6] != int64(42) || args[7] != int64(42) {
			t.Fatalf("trailing args = %v, want 42/42", args[6:])
		}
	})

	t.Run("unsafe integer keeps only the contains clauses", func(t *testing.T) {
		// 99999999999999999999 overflows int64; the JS value is 1e20, which is not
		// a safe integer, so Number.isSafeInteger is false and the clauses are not
		// added (no error either).
		clauses, args, err := telegramBindingSearchConditions("99999999999999999999")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(clauses) != 6 || len(args) != 6 {
			t.Fatalf("clauses = %v, want the six contains clauses only", clauses)
		}
	})

	t.Run("safe integer beyond int32 is an error", func(t *testing.T) {
		// 2147483648 is a safe integer but does not fit Prisma's Int column: the
		// TypeScript lets Prisma throw, which the route renders as a 500.
		_, _, err := telegramBindingSearchConditions("2147483648")
		if !errors.Is(err, ErrTelegramSearchIDOutOfRange) {
			t.Fatalf("err = %v, want ErrTelegramSearchIDOutOfRange", err)
		}
	})

	t.Run("int32 max is accepted", func(t *testing.T) {
		clauses, _, err := telegramBindingSearchConditions("2147483647")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(clauses) != 8 {
			t.Fatalf("clauses = %v, want 8 entries", clauses)
		}
	})
}

// TestTelegramBindingRowToBinding pins the relation assembly, including the
// "missing join row degrades to a nil user" guard.
func TestTelegramBindingRowToBinding(t *testing.T) {
	id := int32(7)
	username := "alice"
	status := "active"
	email := "a@example.com"

	row := telegramBindingJoinedRow{
		ID:             1,
		UserID:         7,
		TelegramUserID: "123",
		JoinedUserID:   &id,
		JoinedUsername: &username,
		JoinedEmail:    &email,
		JoinedStatus:   &status,
	}
	binding := row.toBinding()
	if binding.User == nil {
		t.Fatal("expected a user relation")
	}
	if binding.User.ID != 7 || binding.User.Username != "alice" || binding.User.Status != "active" {
		t.Fatalf("unexpected user: %+v", binding.User)
	}
	if binding.User.Email == nil || *binding.User.Email != "a@example.com" {
		t.Fatalf("unexpected email: %v", binding.User.Email)
	}

	empty := telegramBindingJoinedRow{ID: 2, UserID: 8, TelegramUserID: "456"}
	if empty.toBinding().User != nil {
		t.Fatal("expected a nil user relation when the join is missing")
	}
}
