//go:build integration

package payoutinstrument

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestGate_Matrix(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	good := w.verified(p, ibanA)
	other := w.newPlayer(w.brandID)
	otherBrand := w.newPlayer(w.brand2ID)

	// Passing paths.
	if _, err := w.gate(good, p, "EUR", nil); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// (5) relational: another player, another brand, another person, unknown id.
	for name, pl := range map[string]player{"other player": other, "other brand": otherBrand} {
		_, err := w.gate(good, pl, "EUR", nil)
		requireGateReason(t, err, ReasonRelationMismatch, true)
		_ = name
	}
	wrongPerson := p
	wrongPerson.PersonID = uuid.New()
	_, err := w.gate(good, wrongPerson, "EUR", nil)
	requireGateReason(t, err, ReasonRelationMismatch, true)
	ghost := good
	ghost.ID = uuid.New()
	_, err = w.gate(ghost, p, "EUR", nil)
	requireGateReason(t, err, ReasonNotFound, false)
	// asset not listed.
	_, err = w.gate(good, p, "USD", nil)
	requireGateReason(t, err, ReasonAssetNotListed, false)

	// (2) a LATER verified verification row exists: the in-force one is not the latest.
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_verifications (tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id, outcome, verified_at, expires_at, verification_seal, seal_kid, created_txid)
			VALUES ($1,$2,'synthetic','synthetic_asserted','x','verified', now(), now() + interval '1 day', repeat('c',64),'m1',0)`, w.tenantID, good.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = w.gate(w.load(good.ID), p, "EUR", nil)
	requireGateReason(t, err, ReasonVerificationNotLast, true)
}

func TestGate_BlockingEventsEvenWithTamperedState(t *testing.T) {
	// Rules (3) and (4) are enforced from the EVENTS, not only from the state
	// column: an attacker who flips the state back to verified (owner SQL with
	// the triggers off) is still refused.
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	a := w.verified(p, ibanA)
	if err := w.block(a, EventSuspend, staff(), p); err != nil {
		t.Fatal(err)
	}
	w.tamper("payout_instruments", `UPDATE payout_instruments SET state='verified' WHERE id=$1`, a.ID)
	_, err := w.gate(w.load(a.ID), p, "EUR", nil)
	requireGateReason(t, err, ReasonSuspended, false)

	b := w.verified(p, ibanB)
	if err := w.block(b, EventRevoke, Actor{Type: ActorPlayer, ID: p.ID.String()}, p); err != nil {
		t.Fatal(err)
	}
	w.tamper("payout_instruments", `UPDATE payout_instruments SET state='verified' WHERE id=$1`, b.ID)
	_, err = w.gate(w.load(b.ID), p, "EUR", nil)
	requireGateReason(t, err, ReasonRevoked, false)

	// A tampered blocking event (seal) is an integrity failure.
	c := w.verified(p, "FR1420041010050500013M02606")
	if err := w.block(c, EventSuspend, staff(), p); err != nil {
		t.Fatal(err)
	}
	w.tamper("payout_instruments", `UPDATE payout_instruments SET state='verified' WHERE id=$1`, c.ID)
	w.tamper("payout_instrument_blocking_events", `UPDATE payout_instrument_blocking_events SET reason_code='tampered' WHERE instrument_id=$1`, c.ID)
	_, err = w.gate(w.load(c.ID), p, "EUR", nil)
	requireGateReason(t, err, ReasonSealInvalid, true)
}

// Seal tamper detection against the database: every tamperable sealed column
// of the instrument, the verification and the blocking event is edited with
// the triggers off and the gate refuses (integrity failure).
func TestGate_SealTamperDetection(t *testing.T) {
	type tc struct{ table, stmt string }
	instrumentCols := []tc{
		{"payout_instruments", `UPDATE payout_instruments SET rail='pix' WHERE id=$1`},
		{"payout_instruments", `UPDATE payout_instruments SET asset_codes='{EUR,USD}' WHERE id=$1`},
		{"payout_instruments", `UPDATE payout_instruments SET display_mask='GB****0000' WHERE id=$1`},
		{"payout_instruments", `UPDATE payout_instruments SET detail_schema_version=2 WHERE id=$1`},
		{"payout_instruments", `UPDATE payout_instruments SET detail_key_kid='m9' WHERE id=$1`},
		{"payout_instruments", `UPDATE payout_instruments SET instrument_seal=repeat('f',64) WHERE id=$1`},
		{"payout_instruments", `UPDATE payout_instruments SET seal_kid='m9' WHERE id=$1`},
		{"payout_instruments", `UPDATE payout_instruments SET supersedes_instrument_id=id WHERE id=$1`}, // self-supersede: CHECK blocks
		{"payout_instruments", `UPDATE payout_instruments SET kind='card_token', rail='card' WHERE id=$1`},
	}
	skipped := 0
	defer func() {
		if skipped != 1 { // only the self-supersede CHECK protects itself
			t.Errorf("tamper cases skipped = %d, want exactly 1", skipped)
		}
	}()
	for i, c := range instrumentCols {
		w := newWorld(t)
		p := w.newPlayer(w.brandID)
		inst := w.verified(p, ibanA)
		if err := w.tamperErr(c.table, c.stmt, inst.ID); err != nil {
			skipped++ // a column the schema itself protects (CHECK/FK) cannot be tampered at all
			continue
		}
		_, gerr := w.gate(w.load(inst.ID), p, "EUR", nil)
		if g, ok := IsGateRefusal(gerr); !ok || !g.Integrity() {
			t.Errorf("case %d (%s): tampering must be an integrity refusal, got %v", i, c.stmt, gerr)
		}
	}

	verCols := []string{
		`UPDATE payout_instrument_verifications SET expires_at = expires_at + interval '1 year' WHERE instrument_id=$1`,
		`UPDATE payout_instrument_verifications SET verifier_provider_id = 'other' WHERE instrument_id=$1`,
		`UPDATE payout_instrument_verifications SET verifier_reference_hash = repeat('1',64) WHERE instrument_id=$1`,
		`UPDATE payout_instrument_verifications SET verification_seal = repeat('1',64) WHERE instrument_id=$1`,
		`UPDATE payout_instrument_verifications SET seal_kid = 'm9' WHERE instrument_id=$1`,
		`UPDATE payout_instrument_verifications SET verified_at = verified_at + interval '1 second', expires_at = expires_at + interval '1 second' WHERE instrument_id=$1`,
	}
	for i, q := range verCols {
		w := newWorld(t)
		p := w.newPlayer(w.brandID)
		inst := w.verified(p, ibanA)
		w.tamper("payout_instrument_verifications", q, inst.ID)
		_, gerr := w.gate(w.load(inst.ID), p, "EUR", nil)
		if g, ok := IsGateRefusal(gerr); !ok || !g.Integrity() {
			t.Errorf("verification case %d: tampering must be an integrity refusal, got %v", i, gerr)
		}
	}
	// Source flipped synthetic -> real keeps the CHECK happy only with the
	// ownership flip; both columns are sealed.
	{
		w := newWorld(t)
		p := w.newPlayer(w.brandID)
		inst := w.verified(p, ibanA)
		w.tamper("payout_instrument_verifications", `UPDATE payout_instrument_verifications SET source='psp_account_verification', ownership_assertion='account_holder_matches_verified_identity' WHERE instrument_id=$1`, inst.ID)
		_, gerr := w.gate(w.load(inst.ID), p, "EUR", syntheticAdapter{})
		if g, ok := IsGateRefusal(gerr); !ok || !g.Integrity() {
			t.Errorf("source/ownership flip must be an integrity refusal, got %v", gerr)
		}
	}
}

// (l) AEAD bound to the row: an instrument's ciphertext moved to another row fails.
func TestGate_CiphertextMovedToAnotherRowFails(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	a, b := w.verified(p, ibanA), w.verified(p, ibanB)
	w.tamper("payout_instruments", `UPDATE payout_instruments SET detail_ciphertext = $1, detail_nonce = $2 WHERE id = $3`, a.DetailCiphertext, a.DetailNonce, b.ID)
	_, err := w.gate(w.load(b.ID), p, "EUR", nil)
	requireGateReason(t, err, ReasonDetailUnavailable, true)
	// The original is intact.
	if _, err := w.gate(a, p, "EUR", nil); err != nil {
		t.Fatalf("original: %v", err)
	}
	// Moved across tenants: the AAD binds the tenant too (decrypt directly).
	pt, derr := w.keys.DecryptDetail(a.DetailKeyKID, DetailAAD{TenantID: uuid.New(), InstrumentID: a.ID, Kind: a.Kind, SchemaVersion: a.DetailSchemaVersion}, a.DetailCiphertext, a.DetailNonce)
	if derr == nil || pt != nil {
		t.Fatal("ciphertext must not decrypt under another tenant's AAD")
	}
	// The seal still passes for b (it does not cover the ciphertext); the AEAD is the control.
	bb := w.load(b.ID)
	if err := w.keys.VerifyInstrument(&bb); err != nil {
		t.Fatalf("seal unexpectedly invalid: %v", err)
	}
}

// Tiering at the gate (the same predicate runs at T1p, phase B and T2/T12).
func TestGate_TieringSyntheticVerificationNeverSatisfiesARealAdapter(t *testing.T) {
	w := newWorld(t)
	p := w.newPlayer(w.brandID)
	inst := w.verified(p, ibanA) // MOCK verifier -> synthetic source
	if _, err := w.gate(inst, p, "EUR", syntheticAdapter{}); err != nil {
		t.Fatalf("synthetic adapter must accept a synthetic verification: %v", err)
	}
	_, err := w.gate(inst, p, "EUR", realAdapter{})
	requireGateReason(t, err, ReasonVerifierNotRegd, false)
	// Even with the (synthetic) verifier id registered, a real adapter is refused.
	_, err = w.gate(inst, p, "EUR", nilAdapterMarker{})
	requireGateReason(t, err, ReasonVerifierNotRegd, false)
}

type nilAdapterMarker struct{}

func TestVerify_NonSyntheticRequirements(t *testing.T) {
	ctx := context.Background()
	newReal := func(src VerificationSource) (*world, *Service, player) {
		w := newWorld(t)
		svc, _ := NewService(w.keys, DefaultKinds(), realVerifierOK{realVerifier{src: src}})
		return w, svc, w.newPlayer(w.brandID)
	}
	// No KYC -> refused before any vendor call; the instrument stays pending.
	w, svc, p := newReal(SourcePSPAccount)
	inst := w.mustRegister(p, ibanA)
	if _, err := svc.Verify(ctx, w.rt, w.tenantID, inst.ID); !errors.Is(err, ErrKYCNotVerified) {
		t.Fatalf("no KYC: %v", err)
	}
	// KYC approved but no max-age row -> no non-synthetic verification can be recorded.
	w.approveKYC(p)
	if _, err := svc.Verify(ctx, w.rt, w.tenantID, inst.ID); !errors.Is(err, ErrMaxAgeNotConfigured) {
		t.Fatalf("no max-age: %v", err)
	}
	// A newer non-approved KYC row denies (fail-closed).
	w.setMaxAge(7 * 24 * time.Hour)
	if err := w.ownerTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, created_at)
			VALUES ($1,$2,$3,$4,$5,'rejected','mock', now() + interval '1 second')`, uuid.New(), w.tenantID, p.BrandID, p.ID, p.PersonID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(ctx, w.rt, w.tenantID, inst.ID); !errors.Is(err, ErrKYCNotVerified) {
		t.Fatalf("a newer rejected KYC row must deny: %v", err)
	}

	// Full success with max age: expires_at = min(source expiry, max age).
	w, svc, p = newReal(SourcePSPAccount)
	w.approveKYC(p)
	w.setMaxAge(36 * time.Hour) // source expiry is 90 days
	inst = w.mustRegister(p, ibanA)
	res, err := svc.Verify(ctx, w.rt, w.tenantID, inst.ID)
	if err != nil || !res.Verified || res.Source != SourcePSPAccount {
		t.Fatalf("verify: %+v %v", res, err)
	}
	got := w.load(inst.ID)
	var exp, at time.Time
	var own string
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT expires_at, verified_at, ownership_assertion FROM payout_instrument_verifications WHERE id=$1`, *got.CurrentVerificationID).Scan(&exp, &at, &own)
	}); err != nil {
		t.Fatal(err)
	}
	if d := exp.Sub(at); d < 35*time.Hour || d > 37*time.Hour || own != OwnershipVerified {
		t.Fatalf("expiry window = %v ownership %s (want ~36h, account_holder_matches_verified_identity)", d, own)
	}
	// The real adapter accepts it only with a REGISTERED non-Synthetic verifier.
	g := func(s *Service, adapter any) error {
		return w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := s.EvaluateGate(ctx, tx, GateParams{TenantID: w.tenantID, BrandID: p.BrandID, PlayerAccountID: p.ID, PersonID: p.PersonID, InstrumentID: inst.ID, AssetCode: "EUR", Adapter: adapter, Lock: true})
			return err
		})
	}
	if err := g(svc, realAdapter{}); err != nil {
		t.Fatalf("real adapter + real verification + registered verifier: %v", err)
	}
	if err := g(svc, syntheticAdapter{}); err != nil {
		t.Fatalf("synthetic adapter accepts any source: %v", err)
	}
	// Verifier no longer registered (e.g. decommissioned): refused for a real adapter.
	unreg, _ := NewService(w.keys, DefaultKinds(), NewMockVerifier())
	requireGateReason(t, g(unreg, realAdapter{}), ReasonVerifierNotRegd, false)
	// A kind not enabled for non-synthetic use cannot be verified by a real verifier.
	cp := w.newPlayer(w.brandID)
	w.approveKYC(cp)
	crypto, err := w.registerWith(svc, RegisterParams{TenantID: w.tenantID, PlayerAccountID: cp.ID, Kind: KindCryptoAddress, Rail: "crypto", AssetCodes: []string{"BTC"},
		Detail: []byte(`{"network":"btc","address":"bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(ctx, w.rt, w.tenantID, crypto.Instrument.ID); !errors.Is(err, ErrKindDisabled) {
		t.Fatalf("crypto_address must be disabled for non-synthetic verification: %v", err)
	}
	// A real verifier that declares the synthetic source is refused.
	w2 := newWorld(t)
	bad, _ := NewService(w2.keys, DefaultKinds(), realVerifierOK{realVerifier{src: SourceSynthetic}})
	p2 := w2.newPlayer(w2.brandID)
	w2.approveKYC(p2)
	w2.setMaxAge(time.Hour)
	i2 := w2.mustRegister(p2, ibanA)
	if _, err := bad.Verify(ctx, w2.rt, w2.tenantID, i2.ID); !errors.Is(err, ErrVerifierSourceRefused) {
		t.Fatalf("a non-Synthetic verifier returning synthetic must be refused: %v", err)
	}
	// A real verifier that does not assert the holder matches -> rejected.
	w3 := newWorld(t)
	noMatch, _ := NewService(w3.keys, DefaultKinds(), realVerifierNoMatch{realVerifier{src: SourceKYCVendor}})
	p3 := w3.newPlayer(w3.brandID)
	w3.approveKYC(p3)
	w3.setMaxAge(time.Hour)
	i3 := w3.mustRegister(p3, ibanA)
	r3, err := noMatch.Verify(ctx, w3.rt, w3.tenantID, i3.ID)
	if err != nil || r3.Verified || r3.State != StateRejected {
		t.Fatalf("holder mismatch must reject: %+v %v", r3, err)
	}
}

type realVerifierOK struct{ realVerifier }

func (realVerifierOK) Verify(context.Context, VerifyRequest) (VerifyResult, error) {
	return VerifyResult{Verified: true, AccountHolderMatchesVerifiedIdentity: true, SourceExpiry: time.Now().Add(90 * 24 * time.Hour)}, nil
}

type realVerifierNoMatch struct{ realVerifier }

func (realVerifierNoMatch) Verify(context.Context, VerifyRequest) (VerifyResult, error) {
	return VerifyResult{Verified: true, AccountHolderMatchesVerifiedIdentity: false, SourceExpiry: time.Now().Add(90 * 24 * time.Hour)}, nil
}

// setMaxAge licenses the tenant under a fresh jurisdiction and writes the
// per-jurisdiction max age as the OWNER role (the runtime role cannot).
func (w *world) setMaxAge(d time.Duration) {
	w.t.Helper()
	jid, lid := uuid.New(), uuid.New()
	if err := w.owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'B13 test jurisdiction')`, jid, "B13-"+jid.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1,$2,'platform',$3)`, lid, jid, "L-"+lid.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lid, w.tenantID)
		return err
	}); err != nil {
		w.t.Fatalf("license tenant: %v", err)
	}
	if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_verification_max_age (jurisdiction_id, max_age) VALUES ($1, make_interval(secs => $2))`, jid, d.Seconds())
		return err
	}); err != nil {
		w.t.Fatalf("write max age as the owner role: %v", err)
	}
}
