//go:build integration

package marketgate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/geolocation"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/testsupport/staffsv"
)

// Owner decision 3 (docs/plans/market-evidence-kyc-record.md): the KYC-verified
// residence is the CURRENT market-evidence source. These tests drive the real
// KYCResidenceLocator -> Gate.Locate -> Check chain against PostgreSQL (runtime
// role, RLS) with kyc_verifications rows in every status.

// kycRow is one kyc_verifications row to seed. age orders rows: a larger age
// is OLDER (created_at = now - age).
type kycRow struct {
	status    string
	country   string // "" = no residence determination
	setAge    time.Duration
	age       time.Duration
	expiresIn *time.Duration // nil = never expires; negative = already expired
	orphan    bool           // status 'unverified' with no provider reference (phase-A intent row)
}

type kycPlayer struct {
	id     uuid.UUID
	person uuid.UUID
	staff  uuid.UUID
}

func (w world) seedPlayer(t *testing.T, pool *db.Pool) kycPlayer {
	t.Helper()
	p := kycPlayer{id: uuid.New()}
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		person, err := identity.CreatePerson(ctx, tx)
		if err != nil {
			return err
		}
		p.person = person.ID
		spv, err := staffsv.PersonIfRequired(ctx, tx, string(identity.StaffRoleCompliance))
		if err != nil {
			return err
		}
		staff, err := identity.CreateStaffUser(ctx, tx, w.tenantID, "rev-"+uuid.NewString()+"@example.com", "hash", identity.StaffRoleCompliance, spv)
		if err != nil {
			return err
		}
		p.staff = staff.ID
		_, err = tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			VALUES ($1, $2, $3, $4, $5, 'x', 'active')`, p.id, w.tenantID, w.brandID, p.person, p.id.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed player: %v", err)
	}
	return p
}

func (w world) addKYC(t *testing.T, pool *db.Pool, p kycPlayer, r kycRow) {
	t.Helper()
	now := time.Now()
	var expires *time.Time
	if r.expiresIn != nil {
		e := now.Add(*r.expiresIn)
		expires = &e
	}
	var ref *string
	if !r.orphan {
		x := "ref-" + uuid.NewString()
		ref = &x
	}
	var country, source *string
	var setBy *uuid.UUID
	var setAt *time.Time
	if r.country != "" {
		country, source, setBy = &r.country, ptr("reviewer_determination"), &p.staff
		sa := now.Add(-r.setAge)
		setAt = &sa
	}
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference,
			       expires_at, created_at, updated_at,
			       verified_residence_country, verified_residence_source, verified_residence_set_by, verified_residence_set_at)
			VALUES ($1,$2,$3,$4,$5,$6,'mock',$7,$8,$9,$9,$10,$11,$12,$13)`,
			uuid.New(), w.tenantID, w.brandID, p.id, p.person, r.status, ref, expires, now.Add(-r.age), country, source, setBy, setAt)
		return err
	})
	if err != nil {
		t.Fatalf("seed kyc row %+v: %v", r, err)
	}
}

func ptr[T any](v T) *T { return &v }

func dur(d time.Duration) *time.Duration { return &d }

func (w world) locate(pool *db.Pool, playerID uuid.UUID) Evidence {
	return New(NewKYCResidenceLocator(pool)).Locate(context.Background(), LocateRequest{TenantID: w.tenantID, PlayerAccountID: playerID})
}

func (w world) checkWith(t *testing.T, pool *db.Pool, op Operation, playerID uuid.UUID, ev Evidence) Decision {
	t.Helper()
	req := w.req(op)
	req.PlayerAccountID = playerID
	d, err := w.check(pool, req, ev)
	if err != nil {
		t.Fatalf("%s: unexpected error %v", op, err)
	}
	return d
}

// A verified residence in a market the brand is permitted to serve passes at
// every player-evidenced point.
func TestKYCResidence_VerifiedResidenceInAllowedMarketPasses(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	p := w.seedPlayer(t, pool)
	w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: time.Hour, setAge: time.Hour})
	ev := w.locate(pool, p.id)
	if !ev.Known() {
		t.Fatal("an approved verification with a residence determination must produce evidence")
	}
	for _, op := range []Operation{OpDeposit, OpLaunch, OpSportsbookWager} {
		if d := w.checkWith(t, pool, op, p.id, ev); !d.Allowed {
			t.Fatalf("%s: verified MT residence in an open MT market must pass, got %+v", op, d)
		}
	}
}

// A verified residence in a closed or unlicensed market is refused with the
// existing closed code; the audit row never carries the country.
func TestKYCResidence_VerifiedResidenceInClosedOrUnlicensedMarketIsRefused(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	w.enableCeiling(t, pool, "DE")                               // licensed ceiling, tenant never opted in
	w.openMarket(t, pool, "IT")                                  // open ...
	w.tenantPolicy(t, pool, "IT", operatingmarket.StateDisabled) // ... then closed by the tenant
	for _, cc := range []string{"DE", "IT", "FR"} {              // not opted in, disabled, no ceiling at all
		p := w.seedPlayer(t, pool)
		w.addKYC(t, pool, p, kycRow{status: "approved", country: cc, age: time.Hour, setAge: time.Hour})
		ev := w.locate(pool, p.id)
		if !ev.Known() {
			t.Fatalf("%s: residence evidence itself must resolve (the POLICY refuses)", cc)
		}
		for _, op := range []Operation{OpDeposit, OpLaunch, OpSportsbookWager} {
			req := w.req(op)
			req.PlayerAccountID = p.id
			d, err := w.check(pool, req, ev)
			if err != nil || d.Allowed || d.Code != ReasonNotPermitted {
				t.Fatalf("%s/%s: want %s, got %+v err=%v", cc, op, ReasonNotPermitted, d, err)
			}
			rows := w.refusals(t, pool, req.RequestID)
			if len(rows) != 1 {
				t.Fatalf("%s/%s: want 1 audit row, got %d", cc, op, len(rows))
			}
			for k, v := range rows[0].Metadata {
				if s, ok := v.(string); ok && (s == cc || k == "country") {
					t.Fatalf("%s/%s: audit must never carry the country, found %s=%v", cc, op, k, v)
				}
			}
		}
	}
}

// Unverified / pending / review / rejected / expired / expired-by-date / no
// determination / no record: no evidence, so the gate refuses even though MT is open.
func TestKYCResidence_NoVerifiedResidenceMeansNoEvidenceAndRefusal(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	past, future := -time.Hour, time.Hour
	cases := []struct {
		name string
		rows []kycRow
	}{
		{"no kyc record at all", nil},
		{"unverified (decided intent)", []kycRow{{status: "unverified", country: "MT", age: time.Hour, setAge: time.Hour}}},
		{"pending", []kycRow{{status: "pending", country: "MT", age: time.Hour, setAge: time.Hour}}},
		{"review_required", []kycRow{{status: "review_required", country: "MT", age: time.Hour, setAge: time.Hour}}},
		{"rejected", []kycRow{{status: "rejected", country: "MT", age: time.Hour, setAge: time.Hour}}},
		{"expired status", []kycRow{{status: "expired", country: "MT", age: time.Hour, setAge: time.Hour}}},
		{"approved but expires_at passed", []kycRow{{status: "approved", country: "MT", age: time.Hour, setAge: time.Hour, expiresIn: &past}}},
		{"approved without a residence determination", []kycRow{{status: "approved", age: time.Hour}}},
		{"approved, residence only on a rejected row", []kycRow{
			{status: "approved", age: 2 * time.Hour},
			{status: "rejected", country: "MT", age: time.Hour, setAge: time.Hour}}},
		// SRME-2: no carry-forward across a rejection, an expiry or a manual-review stage.
		{"SRME-2 probe: approved MT, then rejected, then approved without a new residence", []kycRow{
			{status: "approved", country: "MT", age: 3 * time.Hour, setAge: 3 * time.Hour},
			{status: "rejected", age: 2 * time.Hour},
			{status: "approved", age: time.Hour}}},
		{"approved MT, then expired, then approved without a new residence", []kycRow{
			{status: "approved", country: "MT", age: 3 * time.Hour, setAge: 3 * time.Hour},
			{status: "expired", age: 2 * time.Hour},
			{status: "approved", age: time.Hour}}},
		{"approved MT, then review_required, then approved without a new residence", []kycRow{
			{status: "approved", country: "MT", age: 3 * time.Hour, setAge: 3 * time.Hour},
			{status: "review_required", age: 2 * time.Hour},
			{status: "approved", age: time.Hour}}},
		{"older rejected row carries a residence, newer approval has none", []kycRow{
			{status: "rejected", country: "MT", age: 2 * time.Hour, setAge: 2 * time.Hour},
			{status: "approved", age: time.Hour}}},
		{"older pending row carries a residence, newer approval has none", []kycRow{
			{status: "pending", country: "MT", age: 2 * time.Hour, setAge: 2 * time.Hour},
			{status: "approved", age: time.Hour}}},
		{"approved MT then NEWER pending re-verification", []kycRow{
			{status: "approved", country: "MT", age: 2 * time.Hour, setAge: 2 * time.Hour},
			{status: "pending", age: time.Hour}}},
		{"approved MT then NEWER rejected", []kycRow{
			{status: "approved", country: "MT", age: 2 * time.Hour, setAge: 2 * time.Hour},
			{status: "rejected", age: time.Hour}}},
		{"approved MT then NEWER expired", []kycRow{
			{status: "approved", country: "MT", age: 2 * time.Hour, setAge: 2 * time.Hour},
			{status: "expired", age: time.Hour}}},
		{"approved MT then NEWER approved that has expired", []kycRow{
			{status: "approved", country: "MT", age: 2 * time.Hour, setAge: 2 * time.Hour},
			{status: "approved", age: time.Hour, expiresIn: &past}}},
		{"residence row itself expired, newer approval fresh", []kycRow{
			{status: "approved", country: "MT", age: 2 * time.Hour, setAge: 2 * time.Hour, expiresIn: &past},
			{status: "approved", age: time.Hour, expiresIn: &future}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := w.seedPlayer(t, pool)
			for _, r := range tc.rows {
				w.addKYC(t, pool, p, r)
			}
			ev := w.locate(pool, p.id)
			if ev.Known() {
				t.Fatalf("%s: must be unknown evidence", tc.name)
			}
			// The locator itself (before the gate's own ISO validation) must not resolve either.
			sig, err := NewKYCResidenceLocator(pool).GetSignal(context.Background(), geolocation.SignalRequest{TenantID: w.tenantID, PlayerAccountID: p.id})
			if err != nil || sig.Outcome != geolocation.SignalInconclusive || sig.CountryCode != "" {
				t.Fatalf("%s: locator must report inconclusive without a country, got %+v err=%v", tc.name, sig, err)
			}
			for _, op := range []Operation{OpDeposit, OpLaunch, OpSportsbookWager} {
				if d := w.checkWith(t, pool, op, p.id, ev); d.Allowed || d.Code != ReasonEvidenceUnavailable {
					t.Fatalf("%s/%s: want refusal %s, got %+v", tc.name, op, ReasonEvidenceUnavailable, d)
				}
			}
		})
	}
}

// The remaining selection rules: the account's latest DECIDED row governs
// (a never-decided orphan intent row is ignored), and the most recent residence
// determination among approved rows wins.
func TestKYCResidence_SelectionRules(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	w.openMarket(t, pool, "IT")
	future := time.Hour

	t.Run("orphan intent row newer than an approval does not withdraw it", func(t *testing.T) {
		p := w.seedPlayer(t, pool)
		w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: 2 * time.Hour, setAge: 2 * time.Hour})
		w.addKYC(t, pool, p, kycRow{status: "unverified", orphan: true, age: time.Hour})
		if !w.locate(pool, p.id).Known() {
			t.Fatal("an undecided orphan row must not supersede a decided approval (same rule as KYC enforcement)")
		}
	})
	t.Run("most recent determination wins", func(t *testing.T) {
		p := w.seedPlayer(t, pool)
		w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: 3 * time.Hour, setAge: 3 * time.Hour})
		w.addKYC(t, pool, p, kycRow{status: "approved", country: "DE", age: 2 * time.Hour, setAge: 2 * time.Hour, expiresIn: &future})
		// DE is not an open market: the NEWER determination must be the one used.
		ev := w.locate(pool, p.id)
		if !ev.Known() {
			t.Fatal("want evidence")
		}
		if d := w.checkWith(t, pool, OpDeposit, p.id, ev); d.Allowed {
			t.Fatal("the newer determination (DE, not open) must govern, not the older MT one")
		}
	})
	t.Run("a fresh determination after a rejection restores the evidence", func(t *testing.T) {
		p := w.seedPlayer(t, pool)
		w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: 4 * time.Hour, setAge: 4 * time.Hour})
		w.addKYC(t, pool, p, kycRow{status: "rejected", age: 3 * time.Hour})
		w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: time.Hour, setAge: time.Hour})
		if !w.locate(pool, p.id).Known() {
			t.Fatal("a determination recorded AFTER the rejection must be usable")
		}
	})
	t.Run("a pending re-verification between two approvals keeps the carry-forward", func(t *testing.T) {
		p := w.seedPlayer(t, pool)
		w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: 4 * time.Hour, setAge: 4 * time.Hour})
		w.addKYC(t, pool, p, kycRow{status: "pending", age: 3 * time.Hour})
		w.addKYC(t, pool, p, kycRow{status: "approved", age: time.Hour})
		if !w.locate(pool, p.id).Known() {
			t.Fatal("pending is not a break (routine re-verification)")
		}
	})
	t.Run("approved re-verification without a new determination keeps the last determination", func(t *testing.T) {
		p := w.seedPlayer(t, pool)
		w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: 3 * time.Hour, setAge: 3 * time.Hour})
		w.addKYC(t, pool, p, kycRow{status: "approved", age: time.Hour})
		if !w.locate(pool, p.id).Known() {
			t.Fatal("a fresh approval carries forward the last reviewer determination")
		}
	})
}

// SRME-3: evidence read outside the transaction is re-read INSIDE the check's transaction. The
// sequence read-evidence -> a rejection commits -> check must refuse, for every player operation, and
// a determination changed in between governs (not the stale country).
func TestKYCResidence_EvidenceIsReReadInsideTheChecksTransaction(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	for _, op := range []Operation{OpDeposit, OpLaunch, OpSportsbookWager} {
		p := w.seedPlayer(t, pool)
		w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: time.Hour, setAge: time.Hour})
		ev := w.locate(pool, p.id)
		if !ev.Known() {
			t.Fatalf("%s: precondition: evidence read", op)
		}
		w.addKYC(t, pool, p, kycRow{status: "rejected", age: 0}) // a rejection commits after the read
		d := w.checkWith(t, pool, op, p.id, ev)
		if d.Allowed || d.Code != ReasonEvidenceUnavailable {
			t.Fatalf("%s: stale evidence must not admit after a rejection committed, got %+v", op, d)
		}
	}
	// A different determination committed in between governs the decision.
	p := w.seedPlayer(t, pool)
	w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: 2 * time.Hour, setAge: 2 * time.Hour})
	ev := w.locate(pool, p.id)
	w.addKYC(t, pool, p, kycRow{status: "approved", country: "DE", age: 0, setAge: 0}) // DE is not an open market
	if d := w.checkWith(t, pool, OpDeposit, p.id, ev); d.Allowed || d.Code != ReasonNotPermitted {
		t.Fatalf("the in-transaction country (DE) must govern, got %+v", d)
	}
	// Control: unchanged evidence still admits.
	q := w.seedPlayer(t, pool)
	w.addKYC(t, pool, q, kycRow{status: "approved", country: "MT", age: time.Hour, setAge: time.Hour})
	if d := w.checkWith(t, pool, OpLaunch, q.id, w.locate(pool, q.id)); !d.Allowed {
		t.Fatalf("control: %+v", d)
	}
	// A check whose request names a different player than the evidence was read for re-reads for THAT player.
	other := w.seedPlayer(t, pool)
	if d := w.checkWith(t, pool, OpDeposit, other.id, w.locate(pool, q.id)); d.Allowed {
		t.Fatal("evidence read for one player must not admit another player's request")
	}
}

// Another tenant's KYC data is never read: the same player id queried under a
// different tenant's scope yields no evidence, and the read refuses a
// transaction whose scope disagrees with the requested tenant.
func TestKYCResidence_TenantIsolation(t *testing.T) {
	pool := testPool(t)
	a := seedWorld(t, pool)
	b := seedWorld(t, pool)
	a.openMarket(t, pool, "MT")
	b.openMarket(t, pool, "MT")
	p := a.seedPlayer(t, pool)
	a.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: time.Hour, setAge: time.Hour})
	if !a.locate(pool, p.id).Known() {
		t.Fatal("control: tenant A reads its own player")
	}
	if b.locate(pool, p.id).Known() {
		t.Fatal("tenant B must not read tenant A's KYC (RLS + tenant predicate)")
	}
	// A transaction scoped to B, asked for A's tenant id: error, never A's row.
	err := pool.WithTenant(context.Background(), b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := kyc.ReadMarketResidenceEvidence(ctx, tx, a.tenantID, p.id)
		return err
	})
	if err == nil {
		t.Fatal("a transaction/tenant scope mismatch must be an error")
	}
	// And B's own request for A's player id through the locator is unknown.
	d := b.checkWith(t, pool, OpDeposit, p.id, b.locate(pool, p.id))
	if d.Allowed || d.Code != ReasonEvidenceUnavailable {
		t.Fatalf("tenant B with A's player: want %s, got %+v", ReasonEvidenceUnavailable, d)
	}
}

type failingReader struct{ err error }

func (f failingReader) WithTenantReadOnly(context.Context, uuid.UUID, db.TxFunc) error { return f.err }

// Fail closed on a database error: the locator returns the error and the gate
// reports unknown evidence (refusal), for a failing reader and for a closed pool.
func TestKYCResidence_DatabaseErrorFailsClosed(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	p := w.seedPlayer(t, pool)
	w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: time.Hour, setAge: time.Hour})

	l := &KYCResidenceLocator{}
	_ = l.bindReader(failingReader{err: errors.New("connection reset")})
	if _, err := l.GetSignal(context.Background(), geolocation.SignalRequest{TenantID: w.tenantID, PlayerAccountID: p.id}); err == nil {
		t.Fatal("a database error must surface as an error")
	}
	if New(l).Locate(context.Background(), LocateRequest{TenantID: w.tenantID, PlayerAccountID: p.id}).Known() {
		t.Fatal("a database error must be unknown evidence")
	}

	// A real pool that can no longer serve queries.
	closed := testPool(t)
	loc := NewKYCResidenceLocator(closed)
	closed.Close()
	ev := New(loc).Locate(context.Background(), LocateRequest{TenantID: w.tenantID, PlayerAccountID: p.id})
	if ev.Known() {
		t.Fatal("a closed pool must be unknown evidence")
	}
	if d := w.checkWith(t, pool, OpDeposit, p.id, ev); d.Allowed {
		t.Fatal("unknown evidence must refuse")
	}

	// A cancelled request context is unknown as well.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if New(NewKYCResidenceLocator(pool)).Locate(ctx, LocateRequest{TenantID: w.tenantID, PlayerAccountID: p.id}).Known() {
		t.Fatal("a cancelled context must be unknown evidence")
	}
}

// Registration rule (PD-MKT-REG-1): brand-level, no evidence, no client value.
func TestCheck_RegistrationIsBrandLevelAndNeedsNoEvidence(t *testing.T) {
	pool := testPool(t)

	t.Run("open brand registers with zero evidence", func(t *testing.T) {
		w := seedWorld(t, pool)
		w.openMarket(t, pool, "MT")
		req := w.req(OpRegistration)
		for name, ev := range map[string]Evidence{"zero": {}, "other country known": evidenceFor("FR")} {
			d, err := w.check(pool, req, ev)
			if err != nil || !d.Allowed {
				t.Fatalf("%s: want allowed, got %+v err=%v", name, d, err)
			}
		}
		if rows := w.refusals(t, pool, req.RequestID); len(rows) != 0 {
			t.Fatalf("allowed registration writes no refusal row, got %d", len(rows))
		}
	})
	t.Run("unconfigured brand refuses (fail closed)", func(t *testing.T) {
		w := seedWorld(t, pool)
		req := w.req(OpRegistration)
		d, err := w.check(pool, req, evidenceFor("MT"))
		if err != nil || d.Allowed || d.Code != ReasonNotPermitted {
			t.Fatalf("want refusal %s, got %+v err=%v", ReasonNotPermitted, d, err)
		}
		rows := w.refusals(t, pool, req.RequestID)
		if len(rows) != 1 || rows[0].Metadata["evidence"] != "brand_level" || rows[0].ActorTyp != "system" {
			t.Fatalf("want 1 system audit row with evidence=brand_level, got %+v", rows)
		}
		if _, has := rows[0].Metadata["country"]; has {
			t.Fatal("audit must not carry a country")
		}
	})
	t.Run("every country closed at the brand rung refuses", func(t *testing.T) {
		w := seedWorld(t, pool)
		w.openMarket(t, pool, "MT")
		w.brandPolicy(t, pool, "MT", operatingmarket.StateDisabled)
		if d, err := w.check(pool, w.req(OpRegistration), evidenceFor("MT")); err != nil || d.Allowed {
			t.Fatalf("want refusal, got %+v err=%v", d, err)
		}
	})
	t.Run("registration disabled at the operation rung refuses while deposit stays open", func(t *testing.T) {
		w := seedWorld(t, pool)
		w.openMarket(t, pool, "MT")
		w.operationPolicy(t, pool, "MT", "registration", nil, operatingmarket.StateDisabled)
		if d, err := w.check(pool, w.req(OpRegistration), Evidence{}); err != nil || d.Allowed {
			t.Fatalf("want registration refused, got %+v err=%v", d, err)
		}
		if d, err := w.check(pool, w.req(OpDeposit), evidenceFor("MT")); err != nil || !d.Allowed {
			t.Fatalf("deposit is a different operation, want allowed, got %+v err=%v", d, err)
		}
	})
	t.Run("one open country among closed ones suffices", func(t *testing.T) {
		w := seedWorld(t, pool)
		w.openMarket(t, pool, "MT")
		w.openMarket(t, pool, "DE")
		w.tenantPolicy(t, pool, "DE", operatingmarket.StateDisabled)
		if d, err := w.check(pool, w.req(OpRegistration), Evidence{}); err != nil || !d.Allowed {
			t.Fatalf("want allowed, got %+v err=%v", d, err)
		}
	})
	t.Run("configuration change is effective on the next registration", func(t *testing.T) {
		w := seedWorld(t, pool)
		w.openMarket(t, pool, "MT")
		w.tenantPolicy(t, pool, "MT", operatingmarket.StateDisabled)
		if d, err := w.check(pool, w.req(OpRegistration), Evidence{}); err != nil || d.Allowed {
			t.Fatalf("want refused after disable, got %+v err=%v", d, err)
		}
	})
	t.Run("another tenant's open market does not open this brand", func(t *testing.T) {
		a := seedWorld(t, pool)
		b := seedWorld(t, pool)
		a.openMarket(t, pool, "MT")
		if d, err := b.check(pool, b.req(OpRegistration), Evidence{}); err != nil || d.Allowed {
			t.Fatalf("tenant B unconfigured: want refused, got %+v err=%v", d, err)
		}
	})
	t.Run("a failed statement in the transaction is an error never an allow", func(t *testing.T) {
		w := seedWorld(t, pool)
		w.openMarket(t, pool, "MT")
		err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, `SELECT 1/0`) // aborts the transaction
			d, err := Check(ctx, tx, w.req(OpRegistration), Evidence{})
			if err == nil && d.Allowed {
				t.Fatal("must not allow on an aborted transaction")
			}
			if err == nil {
				t.Fatalf("want an error, got %+v", d)
			}
			return nil
		})
		_ = err
	})
}

// Concurrency: many simultaneous gated requests while the player's KYC is
// revoked. After the revoking row commits, no request is ever allowed.
func TestKYCResidence_ConcurrentRequestsAndRevocation(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	p := w.seedPlayer(t, pool)
	w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: time.Hour, setAge: time.Hour})
	gate := New(NewKYCResidenceLocator(pool))

	run := func(n int) (allowed, refused int) {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ev := gate.Locate(context.Background(), LocateRequest{TenantID: w.tenantID, PlayerAccountID: p.id})
				req := w.req(OpDeposit)
				req.PlayerAccountID = p.id
				d, err := w.check(pool, req, ev)
				mu.Lock()
				defer mu.Unlock()
				if err == nil && d.Allowed {
					allowed++
				} else {
					refused++
				}
			}()
		}
		wg.Wait()
		return
	}
	if a, r := run(40); a != 40 || r != 0 {
		t.Fatalf("verified player: want 40 allowed, got allowed=%d refused=%d", a, r)
	}
	w.addKYC(t, pool, p, kycRow{status: "rejected", age: 0})
	if a, r := run(40); a != 0 || r != 40 {
		t.Fatalf("after a newer rejection committed: want 40 refused, got allowed=%d refused=%d", a, r)
	}
}

// Latency: the KYC lookup is one indexed read; 200 sequential lookups and a
// 64-way burst stay within generous bounds (the vendor-free path must not
// approach DefaultLocateTimeout).
func TestKYCResidence_LookupLatencyWithinBounds(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	p := w.seedPlayer(t, pool)
	w.addKYC(t, pool, p, kycRow{status: "approved", country: "MT", age: time.Hour, setAge: time.Hour})
	gate := New(NewKYCResidenceLocator(pool))
	start := time.Now()
	for i := 0; i < 200; i++ {
		if !gate.Locate(context.Background(), LocateRequest{TenantID: w.tenantID, PlayerAccountID: p.id}).Known() {
			t.Fatal("expected evidence")
		}
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("200 sequential lookups took %s (bound 10s)", el)
	}
	start = time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate.Locate(context.Background(), LocateRequest{TenantID: w.tenantID, PlayerAccountID: p.id})
		}()
	}
	wg.Wait()
	if el := time.Since(start); el > DefaultLocateTimeout+time.Second {
		t.Fatalf("64-way burst took %s", el)
	}
}
