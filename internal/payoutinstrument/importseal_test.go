package payoutinstrument

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// ADR 0111 4.3 (S-5; ADR 0110 T10): the statement-import seal.

type synthSrc struct{}

func (synthSrc) SyntheticComponent() {}

func importSealFixture() (statement.ImportSealInput, []statement.ImportLineCanon) {
	m := "merchant-1"
	lines := []statement.ImportLineCanon{
		{LineNo: 0, ProviderID: "mock-payments", Kind: "payout", ProviderReference: "R-1", MerchantReference: &m, Status: "succeeded", Amount: "700", AssetCode: "EUR", OccurredAt: time.Unix(1700000000, 123456000)},
		{LineNo: 1, ProviderID: "mock-payments", Kind: "payout", ProviderReference: "R-2", Status: "declined", Amount: "5", AssetCode: "EUR", OccurredAt: time.Unix(1700000100, 0)},
	}
	d := sha256.Sum256([]byte("content"))
	in := statement.ImportSealInput{ImportID: uuid.New(), TenantID: uuid.New(), ProviderID: "mock-payments", SourceLabel: "MOCK x",
		IsMock: true, CoverageStart: time.Unix(1699990000, 0), CoverageEnd: time.Unix(1700090000, 0), ContentDigest: d[:], LineCount: 2,
		FetchedAt: time.Unix(1700090001, 5000), PayoutLinesCarryMerchantReference: true, ImportedByService: "reconciliation.payment_statement",
		LinesDigest: statement.ImportLinesDigest(lines)}
	return in, lines
}

func TestImportSeal_RoundTrip_AndEveryFieldIsCovered(t *testing.T) {
	k := newTestKeys(t)
	in, lines := importSealFixture()
	seal, kid, err := k.SealStatementImport(in)
	if err != nil || kid != "m1" || len(seal) != 64 {
		t.Fatalf("seal: %q %q %v", seal, kid, err)
	}
	if err := k.VerifyStatementImport(in, kid, seal); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Every scalar field of the seal input changes the seal.
	v := reflect.ValueOf(&in).Elem()
	for i := 0; i < v.NumField(); i++ {
		mut := in
		f := reflect.ValueOf(&mut).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString(f.String() + "x")
		case reflect.Bool:
			f.SetBool(!f.Bool())
		case reflect.Int:
			f.SetInt(f.Int() + 1)
		case reflect.Slice:
			f.SetBytes(append([]byte{1}, f.Bytes()...))
		case reflect.Array: // uuid.UUID
			mut2 := uuid.New()
			f.Set(reflect.ValueOf(mut2))
		case reflect.Struct: // time.Time
			f.Set(reflect.ValueOf(f.Interface().(time.Time).Add(time.Microsecond)))
		default:
			t.Fatalf("unhandled field %s", v.Type().Field(i).Name)
		}
		if err := k.VerifyStatementImport(mut, kid, seal); !errors.Is(err, ErrImportSealInvalid) {
			t.Errorf("field %s is not covered by the seal", v.Type().Field(i).Name)
		}
	}
	// Every line column the verdict reads changes the lines digest.
	base := statement.ImportLinesDigest(lines)
	m2 := "merchant-2"
	o := "orig"
	mutators := map[string]func(l *statement.ImportLineCanon){
		"line_no":     func(l *statement.ImportLineCanon) { l.LineNo++ },
		"provider_id": func(l *statement.ImportLineCanon) { l.ProviderID = "other-psp" }, "kind": func(l *statement.ImportLineCanon) { l.Kind = "deposit" },
		"provider_reference":          func(l *statement.ImportLineCanon) { l.ProviderReference += "x" },
		"merchant_reference":          func(l *statement.ImportLineCanon) { l.MerchantReference = &m2 },
		"merchant_reference NULL":     func(l *statement.ImportLineCanon) { l.MerchantReference = nil },
		"original_provider_reference": func(l *statement.ImportLineCanon) { l.OriginalProviderReference = &o },
		"settlement_reference":        func(l *statement.ImportLineCanon) { l.SettlementReference = &o },
		"status":                      func(l *statement.ImportLineCanon) { l.Status = "pending" }, "amount": func(l *statement.ImportLineCanon) { l.Amount = "701" },
		"asset_code":  func(l *statement.ImportLineCanon) { l.AssetCode = "USD" },
		"occurred_at": func(l *statement.ImportLineCanon) { l.OccurredAt = l.OccurredAt.Add(time.Microsecond) },
	}
	for name, f := range mutators {
		cp := append([]statement.ImportLineCanon(nil), lines...)
		f(&cp[0])
		if statement.ImportLinesDigest(cp) == base {
			t.Errorf("lines digest does not cover %s", name)
		}
	}
	if statement.ImportLinesDigest(lines[:1]) == base || statement.ImportLinesDigest([]statement.ImportLineCanon{lines[1], lines[0]}) == base {
		t.Error("lines digest does not bind the line set and order")
	}
}

func TestImportSeal_KeySeparation_RetiredKid_UnknownKid(t *testing.T) {
	m1, m2, fp := randKey(t), randKey(t), randKey(t)
	k, err := NewKeys("m2", map[string][]byte{"m1": m1, "m2": m2}, "f1", map[string][]byte{"f1": fp})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := importSealFixture()
	seal, kid, err := k.SealStatementImport(in)
	if err != nil || kid != "m2" {
		t.Fatalf("active kid: %q %v", kid, err)
	}
	// A seal made under the retired kid m1 (an older process) still verifies.
	old, err := NewKeys("m1", map[string][]byte{"m1": m1}, "f1", map[string][]byte{"f1": fp})
	if err != nil {
		t.Fatal(err)
	}
	oldSeal, oldKid, _ := old.SealStatementImport(in)
	if err := k.VerifyStatementImport(in, oldKid, oldSeal); err != nil {
		t.Fatalf("retired kid must stay verify-only: %v", err)
	}
	// The import subkey is independent of the instrument-seal subkey (distinct HKDF labels).
	if instr, _ := k.sealMAC(kid, statement.ImportCanon(in)); instr == seal {
		t.Fatal("import and instrument seals share a subkey")
	}
	if err := k.VerifyStatementImport(in, "unknown", seal); !errors.Is(err, ErrImportSealInvalid) {
		t.Fatalf("unknown kid: %v", err)
	}
	var nilKeys *Keys
	if err := nilKeys.VerifyStatementImport(in, kid, seal); !errors.Is(err, ErrImportSealInvalid) {
		t.Fatalf("nil keys: %v", err)
	}
	if _, _, err := nilKeys.SealStatementImport(in); err == nil {
		t.Fatal("nil keys sealed")
	}
	if err := k.VerifyStatementImport(in, kid, ""); !errors.Is(err, ErrImportSealInvalid) {
		t.Fatalf("empty seal: %v", err)
	}
}

// S-5: the startup gate also fires whenever a non-MOCK statement source is registered.
func TestStartupGate_NonMockStatementSourceRequiresKeys(t *testing.T) {
	type realSrc struct{}
	if err := VerifyStartup("development", nil, Registrations{StatementSources: []any{realSrc{}}}); !errors.Is(err, ErrStartupGate) {
		t.Fatalf("a non-MOCK statement source without keys must refuse: %v", err)
	}
	if err := VerifyStartup("development", nil, Registrations{StatementSources: []any{synthSrc{}}}); err != nil {
		t.Fatalf("a MOCK source alone needs no keys: %v", err)
	}
	if err := VerifyStartup("development", newTestKeys(t), Registrations{StatementSources: []any{realSrc{}}}); err != nil {
		t.Fatalf("with keys: %v", err)
	}
}
