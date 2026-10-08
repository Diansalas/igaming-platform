//go:build integration

package payoutinstrument

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// countingVerifier is a Synthetic verifier that counts vendor calls.
type countingVerifier struct {
	MockVerifier
	calls atomic.Int32
}

func (c *countingVerifier) Verify(ctx context.Context, r VerifyRequest) (VerifyResult, error) {
	c.calls.Add(1)
	return c.MockVerifier.Verify(ctx, r)
}

func TestRegisterVerify_HappyPath(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	inst := w.mustRegister(p, ibanA)
	if inst.State != StatePendingVerification || inst.DisplayMask != "GB****5432" || inst.Kind != KindBankAccount {
		t.Fatalf("registered instrument = %+v", inst)
	}
	if inst.PersonID != p.PersonID || inst.BrandID != p.BrandID || inst.DetailKeyKID != "m1" || inst.DetailSchemaVersion != 1 {
		t.Fatalf("DB-forced/copied columns wrong: %+v", inst)
	}
	res, err := w.svc.Verify(context.Background(), w.rt, w.tenantID, inst.ID)
	if err != nil || !res.Verified || res.State != StateVerified || res.Source != SourceSynthetic {
		t.Fatalf("verify: %+v %v", res, err)
	}
	got := w.load(inst.ID)
	if got.State != StateVerified || got.CurrentVerificationID == nil {
		t.Fatalf("after verify: %+v", got)
	}
	var src, own, outcome string
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT source, ownership_assertion, outcome FROM payout_instrument_verifications WHERE id = $1`, *got.CurrentVerificationID).Scan(&src, &own, &outcome)
	}); err != nil {
		t.Fatal(err)
	}
	if src != "synthetic" || own != "synthetic_asserted" || outcome != "verified" {
		t.Fatalf("verification = %s %s %s", src, own, outcome)
	}
	// Audit rows exist for both transitions and carry no detail.
	for _, action := range []string{"payout_instrument.registered", "payout_instrument.verified"} {
		if n := w.count("audit_log", "action = $1 AND target_id = $2", action, inst.ID.String()); n != 1 {
			t.Errorf("audit %s rows = %d, want 1", action, n)
		}
	}
	// The seals verify and the gate passes for a Synthetic adapter and for no adapter.
	if _, err := w.gate(got, p, "EUR", syntheticAdapter{}); err != nil {
		t.Fatalf("gate (synthetic adapter): %v", err)
	}
	g, err := w.gate(got, p, "EUR", nil)
	if err != nil {
		t.Fatalf("gate (no adapter): %v", err)
	}
	if string(g.Detail) != `{"iban":"`+ibanA+`"}` {
		t.Fatalf("decrypted detail = %q", g.Detail)
	}
	dest := g.Destination(got.FingerprintKID)
	if strings.Contains(fmt.Sprintf("%+v", dest), ibanA) {
		t.Fatal("destination leaked")
	}
}

// scanForNeedles scans every public base table in the tenant's session (text
// and hex form of each row) and returns table -> needle -> hit count.
func scanForNeedles(t *testing.T, w *world, needles []string) (hits map[string]int, scanned int) {
	t.Helper()
	var tables []string
	if err := w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY 1`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			tables = append(tables, n)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	hits = map[string]int{}
	for _, tbl := range tables {
		for _, needle := range needles {
			var n int
			err := w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s t WHERE t::text ILIKE $1`, pgx.Identifier{tbl}.Sanitize()), "%"+needle+"%").Scan(&n)
			})
			if err != nil {
				continue // a table whose RLS needs another GUC shape cannot hold this tenant's rows
			}
			if n > 0 {
				hits[tbl+"|"+needle] += n
			}
		}
		scanned++
	}
	return hits, scanned
}

var ibanNeedles = []string{"GB82WEST", "GB82 WEST", "12345698765432", "4742383257455354" /* hex of GB82WEST */, `"iban"`}

// No plaintext IBAN anywhere in the database, including the audit log: every
// public table is scanned (text and hex form).
func TestCiphertextOnlyAtRest(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	inst := w.verified(p, ibanA)
	// Suspend and revoke paths also write audit rows: scan after they ran.
	p2 := w.newPlayer(w.brandID)
	inst2 := w.verified(p2, ibanB)
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.Suspend(ctx, tx, BlockParams{TenantID: w.tenantID, InstrumentID: inst2.ID, Actor: Actor{Type: ActorStaff, ID: uuid.NewString()}, ReasonCode: "aml_review"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	hits, scanned := scanForNeedles(t, w, ibanNeedles)
	if scanned < 100 {
		t.Fatalf("only %d tables scanned", scanned)
	}
	for k, n := range hits {
		t.Errorf("plaintext found: %s (%d row(s))", k, n)
	}
	if len(inst.DetailCiphertext) < 17 || len(inst.DetailNonce) != 12 {
		t.Fatalf("ciphertext/nonce shape: %d/%d", len(inst.DetailCiphertext), len(inst.DetailNonce))
	}
}

// Negative control for the scan: it DOES find a plaintext IBAN planted in the
// audit log and in a payout table's bytea, so the clean result above means
// something.
func TestCiphertextOnlyAtRest_ScanControl(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	w.mustRegister(p, ibanA)
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor_type, actor_id, action, outcome, metadata) VALUES ($1,'player',$2,'control.plaintext','success',$3::jsonb)`,
			w.tenantID, p.ID, `{"leak":"`+ibanA+`"}`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.tamper("payout_instruments", `UPDATE payout_instruments SET detail_ciphertext = convert_to($1, 'UTF8') || convert_to('0123456789abcdef', 'UTF8') WHERE tenant_id = $2`, ibanA, w.tenantID)
	hits, _ := scanForNeedles(t, w, ibanNeedles)
	foundAudit, foundBytea := false, false
	for k := range hits {
		if strings.HasPrefix(k, "audit_log|") {
			foundAudit = true
		}
		if strings.HasPrefix(k, "payout_instruments|") {
			foundBytea = true
		}
	}
	if !foundAudit || !foundBytea {
		t.Fatalf("the scan must find planted plaintext in the audit log (%v) and in a bytea column (%v); hits=%v", foundAudit, foundBytea, hits)
	}
}

func TestRegisterIdempotentAndConcurrent(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	first, err := w.register(p, ibanA)
	if err != nil || first.Existing {
		t.Fatalf("first: %+v %v", first, err)
	}
	// Spacing/case variants of the same destination are the same instrument.
	again, err := w.registerWith(w.svc, RegisterParams{TenantID: w.tenantID, PlayerAccountID: p.ID, Kind: KindBankAccount, Rail: "sepa", AssetCodes: []string{"EUR"},
		Detail: ibanDetail("gb82 west 1234 5698 7654 32")})
	if err != nil || !again.Existing || again.Instrument.ID != first.Instrument.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	// Concurrent double register of a new destination: exactly one live row.
	const n = 12
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := w.register(p, ibanB)
			ids[i], errs[i] = r.Instrument.ID, err
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("concurrent register %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("concurrent registers returned different instruments: %v vs %v", ids[i], ids[0])
		}
	}
	if c := w.count("payout_instruments", "player_account_id = $1", p.ID); c != 2 {
		t.Fatalf("live instruments = %d, want 2", c)
	}
}

func TestFingerprintConflictAcrossPersons(t *testing.T) {
	w := newWorld(t)
	p1, p2 := w.newPlayer(w.brandID), w.newPlayer(w.brandID)
	w.mustRegister(p1, ibanA)
	for _, variant := range []string{ibanA, "gb82 west 1234 5698 7654 32", strings.ToLower(ibanA)} {
		if _, err := w.register(p2, variant); !errors.Is(err, ErrFingerprintConflict) {
			t.Fatalf("a second Person registering %q must conflict, got %v", variant, err)
		}
	}
	// A conflict leaves nothing behind for p2 (the tx rolled back).
	if c := w.count("payout_instruments", "player_account_id = $1", p2.ID); c != 0 {
		t.Fatalf("p2 instruments after a conflict = %d", c)
	}
	// Other destinations are unaffected.
	w.mustRegister(p2, ibanB)
	// Ownership is never deleted: after p1 revokes, p2 STILL conflicts (A-2).
	inst := w.load(w.mustRegister(p1, ibanA).ID)
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.svc.Revoke(ctx, tx, BlockParams{TenantID: w.tenantID, InstrumentID: inst.ID, PlayerAccountID: p1.ID,
			Actor: Actor{Type: ActorPlayer, ID: p1.ID.String()}, ReasonCode: "player_revoked"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p3 := w.newPlayer(w.brandID)
	if _, err := w.register(p3, ibanA); !errors.Is(err, ErrFingerprintConflict) {
		t.Fatalf("ownership survives revocation: %v", err)
	}
	// The SAME Person on a second player account (another brand) is not a conflict.
	p1b := player{ID: uuid.New(), PersonID: p1.PersonID, BrandID: w.brand2ID}
	if err := w.owner.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,$5,'x','active')`,
			p1b.ID, w.tenantID, p1b.BrandID, p1b.PersonID, p1b.ID.String()+"@example.test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.register(p1b, ibanA); err != nil {
		t.Fatalf("same Person, other account: %v", err)
	}
}

func TestFingerprintConflictAcrossTenantsIsAllowed(t *testing.T) {
	w1 := newWorld(t)
	w2 := newWorld(t)
	w2.svc = w1.svc // same keys: only the tenant binding differs
	p1, p2 := w1.newPlayer(w1.brandID), w2.newPlayer(w2.brandID)
	w1.mustRegister(p1, ibanA)
	if _, err := w2.register(p2, ibanA); err != nil {
		t.Fatalf("fingerprints are tenant-bound: the same IBAN in another tenant is fine: %v", err)
	}
}

func TestFingerprintConflictRaceIsRaceFree(t *testing.T) {
	w := newWorld(t)
	for round := 0; round < 10; round++ {
		p1, p2 := w.newPlayer(w.brandID), w.newPlayer(w.brandID)
		iban := fmt.Sprintf("DE%02d370400440532013000", 10+round)
		_ = iban
		d := fmt.Sprintf(`{"country":"BR","account_number":"RACE%04d"}`, round)
		rp := func(pl player) RegisterParams {
			return RegisterParams{TenantID: w.tenantID, PlayerAccountID: pl.ID, Kind: KindBankAccount, Rail: "pix", AssetCodes: []string{"EUR"}, Detail: []byte(d)}
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, pl := range []player{p1, p2} {
			wg.Add(1)
			go func(i int, pl player) {
				defer wg.Done()
				<-start
				_, errs[i] = w.registerWith(w.svc, rp(pl))
			}(i, pl)
		}
		close(start)
		wg.Wait()
		ok, conflict := 0, 0
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			case errors.Is(e, ErrFingerprintConflict):
				conflict++
			default:
				t.Fatalf("round %d unexpected error: %v", round, e)
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("round %d: exactly one Person must own the fingerprint, got ok=%d conflict=%d", round, ok, conflict)
		}
	}
}

// Fingerprint key rotation: a conflict is detected under EVERY retained kid.
func TestFingerprintRotation_ConflictUnderAllRetainedKids(t *testing.T) {
	master, f1, f2 := randKey(t), randKey(t), randKey(t)
	k1, err := NewKeys("m1", map[string][]byte{"m1": master}, "f1", map[string][]byte{"f1": f1})
	if err != nil {
		t.Fatal(err)
	}
	k2, err := NewKeys("m1", map[string][]byte{"m1": master}, "f2", map[string][]byte{"f1": f1, "f2": f2})
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	svc1, _ := NewService(k1, DefaultKinds(), NewMockVerifier())
	svc2, _ := NewService(k2, DefaultKinds(), NewMockVerifier())
	p1, p2 := w.newPlayer(w.brandID), w.newPlayer(w.brandID)

	// p1 registers under the old key set only (owner row under f1 only).
	old, err := w.registerWith(svc1, w.registerParams(p1, ibanA))
	if err != nil {
		t.Fatal(err)
	}
	if old.Instrument.FingerprintKID != "f1" {
		t.Fatalf("kid = %s", old.Instrument.FingerprintKID)
	}
	// After rotation (active f2, f1 retained) another Person still conflicts: the
	// f2 fingerprint has no owner row yet, the f1 one does.
	if _, err := w.registerWith(svc2, w.registerParams(p2, ibanA)); !errors.Is(err, ErrFingerprintConflict) {
		t.Fatalf("conflict under a retained kid must be detected: %v", err)
	}
	// A new registration after rotation records owner rows under BOTH kids.
	newInst, err := w.registerWith(svc2, w.registerParams(p1, ibanB))
	if err != nil || newInst.Instrument.FingerprintKID != "f2" {
		t.Fatalf("post-rotation register: %+v %v", newInst, err)
	}
	if n := w.count("payout_instrument_fingerprint_owners", "person_id = $1", p1.PersonID); n != 3 {
		t.Fatalf("owner rows for p1 = %d, want 3 (A@f1, B@f1, B@f2)", n)
	}
	// The old instrument still verifies and passes the gate with the rotated key set (retained kid).
	if _, err := svc2.Verify(context.Background(), w.rt, w.tenantID, old.Instrument.ID); err != nil {
		t.Fatalf("verify an f1 instrument under the rotated set: %v", err)
	}
	// The re-fingerprint job inserts the f2 owner row for the old instrument; no conflicts.
	var rf RefingerprintResult
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rf, err = svc2.Refingerprint(ctx, tx, w.tenantID, "f2")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rf.Instruments != 2 || rf.Conflicts != 0 {
		t.Fatalf("refingerprint = %+v", rf)
	}
	// A foreign owner under f2 for an f1-era fingerprint is counted as a conflict.
	const ibanC = "FR1420041010050500013M02606"
	if _, err := w.registerWith(svc1, w.registerParams(p1, ibanC)); err != nil {
		t.Fatal(err)
	}
	spec, _ := DefaultKinds().Spec(KindBankAccount)
	n, _ := spec.Normalize(ibanDetail(ibanC))
	fpC2, _ := k2.Fingerprint("f2", w.tenantID, KindBankAccount, n.FingerprintInput)
	p3 := w.newPlayer(w.brandID)
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_fingerprint_owners (tenant_id, fingerprint_kid, fingerprint, person_id) VALUES ($1,'f2',$2,$3)`,
			w.tenantID, fpC2, p3.PersonID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rf, err = svc2.Refingerprint(ctx, tx, w.tenantID, "f2")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rf.Conflicts != 1 {
		t.Fatalf("refingerprint conflicts = %d, want 1", rf.Conflicts)
	}
}

func TestRegisterRefusals(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	base := w.registerParams(p, ibanA)
	bad := map[string]func(rp *RegisterParams){
		"unknown kind":      func(rp *RegisterParams) { rp.Kind = "nope" },
		"rail not for kind": func(rp *RegisterParams) { rp.Rail = "card" },
		"no assets":         func(rp *RegisterParams) { rp.AssetCodes = nil },
		"unknown asset":     func(rp *RegisterParams) { rp.AssetCodes = []string{"XXX"} },
		"invalid detail":    func(rp *RegisterParams) { rp.Detail = []byte(`{"iban":"nope"}`) },
		"PAN": func(rp *RegisterParams) {
			rp.Kind, rp.Rail = KindSyntheticTest, "synthetic"
			rp.Detail = []byte(`{"label":"x","n":"4111111111111111"}`)
		},
		"foreign player": func(rp *RegisterParams) { rp.PlayerAccountID = uuid.New() },
	}
	for name, mut := range bad {
		rp := base
		mut(&rp)
		if name == "PAN" {
			_, err := w.registerWith(w.svc, rp)
			if !errors.Is(err, ErrInvalidRegistration) || !errors.Is(err, ErrPANRefused) {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if _, err := w.registerWith(w.svc, rp); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	if c := w.count("payout_instruments", ""); c != 0 {
		t.Fatalf("refusals must leave no rows, found %d", c)
	}
	// The unknown-asset refusal is the database trigger's (PI005).
	rp := base
	rp.AssetCodes = []string{"XXX"}
	if _, err := w.registerWith(w.svc, rp); err == nil || !errors.Is(err, ErrInvalidRegistration) {
		t.Errorf("unknown asset: %v", err)
	}
}
