package rg

// ADR 0115 S2 review follow-ups (security F2-F4, ledger-finance C2, signer pin): DB-free tests.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// F2: the staff key namespace separator is a character the player key syntax can never produce.
func TestKeyNamespaceSeparatorIsUnreachableByAClientKey(t *testing.T) {
	for _, k := range []string{"staff x", "staff:x", " x", "a b", "staff\tx", "staff\x00x"} {
		if k == "staff:x" {
			continue // a colon is a legal client character; only the space-separated form is the staff namespace
		}
		if keyRe.MatchString(k) {
			t.Errorf("%q must not be a valid client key", k)
		}
	}
	if !keyRe.MatchString("staff:x") {
		t.Fatal("sanity: colon keys stay legal for clients")
	}
	// the stored staff key is "staff " + clientKey, 6+120 <= 128 (the column bound)
	if len("staff ")+120 > 128 {
		t.Fatal("the staff-namespaced key must fit idempotency_key's 128-byte bound")
	}
}

// F4: free-text request fields are printable ASCII or refused as 400 (never a database 500).
func TestFreeTextFieldsAreValidated(t *testing.T) {
	c := baseContent(ControlWager)
	c.AssetCode = sp("EUR")
	for _, bad := range []string{"a\x00b", "café", "tab\there", "line\nbreak"} {
		x := c
		x.ReasonCode = bad
		wantInvalid(t, x.Validate(), "reason "+bad)
	}
	ok := c
	ok.ReasonCode = "printable-reason_01"
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"ok text", "a-b_c.1"} {
		if !printableASCII(s) {
			t.Errorf("%q is printable", s)
		}
	}
	for _, s := range []string{"a\x00", "\x7f", "café", "a\n"} {
		if printableASCII(s) {
			t.Errorf("%q is not printable", s)
		}
	}
	if !printableASCII("") {
		t.Error("the empty string has no non-printable byte (emptiness is checked separately)")
	}
	for _, code := range []string{"22021", "22P05"} {
		r, ok := IsRefusal(MapDBError(&pgconn.PgError{Code: code}))
		if !ok || r.Code != RefusedInvalid {
			t.Errorf("SQLSTATE %s must be a 400-class refusal, got %v", code, r)
		}
	}
}

// C2: large values round-trip into the audit view exactly (no float64).
func TestConfigAuditViewKeepsLargeNumbersExact(t *testing.T) {
	big := int64(9007199254740993) // 2^53 + 1: not representable as a float64
	ceil := strings.Repeat("9", 38)
	row := ConfigRow{ID: uuid.New(), EffectiveFrom: time.Unix(1_800_000_000, 0), Content: ConfigContent{
		ControlKind: ControlDeposit, ReasonCode: "r", AttestationRef: "a",
		Ceiling:                 &Attested[string]{Value: &ceil},
		IncreaseCooldownSeconds: &big,
	}}
	b, err := json.Marshal(configAuditView(row))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"increase_cooldown_seconds":9007199254740993`) || !strings.Contains(string(b), ceil) {
		t.Fatalf("audit view lost precision: %s", b)
	}
}

// Signer pin: internal/rg signs proofs at exactly one call site (limits_config.go attach), nowhere else.
func TestSignerCallSitesInRGArePinned(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	needle := regexp.MustCompile(`\.(Attach|Sign)\(`)
	total := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n := len(needle.FindAll(raw, -1))
		if n > 0 && f != "limits_config.go" {
			t.Errorf("%s calls the proof signer %d time(s); only limits_config.go may", f, n)
		}
		if f == "limits_config.go" && n != 1 {
			t.Errorf("limits_config.go must contain exactly one signer call (attach), found %d", n)
		}
		total += n
	}
	if total != 1 {
		t.Fatalf("expected exactly one signer call site in internal/rg, found %d", total)
	}
	// the one call site is inside attach, whose operations are the closed rg_limit_config:* set
	raw, _ := os.ReadFile("limits_config.go")
	if !strings.Contains(string(raw), "func (l *Limits) attach(") {
		t.Fatal("the signer call must live in (*Limits).attach")
	}
}
