package stafflifecycle

// ADR 0119 S5 + S6 review fixes F9 (rune-safe truncate) and F10 (the actor-proof refusals AP001-AP005 map to a
// refusal, not a 500), DB-free.

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTruncate_RuneSafeAndValidUTF8(t *testing.T) {
	for _, c := range []struct {
		name, in string
		n        int
		want     string
	}{
		{"short ascii", "abc", 10, "abc"},
		{"exact", "abcd", 4, "abcd"},
		{"ascii cut", "abcdef", 3, "abc"},
		// "ñ" is 2 bytes: a 3-byte cut of "añb" must not split it.
		{"cut inside a 2-byte rune", "aññ", 2, "a"},
		// a 4-byte rune: every cut inside it falls back to its start.
		{"cut inside a 4-byte rune", "x\U0001F600y", 3, "x"},
		{"invalid utf-8 replaced", "ab\xffcd", 10, "ab?cd"},
		{"invalid utf-8 then cut", "\xff\xfe\xfdabc", 2, "?a"},
	} {
		got := truncate(c.in, c.n)
		if got != c.want || !utf8.ValidString(got) || len(got) > c.n {
			t.Errorf("%s: truncate(%q, %d) = %q, want %q (valid UTF-8, at most n bytes)", c.name, c.in, c.n, got, c.want)
		}
	}
	long := strings.Repeat("é", 300) // 600 bytes
	if got := truncate(long, 255); !utf8.ValidString(got) || len(got) != 254 {
		t.Errorf("long accented input: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestMapDBError_ProofRefusalsAndBusy(t *testing.T) {
	for _, code := range []string{"AP001", "AP002", "AP003", "AP004", "AP005"} {
		if err := mapDBError(&pgconn.PgError{Code: code}); !errors.Is(err, ErrProofRefused) {
			t.Errorf("%s: got %v, want ErrProofRefused", code, err)
		}
	}
	if err := mapDBError(&pgconn.PgError{Code: "55P03"}); !errors.Is(err, ErrBusy) {
		t.Errorf("55P03: got %v, want ErrBusy", err)
	}
	if err := mapDBError(&pgconn.PgError{Code: "SF020"}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("SF020: got %v, want ErrLastAdmin", err)
	}
}
