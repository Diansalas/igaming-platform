package payoutinstrument

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// Service errors. The registration refusals are mapped to ONE generic player
// response by the route (M-7); the distinct values here exist for the audit
// row and the compliance alert only.
var (
	ErrNotFound            = errors.New("payoutinstrument: instrument not found")
	ErrFingerprintConflict = errors.New("payoutinstrument: fingerprint is owned by another person")
	ErrInvalidRegistration = errors.New("payoutinstrument: registration refused")
	ErrInUse               = errors.New("payoutinstrument: instrument is in use by a live withdrawal")
	ErrNotYours            = errors.New("payoutinstrument: instrument belongs to another player")
	ErrIntegrity           = errors.New("payoutinstrument: instrument integrity check failed")
	ErrKYCNotVerified      = errors.New("payoutinstrument: the person's KYC is not verified")
	ErrMaxAgeNotConfigured = errors.New("payoutinstrument: no verification max-age is configured for the jurisdiction")
	ErrKindDisabled        = errors.New("payoutinstrument: kind is not enabled for non-synthetic verification")
	ErrNotVerifiable       = errors.New("payoutinstrument: instrument is not in a verifiable state")
	ErrBadTransition       = errors.New("payoutinstrument: transition is not allowed from this state")
)

// Service is the payout-instrument subsystem. It is the ONLY writer of the
// payout_instrument* tables (pinned by a static test, L-7).
type Service struct {
	keys      *Keys
	kinds     *KindRegistry
	verifiers []PayoutInstrumentVerifier
}

// NewService builds the service. keys must be non-nil: there is no keyless
// mode (and no random fallback).
func NewService(keys *Keys, kinds *KindRegistry, verifiers ...PayoutInstrumentVerifier) (*Service, error) {
	if keys == nil || kinds == nil {
		return nil, fmt.Errorf("%w: keys and kinds are required", ErrKeyConfig)
	}
	return &Service{keys: keys, kinds: kinds, verifiers: append([]PayoutInstrumentVerifier(nil), verifiers...)}, nil
}

// Keys exposes the key holder to wiring (the fingerprinter for adapters).
func (s *Service) Keys() *Keys { return s.keys }

// Kinds exposes the kind registry.
func (s *Service) Kinds() *KindRegistry { return s.kinds }

// Registrations reports the verifiers for the startup gate.
func (s *Service) Registrations() []PayoutInstrumentVerifier {
	return append([]PayoutInstrumentVerifier(nil), s.verifiers...)
}

// verifierByID returns a registered verifier.
func (s *Service) verifierByID(id string) (PayoutInstrumentVerifier, bool) {
	for _, v := range s.verifiers {
		if v.ID() == id {
			return v, true
		}
	}
	return nil, false
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ---- registration -----------------------------------------------------------

// RegisterParams is a registration request. The player is resolved from the
// authenticated session by the caller, never from a body.
type RegisterParams struct {
	TenantID        uuid.UUID
	PlayerAccountID uuid.UUID
	Kind            string
	Rail            string
	AssetCodes      []string
	Detail          json.RawMessage
	// Supersedes, when set, makes this a replacing version of a same-player
	// instrument (a destination change is a new instrument, section 2.3).
	Supersedes *uuid.UUID
}

// RegisterResult is the outcome of Register.
type RegisterResult struct {
	Instrument Instrument
	// Existing: the same player already has a live instrument with the same
	// destination; it is returned unchanged (registration is idempotent).
	Existing bool
}

var activeStates = `('pending_verification','verified','verification_expired','suspended')`

// Register validates, normalises, fingerprints, encrypts and seals a new
// instrument for the player and inserts it as pending_verification.
//
// tx MUST be rolled back by the caller on any error other than success:
// owner rows for some fingerprint kids may already have been written when a
// later kid conflicts. The refusal reasons (ErrFingerprintConflict,
// ErrInvalidRegistration, ErrNotAccepted wrappers) are for the audit row; the
// player-facing response is generic.
func (s *Service) Register(ctx context.Context, tx pgx.Tx, p RegisterParams) (RegisterResult, error) {
	spec, err := s.kinds.Spec(p.Kind)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("%w: %v", ErrInvalidRegistration, err)
	}
	var kindVersion int
	var allowedRails []string
	err = tx.QueryRow(ctx, `SELECT detail_schema_version, allowed_rails FROM payout_instrument_kinds WHERE code = $1`, p.Kind).
		Scan(&kindVersion, &allowedRails)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegisterResult{}, fmt.Errorf("%w: kind is not seeded", ErrInvalidRegistration)
	}
	if err != nil {
		return RegisterResult{}, fmt.Errorf("payoutinstrument: read kind: %w", err)
	}
	if spec.SchemaVersion() != kindVersion {
		return RegisterResult{}, fmt.Errorf("%w: detail schema version mismatch", ErrInvalidRegistration)
	}
	if !contains(allowedRails, p.Rail) {
		return RegisterResult{}, fmt.Errorf("%w: rail not allowed for kind", ErrInvalidRegistration)
	}
	assets := dedupSorted(p.AssetCodes)
	if len(assets) == 0 || len(assets) > 16 {
		return RegisterResult{}, fmt.Errorf("%w: asset codes", ErrInvalidRegistration)
	}
	norm, err := spec.Normalize(p.Detail)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("%w: %v", ErrInvalidRegistration, err)
	}

	var brandID, personID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT brand_id, person_id FROM player_accounts WHERE id = $1 AND tenant_id = $2`,
		p.PlayerAccountID, p.TenantID).Scan(&brandID, &personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegisterResult{}, fmt.Errorf("%w: player account", ErrInvalidRegistration)
	}
	if err != nil {
		return RegisterResult{}, fmt.Errorf("payoutinstrument: read player account: %w", err)
	}

	// The fingerprint under EVERY retained kid (H-2).
	fps := map[string]string{}
	for _, kid := range s.keys.FingerprintKIDs() {
		fp, err := s.keys.Fingerprint(kid, p.TenantID, p.Kind, norm.FingerprintInput)
		if err != nil {
			return RegisterResult{}, err
		}
		fps[kid] = fp
	}
	activeKID := s.keys.FingerprintKID()

	// Idempotent replay: the same player, the same destination, still live.
	for _, kid := range s.keys.FingerprintKIDs() {
		existing, err := scanInstrument(tx.QueryRow(ctx,
			`SELECT `+instrumentColumns+` FROM payout_instruments
			  WHERE tenant_id = $1 AND player_account_id = $2 AND fingerprint_kid = $3 AND fingerprint = $4
			    AND state IN `+activeStates+` LIMIT 1`, p.TenantID, p.PlayerAccountID, kid, fps[kid]))
		if err == nil {
			return RegisterResult{Instrument: existing, Existing: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return RegisterResult{}, fmt.Errorf("payoutinstrument: replay lookup: %w", err)
		}
	}

	// Ownership: first Person owns the fingerprint in the tenant; another
	// Person conflicts on the PK, race-free (INSERT ... ON CONFLICT waits for a
	// concurrent inserter, then the re-read sees its row).
	for _, kid := range s.keys.FingerprintKIDs() {
		if _, err := tx.Exec(ctx,
			`INSERT INTO payout_instrument_fingerprint_owners (tenant_id, fingerprint_kid, fingerprint, person_id)
			 VALUES ($1,$2,$3,$4) ON CONFLICT (tenant_id, fingerprint_kid, fingerprint) DO NOTHING`,
			p.TenantID, kid, fps[kid], personID); err != nil {
			return RegisterResult{}, fmt.Errorf("payoutinstrument: record fingerprint owner: %w", err)
		}
		var owner uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT person_id FROM payout_instrument_fingerprint_owners WHERE tenant_id = $1 AND fingerprint_kid = $2 AND fingerprint = $3`,
			p.TenantID, kid, fps[kid]).Scan(&owner); err != nil {
			return RegisterResult{}, fmt.Errorf("payoutinstrument: read fingerprint owner: %w", err)
		}
		if owner != personID {
			return RegisterResult{}, ErrFingerprintConflict
		}
	}

	id, err := newID(ctx, tx)
	if err != nil {
		return RegisterResult{}, err
	}
	inst := Instrument{
		ID: id, TenantID: p.TenantID, BrandID: brandID, PlayerAccountID: p.PlayerAccountID, PersonID: personID,
		Kind: p.Kind, Rail: p.Rail, AssetCodes: assets, DetailSchemaVersion: kindVersion, DisplayMask: norm.Mask,
		Fingerprint: fps[activeKID], FingerprintKID: activeKID, State: StatePendingVerification, SupersedesInstrument: p.Supersedes,
	}
	ct, nonce, kid, err := s.keys.EncryptDetail(DetailAAD{TenantID: p.TenantID, InstrumentID: id, Kind: p.Kind, SchemaVersion: kindVersion}, norm.Canonical)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("%w: %v", ErrInvalidRegistration, err)
	}
	inst.DetailCiphertext, inst.DetailNonce, inst.DetailKeyKID = ct, nonce, kid
	if err := s.keys.SealInstrument(&inst); err != nil {
		return RegisterResult{}, err
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("payoutinstrument: savepoint: %w", err)
	}
	_, err = sp.Exec(ctx,
		`INSERT INTO payout_instruments
		   (id, tenant_id, brand_id, player_account_id, person_id, kind, rail, asset_codes, detail_ciphertext, detail_nonce,
		    detail_key_kid, detail_schema_version, display_mask, fingerprint, fingerprint_kid, supersedes_instrument_id,
		    instrument_seal, seal_kid)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		inst.ID, inst.TenantID, inst.BrandID, inst.PlayerAccountID, inst.PersonID, inst.Kind, inst.Rail, inst.AssetCodes,
		inst.DetailCiphertext, inst.DetailNonce, inst.DetailKeyKID, inst.DetailSchemaVersion, inst.DisplayMask,
		inst.Fingerprint, inst.FingerprintKID, inst.SupersedesInstrument, inst.InstrumentSeal, inst.SealKID)
	if err != nil {
		_ = sp.Rollback(ctx)
		if pgCode(err) == "23505" {
			// A concurrent registration of the same destination won: return it.
			existing, lerr := scanInstrument(tx.QueryRow(ctx,
				`SELECT `+instrumentColumns+` FROM payout_instruments
				  WHERE tenant_id = $1 AND player_account_id = $2 AND fingerprint_kid = $3 AND fingerprint = $4
				    AND state IN `+activeStates+` LIMIT 1`, p.TenantID, p.PlayerAccountID, inst.FingerprintKID, inst.Fingerprint))
			if lerr == nil {
				return RegisterResult{Instrument: existing, Existing: true}, nil
			}
		}
		if pc := pgCode(err); len(pc) == 5 && pc[:2] == "PI" {
			return RegisterResult{}, fmt.Errorf("%w: %s", ErrInvalidRegistration, pc)
		}
		return RegisterResult{}, fmt.Errorf("payoutinstrument: insert instrument: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return RegisterResult{}, fmt.Errorf("payoutinstrument: release savepoint: %w", err)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: p.TenantID, ActorType: audit.ActorPlayer, ActorID: p.PlayerAccountID,
		Action: "payout_instrument.registered", TargetType: "payout_instrument", TargetID: id.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{"kind": inst.Kind, "rail": inst.Rail, "display_mask": inst.DisplayMask, "fingerprint_prefix": inst.Fingerprint[:8]},
	}); err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{Instrument: inst}, nil
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func dedupSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// integrityError is an ErrIntegrity with a closed internal reason.
type integrityError struct{ reason string }

func (e *integrityError) Error() string   { return ErrIntegrity.Error() + ": " + e.reason }
func (e *integrityError) Is(t error) bool { return t == ErrIntegrity }

// ---- integrity -------------------------------------------------------------

// checkIntegrity verifies the instrument seal, decrypts the detail (AAD
// bound), re-validates it with the kind spec, and recomputes the fingerprint
// under the row's own fingerprint kid and the mask. It is run before any
// vendor call and at every gate (closes T11, SQL-planted rows). It returns the
// plaintext detail.
func (s *Service) checkIntegrity(inst *Instrument) ([]byte, error) {
	if err := s.keys.VerifyInstrument(inst); err != nil {
		return nil, &integrityError{"seal"}
	}
	pt, err := s.keys.DecryptDetail(inst.DetailKeyKID, DetailAAD{TenantID: inst.TenantID, InstrumentID: inst.ID, Kind: inst.Kind, SchemaVersion: inst.DetailSchemaVersion},
		inst.DetailCiphertext, inst.DetailNonce)
	if err != nil {
		return nil, &integrityError{"decrypt"}
	}
	spec, err := s.kinds.Spec(inst.Kind)
	if err != nil {
		return nil, &integrityError{"kind"}
	}
	norm, err := spec.Normalize(pt)
	if err != nil {
		return nil, &integrityError{"detail"}
	}
	fp, err := s.keys.Fingerprint(inst.FingerprintKID, inst.TenantID, inst.Kind, norm.FingerprintInput)
	if err != nil || !hmac.Equal([]byte(fp), []byte(inst.Fingerprint)) {
		return nil, &integrityError{"fingerprint"}
	}
	if norm.Mask != inst.DisplayMask {
		return nil, &integrityError{"mask"}
	}
	return pt, nil
}

// ---- verification ----------------------------------------------------------

// VerifyResultView is the outcome of Service.Verify.
type VerifyResultView struct {
	State    State
	Source   VerificationSource
	Verified bool
}

type verifyPlan struct {
	inst     Instrument
	detail   []byte
	verifier PayoutInstrumentVerifier
	source   VerificationSource
	maxAge   *time.Duration
}

// TxRunner is the part of db.Pool the service needs: tenant-scoped
// transactions. *db.Pool satisfies it.
type TxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error
}

// Verify runs the ownership verification of a pending (or expired) instrument
// through a registered PayoutInstrumentVerifier. Phases: (1) read + integrity
// + preconditions in a transaction; (2) the vendor call OUTSIDE any
// transaction; (3) write the verification and the state change in one
// transaction (the database requires the verification row in the same
// transaction as every transition into verified).
func (s *Service) Verify(ctx context.Context, db TxRunner, tenantID, instrumentID uuid.UUID) (VerifyResultView, error) {
	var plan verifyPlan
	err := db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		inst, err := loadInstrument(ctx, tx, tenantID, instrumentID, lockNone)
		if err != nil {
			return err
		}
		if inst.State != StatePendingVerification && inst.State != StateVerificationExpired {
			return ErrNotVerifiable
		}
		detail, err := s.checkIntegrity(&inst)
		if err != nil {
			return err
		}
		var v PayoutInstrumentVerifier
		for _, c := range s.verifiers {
			if c.Supports(inst.Kind, inst.Rail) {
				v = c
				break
			}
		}
		if v == nil {
			return ErrNoVerifier
		}
		src, err := ForcedSource(v)
		if err != nil {
			return err
		}
		plan = verifyPlan{inst: inst, detail: detail, verifier: v, source: src}
		if !src.IsSynthetic() {
			var enabled bool
			if err := tx.QueryRow(ctx, `SELECT non_synthetic_enabled FROM payout_instrument_kinds WHERE code = $1`, inst.Kind).Scan(&enabled); err != nil {
				return fmt.Errorf("payoutinstrument: read kind: %w", err)
			}
			if !enabled {
				return ErrKindDisabled
			}
			ok, err := personKYCVerified(ctx, tx, inst)
			if err != nil {
				return err
			}
			if !ok {
				return ErrKYCNotVerified
			}
		}
		ma, err := maxAgeFor(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if !src.IsSynthetic() && ma == nil {
			return ErrMaxAgeNotConfigured
		}
		plan.maxAge = ma
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrIntegrity) {
			s.auditIntegrity(ctx, db, tenantID, instrumentID)
		}
		return VerifyResultView{}, err
	}

	res, verr := plan.verifier.Verify(ctx, VerifyRequest{
		TenantID: tenantID, InstrumentID: instrumentID, Kind: plan.inst.Kind, Rail: plan.inst.Rail, Detail: NewSecretDetail(plan.detail),
	})
	if verr != nil {
		return VerifyResultView{}, fmt.Errorf("payoutinstrument: verifier %q: %w", plan.verifier.ID(), verr)
	}

	var out VerifyResultView
	err = db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		inst, err := loadInstrument(ctx, tx, tenantID, instrumentID, lockUpdate)
		if err != nil {
			return err
		}
		if inst.State != plan.inst.State || inst.InstrumentSeal != plan.inst.InstrumentSeal {
			return ErrNotVerifiable
		}
		if _, err := s.checkIntegrity(&inst); err != nil {
			return err
		}
		now, err := txNow(ctx, tx)
		if err != nil {
			return err
		}
		vid, err := newID(ctx, tx)
		if err != nil {
			return err
		}
		ok := res.Verified && (plan.source.IsSynthetic() || res.AccountHolderMatchesVerifiedIdentity)
		expires := res.SourceExpiry
		if plan.maxAge != nil && now.Add(*plan.maxAge).Before(expires) {
			expires = now.Add(*plan.maxAge)
		}
		if ok && !expires.After(now) {
			ok = false
		}
		if !ok {
			expires = now.Add(24 * time.Hour)
		}
		ver := Verification{
			ID: vid, TenantID: tenantID, InstrumentID: inst.ID, Source: plan.source, OwnershipAssertion: OwnershipFor(plan.source),
			VerifierProviderID: plan.verifier.ID(), Outcome: "rejected", VerifiedAt: now, ExpiresAt: expires,
		}
		if ok {
			ver.Outcome = "verified"
		}
		if res.ReferenceHash != "" {
			h := res.ReferenceHash
			ver.VerifierReferenceHash = &h
		}
		if err := s.keys.SealVerification(&ver, inst.Fingerprint); err != nil {
			return err
		}
		if err := insertVerification(ctx, tx, ver); err != nil {
			return err
		}
		target := inst.State
		switch {
		case ok:
			target = StateVerified
		case inst.State == StatePendingVerification:
			target = StateRejected
		}
		if target != inst.State {
			var cur any
			if ok {
				cur = ver.ID
			}
			if _, err := tx.Exec(ctx, `UPDATE payout_instruments SET state = $1, current_verification_id = COALESCE($2::uuid, current_verification_id)
				WHERE id = $3 AND tenant_id = $4`, string(target), cur, inst.ID, tenantID); err != nil {
				return fmt.Errorf("payoutinstrument: transition: %w", err)
			}
		}
		out = VerifyResultView{State: target, Source: plan.source, Verified: ok}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "payout_instrument." + string(target),
			TargetType: "payout_instrument", TargetID: inst.ID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{"verification_id": ver.ID.String(), "source": string(plan.source), "outcome": ver.Outcome, "verifier": plan.verifier.ID(), "from": string(inst.State)},
		})
	})
	if err != nil {
		return VerifyResultView{}, err
	}
	return out, nil
}

func (s *Service) auditIntegrity(ctx context.Context, db TxRunner, tenantID, instrumentID uuid.UUID) {
	_ = db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "payout_instrument.integrity_failure",
			TargetType: "payout_instrument", TargetID: instrumentID.String(), Outcome: audit.OutcomeFailure,
		})
	})
}

func maxAgeFor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (*time.Duration, error) {
	var licenceID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, tenantID).Scan(&licenceID); err != nil {
		return nil, fmt.Errorf("payoutinstrument: read tenant licence: %w", err)
	}
	if licenceID == nil {
		return nil, nil
	}
	var secs *float64
	err := tx.QueryRow(ctx, `SELECT extract(epoch FROM m.max_age)::float8
		FROM licences l JOIN payout_instrument_verification_max_age m ON m.jurisdiction_id = l.jurisdiction_id
		WHERE l.id = $1`, *licenceID).Scan(&secs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("payoutinstrument: read max age: %w", err)
	}
	d := time.Duration(*secs * float64(time.Second))
	return &d, nil
}

// personKYCVerified: the player account's LATEST KYC verification in the
// tenant/brand is approved and unexpired (A-5, fail-closed: a newer
// non-approved row, including an orphan, denies).
func personKYCVerified(ctx context.Context, tx pgx.Tx, inst Instrument) (bool, error) {
	var status string
	var expires *time.Time
	err := tx.QueryRow(ctx, `SELECT status, expires_at FROM kyc_verifications
		WHERE tenant_id = $1 AND brand_id = $2 AND player_account_id = $3
		ORDER BY created_at DESC, id DESC LIMIT 1`, inst.TenantID, inst.BrandID, inst.PlayerAccountID).Scan(&status, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("payoutinstrument: read kyc: %w", err)
	}
	return status == "approved" && (expires == nil || expires.After(time.Now())), nil
}
