//go:build integration

// PAY-PAYOUT-UNBOUND-RESOLVE-1 (ADR 0111 section 4, migration 0125) shared
// fixtures. Every world is a K3 world (a private, fully migrated scratch
// database, runtime-shaped non-superuser non-BYPASSRLS role) plus a test-only
// random import-seal key family. Statement imports are written through the REAL
// importer (reconciliation.IngestPaymentStatementSealed) in the REAL system
// session shape, from MOCK (Synthetic) sources unless a case needs a
// real-shaped one for the RC-3 eligibility rule. No real provider, no AWS.
package payments

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

type m4World struct {
	*k3World
	keys    *payoutinstrument.Keys
	acting3 k3Staff // a third platform approver (races)
}

func m4NewKeys(t *testing.T) *payoutinstrument.Keys {
	t.Helper()
	m, fp := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(m); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(fp); err != nil {
		t.Fatal(err)
	}
	k, err := payoutinstrument.NewKeys("m4-test", map[string][]byte{"m4-test": m}, "m4-fp", map[string][]byte{"m4-fp": fp})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newM4World(t *testing.T) *m4World {
	t.Helper()
	w := newK3World(t, k3Opts{base: 1})
	keys := m4NewKeys(t)
	w.svc.WithImportSealKeys(keys)
	m := &m4World{k3World: w, keys: keys}
	m.acting3 = w.staffMember(uuid.Nil, "platform_admin")
	w.grantActing(m.acting3, capability.CapabilityPaymentForceResolveApprove)
	return m
}

// m4Source is a fixed statement source with the S-3 declaration.
type m4Source struct {
	k3Source
	carries bool
}

func (s m4Source) PayoutLinesCarryMerchantReference() bool { return s.carries }

type m4MockSource struct{ m4Source }

func (m4MockSource) SyntheticComponent() {}

// m4Imp describes one import.
type m4Imp struct {
	real       bool                   // a real-shaped (is_mock = false) source
	unsealed   bool                   // stored without a seal
	noDecl     bool                   // the source does NOT declare payout_lines_carry_merchant_reference
	start, end time.Time              // coverage; zero = [now-1h, now+1m)
	keys       *payoutinstrument.Keys // sealer override (nil = the world's)
}

// ingest stores one import through the REAL importer in the system session.
func (m *m4World) ingest(o m4Imp, lines ...statement.PaymentStatementLine) uuid.UUID {
	m.t.Helper()
	st, en := o.start, o.end
	if st.IsZero() {
		st = time.Now().Add(-time.Hour).UTC()
	}
	if en.IsZero() {
		en = time.Now().Add(time.Minute).UTC()
	}
	st, en = st.UTC().Truncate(time.Microsecond), en.UTC().Truncate(time.Microsecond)
	src := m4Source{k3Source: k3Source{provider: m.provider, real: o.real, start: st, end: en, lines: lines}, carries: !o.noDecl}
	var source statement.PaymentStatementSource = src
	if !o.real {
		source = m4MockSource{src}
	}
	stmt := statement.PaymentStatement{CoverageStart: st, CoverageEnd: en, Lines: append([]statement.PaymentStatementLine(nil), lines...)}
	for i := range stmt.Lines {
		stmt.Lines[i].OccurredAt = stmt.Lines[i].OccurredAt.UTC().Truncate(time.Microsecond)
	}
	var sealer statement.ImportSealer = m.keys
	if o.keys != nil {
		sealer = o.keys
	}
	if o.unsealed {
		sealer = nil // an untyped nil interface: stored unsealed
	}
	var id uuid.UUID
	if err := m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, _, err = reconciliation.IngestPaymentStatementSealed(ctx, tx, m.f.tenantID, source, stmt, time.Now(), sealer)
		return err
	}); err != nil {
		m.t.Fatalf("ingest: %v", err)
	}
	return id
}

// line is a payout statement line of this provider.
func (m *m4World) line(ref, merchant, status string, amount int64, at time.Time) statement.PaymentStatementLine {
	return statement.PaymentStatementLine{ProviderID: m.provider, ProviderReference: ref, MerchantReference: merchant,
		Kind: statement.PaymentLinePayout, Status: status, Amount: amount, AssetCode: "EUR", OccurredAt: at.UTC()}
}

// park returns an UNBOUND payout park (invalid_provider_reference:*, NO reference)
// made by the REAL phase C writer.
func (m *m4World) park(amount int64) *b11Parked {
	m.t.Helper()
	p := m.b11ParkSync(amount, OutcomePending)
	if p.fresh.ProviderReference != nil || p.fresh.LastSentAt == nil {
		m.t.Fatalf("setup: want an unbound park with last_sent_at, got ref=%v last_sent_at=%v", p.fresh.ProviderReference, p.fresh.LastSentAt)
	}
	return p
}

// evidence evaluates payout_m4_evidence in a VALID acting session.
func (m *m4World) evidence(attemptID uuid.UUID) (M4Evidence, error) {
	m.t.Helper()
	var ev M4Evidence
	err := m.pool.WithPlatformActingInTenant(context.Background(), m.acting.ID, m.f.tenantID, uuid.Nil, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ev, err = EvaluateM4Evidence(ctx, tx, m.f.tenantID, attemptID)
		return err
	})
	return ev, err
}

func (m *m4World) mustEvidence(attemptID uuid.UUID, want string) M4Evidence {
	m.t.Helper()
	ev, err := m.evidence(attemptID)
	if err != nil {
		m.t.Fatalf("evidence: %v", err)
	}
	if ev.Verdict != want {
		m.t.Fatalf("verdict: want %s, got %s (line=%v ref=%v imports=%v)", want, ev.Verdict, ev.LineID, ev.Reference, ev.ImportIDs)
	}
	return ev
}

func (m *m4World) m4In(attemptID uuid.UUID, kind ResolutionKind, line *uuid.UUID) ResolutionRequestInput {
	in := ResolutionRequestInput{AttemptID: attemptID, Kind: kind, BasisCode: BasisProviderConfirmedOutOfBand,
		EvidenceRefHash: k3EvidenceHash(), ReasonCode: "m4-test", Note: "M4 test (MOCK)"}
	if line != nil {
		in.EvidenceLineID = *line
	}
	return in
}

// paidPark is a park with ONE sealed MOCK succeeded line naming its merchant
// reference under R, and its paid verdict.
func (m *m4World) paidPark(amount int64) (*b11Parked, string, M4Evidence) {
	m.t.Helper()
	p := m.park(amount)
	r := "m4-R-" + uuid.NewString()[:12]
	m.ingest(m4Imp{}, m.line(r, p.fresh.MerchantReference, statement.PaymentStatusSucceeded, amount, time.Now()))
	return p, r, m.mustEvidence(p.fresh.ID, M4VerdictPaid)
}

// notPaidPark is a park with ONE sealed MOCK import covering [created_at,
// last_sent_at + 24h] holding a declined line on its merchant reference.
func (m *m4World) notPaidPark(amount int64) (*b11Parked, M4Evidence) {
	m.t.Helper()
	p := m.park(amount)
	m.ingestDecline(p, amount, statement.PaymentStatusDeclined)
	return p, m.mustEvidence(p.fresh.ID, M4VerdictNotPaid)
}

func (m *m4World) ingestDecline(p *b11Parked, amount int64, status string) uuid.UUID {
	m.t.Helper()
	return m.ingest(m4Imp{start: p.fresh.CreatedAt.Add(-time.Minute), end: p.fresh.LastSentAt.Add(25 * time.Hour)},
		m.line("m4-D-"+uuid.NewString()[:12], p.fresh.MerchantReference, status, amount, time.Now()))
}

// execute requests as acting (platform) and approves as acting2 (platform).
func (m *m4World) execute(p *b11Parked, kind ResolutionKind, ev M4Evidence) ResolutionOutcome {
	m.t.Helper()
	r, err := m.request(m.acting, m.m4In(p.fresh.ID, kind, ev.LineID))
	if err != nil {
		m.t.Fatalf("request %s: %v", kind, err)
	}
	out, err := m.decide(m.acting2, r, ResolutionApprove)
	if err != nil {
		m.t.Fatalf("approve %s: %v", kind, err)
	}
	if !out.Executed {
		m.t.Fatalf("%s did not execute (counted %d of %d, refused=%v)", kind, out.Counted, out.Required, out.Refused)
	}
	return out
}

// tombstone posts a tombstone ledger row on (provider, ref).
func (m *m4World) tombstone(ref string) {
	m.t.Helper()
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		pid := m.provider
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: m.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "m4-tomb:" + ref, ProviderID: &pid, ProviderTxID: &ref, CorrelationID: uuid.New()})
		return err
	})
}

// idemKeyOnly posts a casino win whose IDEMPOTENCY KEY is provider:ref but whose
// provider tx id is unrelated (the "no ledger idempotency_key = provider:R" leg).
func (m *m4World) idemKeyOnly(ref string) {
	m.t.Helper()
	m.tx(func(ctx context.Context, tx pgx.Tx) error {
		wallet := m.f.walletID
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, m.f.tenantID,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		pid, ptx := "m4-other-casino", uuid.NewString()
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: m.f.tenantID, TransactionType: ledger.TxCasinoWin,
			IdempotencyKey: m.provider + ":" + ref, ProviderID: &pid, ProviderTxID: &ptx, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{{LedgerAccountID: ids[1], Direction: ledger.Debit, Amount: 1},
				{LedgerAccountID: ids[0], Direction: ledger.Credit, Amount: 1}}})
		return err
	})
}

func (m *m4World) wd(id uuid.UUID) withdrawal.WithdrawalRequest { return m.withdrawalOf(id) }
