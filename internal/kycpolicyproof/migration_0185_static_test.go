package kycpolicyproof_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	. "github.com/Diansalas/igaming-platform/internal/kycpolicyproof"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func between(t *testing.T, src, from, to string) string {
	t.Helper()
	i := strings.Index(src, from)
	if i < 0 {
		t.Fatalf("%q not found", from)
	}
	j := strings.Index(src[i:], to)
	if j < 0 {
		t.Fatalf("%q not found after %q", to, from)
	}
	return src[i : i+j+len(to)]
}

const (
	verifierHead  = "CREATE OR REPLACE FUNCTION actor_proof_require("
	verifierTail  = "$$ LANGUAGE plpgsql SECURITY DEFINER\n    SET search_path = pg_catalog, public, pg_temp;"
	lifecycleHead = "CREATE OR REPLACE FUNCTION kyc_enforcement_policies_enforce_lifecycle()"
	lifecycleTail = "$$ LANGUAGE plpgsql;"
	// sha256 of the 0182 verifier body and of the 0103 lifecycle function (the text the 0185 down must restore byte for byte).
	sha0182Verifier  = "b9263d6a8dc58e8ebad233efc0a50b93bbc703317ceb2e974135965b1b23f144"
	sha0103Lifecycle = "d2624e02449fe0d257d554022a54c280c6554cce7d80cddb793693793d7e8c6a"
)

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// The verifier in 0185 is the CURRENT body (0182) plus one marked block and one allow-list term; the down restores 0182's body
// byte for byte. No later migration may replace the verifier from an older base: the latest definer must be 0185.
func TestMigration0185_VerifierIsTheCurrentBodyPlusOneBlock(t *testing.T) {
	m0182 := repoFile(t, "migrations/0182_withdrawal_restriction_policy.up.sql")
	up := repoFile(t, "migrations/0185_kyc_enforcement_policy_signed_four_eyes.up.sql")
	down := repoFile(t, "migrations/0185_kyc_enforcement_policy_signed_four_eyes.down.sql")
	v0182 := between(t, m0182, verifierHead, verifierTail)
	vUp := between(t, up, verifierHead, verifierTail)
	vDown := between(t, down, verifierHead, verifierTail)
	if sum(v0182) != sha0182Verifier {
		t.Fatal("the 0182 verifier body changed: re-base the 0185 verifier on the current body")
	}
	if vDown != v0182 || sum(vDown) != sha0182Verifier {
		t.Fatal("the 0185 down must restore the 0182 verifier body byte for byte")
	}
	block := between(t, vUp, "    -- 0185 (lic19, S4): kyc_enforcement_policy:*", "    END IF;\n    END IF;\n")
	stripped := strings.Replace(vUp, block, "", 1)
	stripped = strings.Replace(stripped, "\n                                         OR p_operation LIKE 'kyc\\_enforcement\\_policy:%') THEN", ") THEN", 1)
	if stripped != v0182 {
		t.Fatal("the 0185 verifier must equal the 0182 body plus the marked block and the allow-list term only")
	}
	if !strings.Contains(block, "'kyc_enforcement_policy:create'") || !strings.Contains(block, "'kyc_enforcement_policy:supersede'") || !strings.Contains(block, "p_scope <> 'platform'") {
		t.Fatal("the block must close the operation set and admit scope platform only")
	}
	// No other migration replaces the verifier after 0185 (a future replacement must be based on 0185's body).
	latest := ""
	ents, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	sort.Strings(ents)
	for _, f := range ents {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), verifierHead) {
			latest = filepath.Base(f)
		}
	}
	if !strings.HasPrefix(latest, "0185_") {
		t.Fatalf("the latest migration defining actor_proof_require is %s, expected 0185: re-base this pin and the verifier on it", latest)
	}
}

func TestMigration0185_LifecycleIsThe0103BodyPlusOneBlock(t *testing.T) {
	m0103 := repoFile(t, "migrations/0103_kyc_enforcement_policy_supersession.up.sql")
	up := repoFile(t, "migrations/0185_kyc_enforcement_policy_signed_four_eyes.up.sql")
	down := repoFile(t, "migrations/0185_kyc_enforcement_policy_signed_four_eyes.down.sql")
	l0103 := between(t, m0103, lifecycleHead, lifecycleTail)
	lUp := between(t, up, lifecycleHead, lifecycleTail)
	lDown := between(t, down, lifecycleHead, lifecycleTail)
	if sum(l0103) != sha0103Lifecycle {
		t.Fatal("the 0103 lifecycle function changed")
	}
	if lDown != l0103 {
		t.Fatal("the 0185 down must restore the 0103 lifecycle function byte for byte")
	}
	decl := between(t, lUp, "    -- 0185 (lic19) BEGIN\n    v_actor", "    -- 0185 (lic19) END\n")
	blk := between(t, lUp, "    -- 0185 (lic19) BEGIN. S4", "    -- 0185 (lic19) END\n")
	s := strings.Replace(lUp, decl, "", 1)
	s = strings.Replace(s, blk, "", 1)
	if s != l0103 {
		t.Fatal("the 0185 lifecycle function must equal the 0103 body plus the two marked regions only")
	}
	// The block verifies the proof, requires a Person on both sides and distinct Persons, and consumes the reason.
	for _, want := range []string{"public.financial_actor_session()", "v_actor.person_id IS NULL", "v_author_person IS NULL", "v_author_person = v_actor.person_id",
		"public.actor_proof_require(v_actor.actor, 'platform', NULL, v_op, OLD.id::text, v_hash)", "set_config('app.kyc_policy_reason_code', '', true)",
		"'kyc_enforcement_policy:activate'", "'kyc_enforcement_policy:withdraw'", "'kyc_enforcement_policy:supersede'"} {
		if !strings.Contains(blk, want) {
			t.Errorf("the lifecycle block lacks %q", want)
		}
	}
	// The creation trigger is AFTER INSERT (the RLS and CHECK refusals keep their own errors) and is dropped by the down.
	if !regexp.MustCompile(`(?s)CREATE TRIGGER kyc_enforcement_policies_require_create_proof\s+AFTER INSERT ON kyc_enforcement_policies`).MatchString(up) {
		t.Fatal("the creation proof trigger must be AFTER INSERT")
	}
	if !strings.Contains(down, "DROP TRIGGER kyc_enforcement_policies_require_create_proof") {
		t.Fatal("the down must drop the creation proof trigger")
	}
	code := regexp.MustCompile(`(?m)^\s*--.*$`).ReplaceAllString(up, "")
	if strings.Count(code, "SECURITY DEFINER") != 1 {
		t.Fatalf("only the replaced verifier may be SECURITY DEFINER, found %d", strings.Count(code, "SECURITY DEFINER"))
	}
	// Ships no policy row.
	if regexp.MustCompile(`(?i)INSERT\s+INTO\s+kyc_enforcement_policies`).MatchString(code) {
		t.Fatal("0185 must ship no policy row")
	}
}

// The Go signer and the SQL agree on the operation names and on the transition-to-operation mapping.
func TestTransitionOperationMapping(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{"draft", "active", actorproof.OpKYCPolicyActivate},
		{"draft", "withdrawn", actorproof.OpKYCPolicyWithdraw},
		{"active", "withdrawn", actorproof.OpKYCPolicySupersede},
	} {
		got, err := TransitionOperation(c.from, c.to)
		if err != nil || got != c.want {
			t.Errorf("%s -> %s: %q %v, want %q", c.from, c.to, got, err, c.want)
		}
	}
	for _, c := range [][2]string{{"active", "draft"}, {"withdrawn", "active"}, {"draft", "draft"}} {
		if _, err := TransitionOperation(c[0], c[1]); err == nil {
			t.Errorf("%v must have no proof operation", c)
		}
	}
	up := repoFile(t, "migrations/0185_kyc_enforcement_policy_signed_four_eyes.up.sql")
	for _, op := range []string{actorproof.OpKYCPolicyCreate, actorproof.OpKYCPolicyActivate, actorproof.OpKYCPolicyWithdraw, actorproof.OpKYCPolicySupersede} {
		if !strings.Contains(up, "'"+op+"'") {
			t.Errorf("%s is not named in migration 0185", op)
		}
	}
}

// The hashes are bound to every field: changing any one changes the digest (so a proof for another row or reason is useless).
func TestHashesBindEveryField(t *testing.T) {
	th, as := "100", "EUR"
	base := PolicyContent{LicensingJurisdictionID: uuid.New(), TriggerType: "cumulative_deposit", ThresholdMinorUnits: &th, AssetCode: &as, LegalReviewReference: "R", ReasonCode: "c"}
	h := CreateHash(base)
	th2 := "101"
	for name, mut := range map[string]func(c *PolicyContent){
		"jurisdiction": func(c *PolicyContent) { c.LicensingJurisdictionID = uuid.New() },
		"trigger":      func(c *PolicyContent) { c.TriggerType = "play" },
		"threshold":    func(c *PolicyContent) { c.ThresholdMinorUnits = &th2 },
		"asset":        func(c *PolicyContent) { c.AssetCode = nil },
		"legal ref":    func(c *PolicyContent) { c.LegalReviewReference = "" },
		"reason":       func(c *PolicyContent) { c.ReasonCode = "d" },
	} {
		c := base
		mut(&c)
		if CreateHash(c) == h {
			t.Errorf("%s is not bound by the creation hash", name)
		}
	}
	succ := uuid.New()
	tr := PolicyTransition{ID: uuid.New(), From: "active", To: "withdrawn", Successor: &succ, ReasonCode: "r"}
	th0 := TransitionHash(tr)
	other := uuid.New()
	for name, mut := range map[string]func(x *PolicyTransition){
		"id":        func(x *PolicyTransition) { x.ID = uuid.New() },
		"to":        func(x *PolicyTransition) { x.To = "active" },
		"successor": func(x *PolicyTransition) { x.Successor = &other },
		"no succ":   func(x *PolicyTransition) { x.Successor = nil },
		"reason":    func(x *PolicyTransition) { x.ReasonCode = "s" },
	} {
		x := tr
		mut(&x)
		if TransitionHash(x) == th0 {
			t.Errorf("%s is not bound by the transition hash", name)
		}
	}
}

// Signer satisfies the interface kyc requires.
var _ kyc.PolicyProof = Signer{}
