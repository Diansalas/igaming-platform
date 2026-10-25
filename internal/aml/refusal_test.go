package aml

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// lic29: the closed refusal codes. The database raises them as HINT; classification keeps the sentinel (so the HTTP status is unchanged)
// and adds the code only if it is in the closed table.
func TestLic29_RefusalCode_ClassificationKeepsTheStatusClassAndAddsTheCode(t *testing.T) {
	cases := []struct {
		sqlstate string
		hint     string
		is       error
	}{
		{"AT004", "screening_worker_heartbeat_missing", ErrInvalidInput},
		{"AT004", "gate_environment_refused", ErrInvalidInput},
		{"AM006", "sanctions_clear_refused", ErrHoldRefused},
		{"AM006", "sanctions_match_needs_hold", ErrHoldRefused},
		{"AT003", "request_lease_live", ErrIllegalTransition},
		{"AT003", "policy_loosening_refused", ErrIllegalTransition},
		{"AM002", "platform_officer_required", ErrForbidden},
	}
	for _, c := range cases {
		err := classify(&pgconn.PgError{Code: c.sqlstate, Message: "text that must not be copied", Hint: c.hint})
		if !errors.Is(err, c.is) {
			t.Errorf("%s/%s: lost the sentinel class: %v", c.sqlstate, c.hint, err)
		}
		if RefusalCode(err) != c.hint {
			t.Errorf("%s/%s: code %q", c.sqlstate, c.hint, RefusalCode(err))
		}
	}
	// A hint outside the closed table is ignored; so is no hint.
	for _, h := range []string{"", "made_up", "SCREENING_WORKER_HEARTBEAT_MISSING"} {
		err := classify(&pgconn.PgError{Code: "AT004", Message: "x", Hint: h})
		if RefusalCode(err) != "" || !errors.Is(err, ErrInvalidInput) {
			t.Errorf("hint %q: code %q err %v", h, RefusalCode(err), err)
		}
	}
	if RefusalCode(errors.New("x")) != "" || classify(nil) != nil {
		t.Error("no code on a plain error; nil stays nil")
	}
}

// The closed vocabulary and the migrations agree both ways: no HINT in a screening migration is outside the table, and every code in
// the table is raised by some migration.
func TestLic29_RefusalCodes_MatchTheMigrations(t *testing.T) {
	re := regexp.MustCompile(`HINT = '([a-z0-9_]+)'`)
	raised := map[string]bool{}
	for _, name := range []string{"0181_lic10_residence_maker_checker_and_aml_triggers.up.sql", "0189_lic29_screening_policy_tighten_only_author.up.sql",
		"0190_c4_screening_policy_governed_loosening.up.sql", "0191_d1_screening_break_glass_and_trigger_loosening.up.sql"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if _, ok := RefusalCodes[m[1]]; !ok {
				t.Errorf("%s raises HINT %q which is not in the closed table", name, m[1])
			}
			raised[m[1]] = true
		}
	}
	for code, desc := range RefusalCodes {
		if !raised[code] {
			t.Errorf("code %q is in the table but no migration raises it", code)
		}
		if !regexp.MustCompile(`^[a-z0-9_]{1,64}$`).MatchString(code) || strings.TrimSpace(desc) == "" {
			t.Errorf("code %q / description malformed", code)
		}
	}
}
