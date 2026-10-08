package payoutinstrument

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewKeysRules(t *testing.T) {
	k1, k2 := randKey(t), randKey(t)
	if _, err := NewKeys("m1", map[string][]byte{"m1": k1}, "f1", map[string][]byte{"f1": k2}); err != nil {
		t.Fatalf("valid keys: %v", err)
	}
	cases := map[string]struct {
		mk  string
		m   map[string][]byte
		fk  string
		fpm map[string][]byte
	}{
		"short master":        {"m1", map[string][]byte{"m1": []byte("short")}, "f1", map[string][]byte{"f1": k2}},
		"short fp":            {"m1", map[string][]byte{"m1": k1}, "f1", map[string][]byte{"f1": []byte("short")}},
		"missing active":      {"m9", map[string][]byte{"m1": k1}, "f1", map[string][]byte{"f1": k2}},
		"missing active fp":   {"m1", map[string][]byte{"m1": k1}, "f9", map[string][]byte{"f1": k2}},
		"same value both fam": {"m1", map[string][]byte{"m1": k1}, "f1", map[string][]byte{"f1": k1}},
		"bad kid":             {"m 1", map[string][]byte{"m 1": k1}, "f1", map[string][]byte{"f1": k2}},
	}
	for name, c := range cases {
		if _, err := NewKeys(c.mk, c.m, c.fk, c.fpm); !errors.Is(err, ErrKeyConfig) {
			t.Errorf("%s: want ErrKeyConfig, got %v", name, err)
		}
	}
}

func TestSubkeysAreIndependent(t *testing.T) {
	k := newTestKeys(t)
	if bytes.Equal(k.seal["m1"], k.detail["m1"]) {
		t.Fatal("seal and detail subkeys must differ (distinct HKDF labels)")
	}
}

func TestAEADRoundTripAndAAD(t *testing.T) {
	k := newTestKeys(t)
	aad := DetailAAD{TenantID: uuid.New(), InstrumentID: uuid.New(), Kind: KindBankAccount, SchemaVersion: 1}
	pt := []byte(`{"iban":"GB82WEST12345698765432"}`)
	ct, nonce, kid, err := k.EncryptDetail(aad, pt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("GB82")) || bytes.Contains(ct, []byte("iban")) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := k.DecryptDetail(kid, aad, ct, nonce)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("round trip: %v %q", err, got)
	}
	// Every AAD component is bound.
	mut := []DetailAAD{
		{TenantID: uuid.New(), InstrumentID: aad.InstrumentID, Kind: aad.Kind, SchemaVersion: aad.SchemaVersion},
		{TenantID: aad.TenantID, InstrumentID: uuid.New(), Kind: aad.Kind, SchemaVersion: aad.SchemaVersion},
		{TenantID: aad.TenantID, InstrumentID: aad.InstrumentID, Kind: KindCardToken, SchemaVersion: aad.SchemaVersion},
		{TenantID: aad.TenantID, InstrumentID: aad.InstrumentID, Kind: aad.Kind, SchemaVersion: 2},
	}
	for i, m := range mut {
		if _, err := k.DecryptDetail(kid, m, ct, nonce); !errors.Is(err, ErrDecrypt) {
			t.Errorf("wrong AAD component %d must fail authentication, got %v", i, err)
		}
	}
	// Tampered ciphertext / nonce, unknown kid.
	bad := append([]byte(nil), ct...)
	bad[0] ^= 1
	if _, err := k.DecryptDetail(kid, aad, bad, nonce); !errors.Is(err, ErrDecrypt) {
		t.Error("tampered ciphertext must fail")
	}
	if _, err := k.DecryptDetail("nope", aad, ct, nonce); !errors.Is(err, ErrUnknownKID) {
		t.Error("unknown kid must fail")
	}
	// Two encryptions of the same plaintext differ (random nonce).
	ct2, n2, _, _ := k.EncryptDetail(aad, pt)
	if bytes.Equal(ct, ct2) || bytes.Equal(nonce, n2) {
		t.Error("nonce reuse")
	}
	if _, _, _, err := k.EncryptDetail(aad, nil); !errors.Is(err, ErrDetailTooLarge) {
		t.Error("empty plaintext must be refused")
	}
	if _, _, _, err := k.EncryptDetail(aad, make([]byte, MaxDetailPlaintext+1)); !errors.Is(err, ErrDetailTooLarge) {
		t.Error("oversize plaintext must be refused")
	}
}

func TestFingerprintTenantBoundAndKeyed(t *testing.T) {
	k := newTestKeys(t, "f1", "f2")
	t1, t2 := uuid.New(), uuid.New()
	a, _ := k.Fingerprint("f1", t1, KindBankAccount, "iban:X")
	b, _ := k.Fingerprint("f1", t2, KindBankAccount, "iban:X")
	c, _ := k.Fingerprint("f2", t1, KindBankAccount, "iban:X")
	d, _ := k.Fingerprint("f1", t1, KindCardToken, "iban:X")
	if a == b || a == c || a == d {
		t.Fatal("fingerprint must be bound to tenant, key and kind")
	}
	if len(a) != 64 {
		t.Fatalf("fingerprint length %d", len(a))
	}
	if _, err := k.Fingerprint("nope", t1, KindBankAccount, "x"); !errors.Is(err, ErrUnknownKID) {
		t.Fatal("unknown fp kid")
	}
	if got := k.FingerprintKIDs(); len(got) != 2 || got[0] != "f1" || got[1] != "f2" {
		t.Fatalf("kids = %v", got)
	}
}

func sampleInstrument() *Instrument {
	sup := uuid.New()
	return &Instrument{
		ID: uuid.New(), TenantID: uuid.New(), BrandID: uuid.New(), PlayerAccountID: uuid.New(), PersonID: uuid.New(),
		Kind: KindBankAccount, Rail: "sepa", AssetCodes: []string{"EUR", "USD"}, DetailKeyKID: "m1", DetailSchemaVersion: 1,
		DisplayMask: "GB****5432", Fingerprint: strings.Repeat("a", 64), FingerprintKID: "f1", SupersedesInstrument: &sup,
	}
}

// Every field the instrument seal covers changes the MAC when mutated.
func TestInstrumentSealCoversEveryColumn(t *testing.T) {
	k := newTestKeys(t)
	base := sampleInstrument()
	if err := k.SealInstrument(base); err != nil {
		t.Fatal(err)
	}
	if err := k.VerifyInstrument(base); err != nil {
		t.Fatalf("fresh seal must verify: %v", err)
	}
	other := uuid.New()
	mutations := map[string]func(i *Instrument){
		"id":                func(i *Instrument) { i.ID = other },
		"tenant_id":         func(i *Instrument) { i.TenantID = other },
		"brand_id":          func(i *Instrument) { i.BrandID = other },
		"player_account_id": func(i *Instrument) { i.PlayerAccountID = other },
		"person_id":         func(i *Instrument) { i.PersonID = other },
		"kind":              func(i *Instrument) { i.Kind = KindCardToken },
		"rail":              func(i *Instrument) { i.Rail = "pix" },
		"asset_codes":       func(i *Instrument) { i.AssetCodes = []string{"EUR"} },
		"fingerprint":       func(i *Instrument) { i.Fingerprint = strings.Repeat("b", 64) },
		"fingerprint_kid":   func(i *Instrument) { i.FingerprintKID = "f2" },
		"supersedes":        func(i *Instrument) { i.SupersedesInstrument = nil },
		"display_mask":      func(i *Instrument) { i.DisplayMask = "GB****0000" },
		"detail_key_kid":    func(i *Instrument) { i.DetailKeyKID = "m2" },
		"schema_version":    func(i *Instrument) { i.DetailSchemaVersion = 2 },
		"instrument_seal":   func(i *Instrument) { i.InstrumentSeal = strings.Repeat("0", 64) },
		"seal_kid":          func(i *Instrument) { i.SealKID = "m9" },
	}
	for name, mut := range mutations {
		c := *base
		c.AssetCodes = append([]string(nil), base.AssetCodes...)
		mut(&c)
		if err := k.VerifyInstrument(&c); !errors.Is(err, ErrSealInvalid) {
			t.Errorf("tampering %s must fail the instrument seal", name)
		}
	}
	// Array order does not matter (arrays are sorted).
	c := *base
	c.AssetCodes = []string{"USD", "EUR"}
	if err := k.VerifyInstrument(&c); err != nil {
		t.Errorf("asset order must not matter: %v", err)
	}
	// A seal made under another key does not verify.
	other2 := newTestKeys(t)
	if err := other2.VerifyInstrument(base); err == nil {
		t.Error("a different key must not verify the seal")
	}
}

func TestVerificationSealCoversEveryColumn(t *testing.T) {
	k := newTestKeys(t)
	ref := strings.Repeat("c", 64)
	now := time.Now().UTC().Truncate(time.Microsecond)
	base := &Verification{ID: uuid.New(), TenantID: uuid.New(), InstrumentID: uuid.New(), Source: SourcePSPAccount,
		OwnershipAssertion: OwnershipVerified, VerifierProviderID: "psp1", VerifierReferenceHash: &ref, Outcome: "verified",
		VerifiedAt: now, ExpiresAt: now.Add(time.Hour)}
	fp := strings.Repeat("a", 64)
	if err := k.SealVerification(base, fp); err != nil {
		t.Fatal(err)
	}
	if err := k.VerifyVerification(base, fp); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	mutations := map[string]func(v *Verification){
		"id": func(v *Verification) { v.ID = other }, "tenant_id": func(v *Verification) { v.TenantID = other },
		"instrument_id":     func(v *Verification) { v.InstrumentID = other },
		"source":            func(v *Verification) { v.Source = SourceSynthetic },
		"ownership":         func(v *Verification) { v.OwnershipAssertion = OwnershipSynthetic },
		"outcome":           func(v *Verification) { v.Outcome = "rejected" },
		"verifier_provider": func(v *Verification) { v.VerifierProviderID = "psp2" },
		"verified_at":       func(v *Verification) { v.VerifiedAt = v.VerifiedAt.Add(time.Microsecond) },
		"expires_at":        func(v *Verification) { v.ExpiresAt = v.ExpiresAt.Add(time.Microsecond) },
		"reference_hash":    func(v *Verification) { v.VerifierReferenceHash = nil },
		"seal":              func(v *Verification) { v.Seal = strings.Repeat("0", 64) },
		"seal_kid":          func(v *Verification) { v.SealKID = "m9" },
	}
	for name, mut := range mutations {
		c := *base
		mut(&c)
		if err := k.VerifyVerification(&c, fp); !errors.Is(err, ErrSealInvalid) {
			t.Errorf("tampering %s must fail the verification seal", name)
		}
	}
	// The seal binds the instrument's fingerprint.
	if err := k.VerifyVerification(base, strings.Repeat("b", 64)); !errors.Is(err, ErrSealInvalid) {
		t.Error("a different instrument fingerprint must fail the verification seal")
	}
}

func TestBlockingAndSnapshotSealsCoverEveryColumn(t *testing.T) {
	k := newTestKeys(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ev := &BlockingEvent{ID: uuid.New(), TenantID: uuid.New(), InstrumentID: uuid.New(), Event: EventSuspend, ActorType: ActorStaff,
		ActorID: uuid.NewString(), ReasonCode: "aml_review", OccurredAt: now}
	if err := k.SealBlockingEvent(ev); err != nil {
		t.Fatal(err)
	}
	if err := k.VerifyBlockingEvent(ev); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	em := map[string]func(e *BlockingEvent){
		"id": func(e *BlockingEvent) { e.ID = other }, "tenant": func(e *BlockingEvent) { e.TenantID = other },
		"instrument": func(e *BlockingEvent) { e.InstrumentID = other }, "event": func(e *BlockingEvent) { e.Event = EventRevoke },
		"actor_type": func(e *BlockingEvent) { e.ActorType = ActorPlayer }, "actor_id": func(e *BlockingEvent) { e.ActorID = "x" },
		"reason": func(e *BlockingEvent) { e.ReasonCode = "other" }, "occurred_at": func(e *BlockingEvent) { e.OccurredAt = e.OccurredAt.Add(time.Microsecond) },
		"seal": func(e *BlockingEvent) { e.Seal = strings.Repeat("0", 64) }, "seal_kid": func(e *BlockingEvent) { e.SealKID = "zz" },
	}
	for name, mut := range em {
		c := *ev
		mut(&c)
		if err := k.VerifyBlockingEvent(&c); !errors.Is(err, ErrSealInvalid) {
			t.Errorf("tampering blocking %s must fail", name)
		}
	}

	snap := &Snapshot{AttemptID: uuid.New(), TenantID: uuid.New(), WithdrawalRequestID: uuid.New(), InstrumentID: uuid.New(),
		Kind: KindBankAccount, Rail: "sepa", Fingerprint: strings.Repeat("a", 64), FingerprintKID: "f1", VerificationID: uuid.New(),
		VerificationSource: SourcePSPAccount, OwnershipAssertion: OwnershipVerified, VerifiedAt: now, VerificationExpiresAt: now.Add(time.Hour),
		DisplayMask: "GB****5432", Amount: "12345", AssetCode: "EUR"}
	if err := k.SealSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	if err := k.VerifySnapshot(snap); err != nil {
		t.Fatal(err)
	}
	sm := map[string]func(s *Snapshot){
		"attempt": func(s *Snapshot) { s.AttemptID = other }, "tenant": func(s *Snapshot) { s.TenantID = other },
		"withdrawal": func(s *Snapshot) { s.WithdrawalRequestID = other }, "instrument": func(s *Snapshot) { s.InstrumentID = other },
		"fingerprint": func(s *Snapshot) { s.Fingerprint = strings.Repeat("b", 64) }, "fp_kid": func(s *Snapshot) { s.FingerprintKID = "f2" },
		"kind": func(s *Snapshot) { s.Kind = KindCardToken }, "rail": func(s *Snapshot) { s.Rail = "pix" },
		"verification": func(s *Snapshot) { s.VerificationID = other }, "source": func(s *Snapshot) { s.VerificationSource = SourceSynthetic },
		"ownership":   func(s *Snapshot) { s.OwnershipAssertion = OwnershipSynthetic },
		"expires":     func(s *Snapshot) { s.VerificationExpiresAt = s.VerificationExpiresAt.Add(time.Microsecond) },
		"verified_at": func(s *Snapshot) { s.VerifiedAt = s.VerifiedAt.Add(time.Microsecond) },
		"mask":        func(s *Snapshot) { s.DisplayMask = "x" }, "amount": func(s *Snapshot) { s.Amount = "12346" },
		"asset": func(s *Snapshot) { s.AssetCode = "USD" }, "seal": func(s *Snapshot) { s.Seal = strings.Repeat("0", 64) },
		"seal_kid": func(s *Snapshot) { s.SealKID = "zz" },
	}
	for name, mut := range sm {
		c := *snap
		mut(&c)
		if err := k.VerifySnapshot(&c); !errors.Is(err, ErrSealInvalid) {
			t.Errorf("tampering snapshot %s must fail", name)
		}
	}
}

func TestCanonIsLengthPrefixed(t *testing.T) {
	// "ab","c" and "a","bc" must not collide.
	if canon(cs("ab"), cs("c")) == canon(cs("a"), cs("bc")) {
		t.Fatal("canonical encoding must be unambiguous")
	}
	if canon(nil, cs("")) == canon(cs(""), nil) {
		t.Fatal("NULL and empty must be distinguishable")
	}
	if got := canon(cs("x"), nil); got != "1:x,~" {
		t.Fatalf("canon = %q", got)
	}
	u := uuid.MustParse("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
	if *cu(u) != strings.ToLower(u.String()) {
		t.Fatal("uuids are lowercase")
	}
}

func TestKeysRedactEverywhere(t *testing.T) {
	k := newTestKeys(t)
	secret := fmt.Sprintf("%x", k.seal["m1"])
	for _, s := range []string{fmt.Sprintf("%v", k), fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", k), fmt.Sprintf("%s", k), fmt.Sprintf("%x", k), k.String(), k.GoString()} {
		if strings.Contains(s, secret[:16]) || !strings.Contains(s, "redacted") {
			t.Errorf("Keys rendering leaked or did not redact: %q", s)
		}
	}
	b, _ := k.MarshalJSON()
	if strings.Contains(string(b), secret[:16]) {
		t.Error("json leak")
	}
}
