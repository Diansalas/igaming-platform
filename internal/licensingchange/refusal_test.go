package licensingchange

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The table is closed and has no dead entry: every code is raised by a precheck branch (refuse("code", ...)), by the guard map, by
// a domain sentinel in ClosedRefusal, or by the jurisdiction create route; and every such source names a code in the table.
func TestClosedRefusals_ClosedAndEveryCodeIsRaised(t *testing.T) {
	raised := map[string]string{}
	re := regexp.MustCompile(`refuse\("([a-z_]+)"`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "refusal.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			raised[m[1]] = "precheck " + f
		}
	}
	if len(raised) < 15 {
		t.Fatalf("found only %d precheck refusal sites: the scan is vacuous", len(raised))
	}
	for sqlstate, code := range guardRefusals {
		raised[code] = "guard " + sqlstate
	}
	for _, code := range []string{"currency_not_active_asset", "policy_exceeds_permitted_scope", "policy_close_needs_successor"} {
		raised[code] = "sentinel"
	}
	j, err := os.ReadFile("../httpserver/jurisdiction_admin_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(j), `[]string{"duplicate_jurisdiction_code"}`) {
		t.Fatal("the jurisdiction create route no longer raises duplicate_jurisdiction_code")
	}
	raised["duplicate_jurisdiction_code"] = "route"

	if got, want := keys(raised), keys(ClosedRefusals); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("raised codes != table\nraised: %v\ntable:  %v", got, want)
	}
	for c, d := range ClosedRefusals {
		if !regexp.MustCompile(`^[a-z][a-z_]{2,63}$`).MatchString(c) {
			t.Errorf("code %q is not a lower-case snake token", c)
		}
		if len(d) < 15 {
			t.Errorf("code %q has no description", c)
		}
		// wording never leaks database internals
		if regexp.MustCompile(`(?i)LC\d{3}|AP\d{3}|SQLSTATE|trigger|migration|constraint|pg_|\buuid\b|[0-9a-f]{8}-[0-9a-f]{4}`).MatchString(d) {
			t.Errorf("code %q: the description mentions database internals: %q", c, d)
		}
	}
}

func TestRefuse_UnknownCodePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("refuse must panic on a code outside the closed table")
		}
	}()
	refuse("made_up", errors.New("x"))
}

// classify keeps its sentinels, and the guard errors that reach the HTTP layer carry the closed code of their SQLSTATE.
func TestClassify_MapsGuardsToClosedCodesAndKeepsSentinels(t *testing.T) {
	for state, want := range guardRefusals {
		err := classify(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: state, Message: "secret detail of table licences row 7"}))
		if GuardCode(err) != state {
			t.Errorf("%s: classify lost the guard: %v", state, err)
		}
		if got := ClosedRefusal(err); got != want {
			t.Errorf("%s: code %q want %q", state, got, want)
		}
		if _, ok := ClosedRefusals[want]; !ok {
			t.Errorf("%s maps to %q which is not in the table", state, want)
		}
	}
	// sentinels first, no refusal code (they have their own API codes)
	for state, sentinel := range map[string]error{"LC022": ErrSelfApproval, "LC032": ErrSelfApproval, "LC013": ErrNotPending, "LC031": ErrNotPending} {
		err := classify(&pgconn.PgError{Code: state, Message: "m"})
		if !errors.Is(err, sentinel) {
			t.Errorf("%s: sentinel lost: %v", state, err)
		}
		if ClosedRefusal(err) != "" {
			t.Errorf("%s: a sentinel-class refusal must carry no refusal code, got %q", state, ClosedRefusal(err))
		}
	}
	// the actor-proof class and migration-only states carry none
	for _, state := range []string{"AP001", "AP004", "LC095", "LC098"} {
		if c := ClosedRefusal(classify(&pgconn.PgError{Code: state})); c != "" {
			t.Errorf("%s: unexpected code %q", state, c)
		}
	}
	// lock waits and non-database errors stay as they were
	if !errors.Is(classify(&pgconn.PgError{Code: "55P03"}), ErrRetryableLockTimeout) {
		t.Error("lock timeout sentinel lost")
	}
	if ClosedRefusal(errors.New("boom")) != "" || ClosedRefusal(nil) != "" || ClosedRefusal(ErrForbidden) != "" || ClosedRefusal(ErrInvalidInput) != "" {
		t.Error("authorisation, input and unknown errors carry no refusal code")
	}
}

func TestClosedRefusal_PrecheckSentinelsAndPrecedence(t *testing.T) {
	d := refuse("ceiling_not_enabled_for_country", fmt.Errorf("%w: x", operatingmarket.ErrCeilingExceeded))
	if ClosedRefusal(fmt.Errorf("outer: %w", d)) != "ceiling_not_enabled_for_country" {
		t.Fatal("an explicit precheck code wins over the sentinel-derived one and survives wrapping")
	}
	// execute-time sentinels without an explicit code
	cases := map[string]error{
		"policy_exceeds_permitted_scope": &DomainError{Err: fmt.Errorf("%w: x", operatingmarket.ErrCeilingExceeded)},
		"policy_close_needs_successor":   &DomainError{Err: fmt.Errorf("%w: x", operatingmarket.ErrPolicyCloseRequiresSuccessor)},
		"currency_not_active_asset":      &DomainError{Err: fmt.Errorf("%w: x%w", jurisdiction.ErrInvalidInput, jurisdiction.ErrInactiveAsset)},
	}
	for want, err := range cases {
		if got := ClosedRefusal(err); got != want {
			t.Errorf("%v: %q want %q", err, got, want)
		}
	}
	// a generic domain refusal stays uncoded
	if c := ClosedRefusal(&DomainError{Err: jurisdiction.ErrInvalidInput}); c != "" {
		t.Errorf("generic domain refusal got %q", c)
	}
	// the duplicate-code error is an ErrInvalidInput (status unchanged) with the marker
	dup := fmt.Errorf("%w: jurisdiction code %q already exists%w", jurisdiction.ErrInvalidInput, "X", jurisdiction.ErrDuplicateJurisdictionCode)
	if !errors.Is(dup, jurisdiction.ErrInvalidInput) || !errors.Is(dup, jurisdiction.ErrDuplicateJurisdictionCode) {
		t.Error("duplicate marker")
	}
	if errors.Is(jurisdiction.ErrInvalidInput, jurisdiction.ErrDuplicateJurisdictionCode) || errors.Is(jurisdiction.ErrInvalidInput, jurisdiction.ErrInactiveAsset) {
		t.Error("the markers must not match the plain sentinel")
	}
}
