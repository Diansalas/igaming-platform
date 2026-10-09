package payoutinstrument

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

var reasonRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Actor identifies who blocks an instrument.
type Actor struct {
	Type string // ActorPlayer | ActorStaff | ActorProvider
	ID   string // uuid for player/staff, verifier id for provider
}

// BlockParams is a revoke or suspend request.
type BlockParams struct {
	TenantID     uuid.UUID
	InstrumentID uuid.UUID
	Actor        Actor
	ReasonCode   string
	// PlayerAccountID, when set, requires the instrument to belong to that
	// player (the player-facing revoke). The staff and provider paths leave it
	// zero.
	PlayerAccountID          uuid.UUID
	IP, UserAgent, RequestID string
}

func (s *Service) validateBlock(p BlockParams) error {
	if !reasonRE.MatchString(p.ReasonCode) || (p.Actor.Type != ActorPlayer && p.Actor.Type != ActorStaff && p.Actor.Type != ActorProvider) || p.Actor.ID == "" {
		return fmt.Errorf("%w: block parameters", ErrInvalidRegistration)
	}
	return nil
}

// inUse reports whether a live withdrawal binds the instrument (A-9).
func inUse(ctx context.Context, tx pgx.Tx, tenantID, instrumentID uuid.UUID) (bool, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests
		WHERE tenant_id = $1 AND payout_instrument_id = $2 AND state IN ('requested','pending_review','approved','submitted')`,
		tenantID, instrumentID).Scan(&n); err != nil {
		return false, fmt.Errorf("payoutinstrument: in-use check: %w", err)
	}
	return n > 0, nil
}

func auditActor(a Actor) (audit.ActorType, uuid.UUID) {
	switch a.Type {
	case ActorPlayer:
		id, _ := uuid.Parse(a.ID)
		return audit.ActorPlayer, id
	case ActorStaff:
		id, _ := uuid.Parse(a.ID)
		return audit.ActorStaff, id
	default:
		return audit.ActorSystem, uuid.Nil
	}
}

func (s *Service) block(ctx context.Context, tx pgx.Tx, p BlockParams, event string, target State) (Instrument, error) {
	if err := s.validateBlock(p); err != nil {
		return Instrument{}, err
	}
	inst, err := loadInstrument(ctx, tx, p.TenantID, p.InstrumentID, lockUpdate)
	if err != nil {
		return Instrument{}, err
	}
	if p.PlayerAccountID != uuid.Nil && inst.PlayerAccountID != p.PlayerAccountID {
		return Instrument{}, ErrNotFound // never reveals another player's instrument
	}
	if inst.State == target {
		return inst, nil // idempotent: already blocked, no second event
	}
	if inst.State.Terminal() {
		return inst, ErrBadTransition
	}
	if target == StateSuspended && inst.State != StateVerified && inst.State != StateVerificationExpired {
		return inst, ErrBadTransition
	}
	// A-9: a PLAYER may not revoke an instrument a live withdrawal binds.
	// Provider and compliance blocks are never refused.
	if target == StateRevoked && p.Actor.Type == ActorPlayer {
		used, err := inUse(ctx, tx, p.TenantID, inst.ID)
		if err != nil {
			return inst, err
		}
		if used {
			return inst, ErrInUse
		}
	}
	now, err := txNow(ctx, tx)
	if err != nil {
		return inst, err
	}
	eid, err := newID(ctx, tx)
	if err != nil {
		return inst, err
	}
	ev := BlockingEvent{ID: eid, TenantID: p.TenantID, InstrumentID: inst.ID, Event: event, ActorType: p.Actor.Type,
		ActorID: p.Actor.ID, ReasonCode: p.ReasonCode, OccurredAt: now}
	if err := s.keys.SealBlockingEvent(&ev); err != nil {
		return inst, err
	}
	if err := insertBlockingEvent(ctx, tx, ev); err != nil {
		return inst, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payout_instruments SET state = $1 WHERE id = $2 AND tenant_id = $3`,
		string(target), inst.ID, p.TenantID); err != nil {
		return inst, fmt.Errorf("payoutinstrument: %s: %w", event, err)
	}
	at, aid := auditActor(p.Actor)
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: p.TenantID, ActorType: at, ActorID: aid, Action: "payout_instrument." + string(target),
		TargetType: "payout_instrument", TargetID: inst.ID.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: p.IP, UserAgent: p.UserAgent, RequestID: p.RequestID,
		Metadata: map[string]any{"reason_code": p.ReasonCode, "from": string(inst.State), "to": string(target), "actor_type": p.Actor.Type, "event_id": ev.ID.String()},
	}); err != nil {
		return inst, err
	}
	inst.State = target
	return inst, nil
}

// Revoke revokes the instrument (player, provider or compliance). Idempotent.
func (s *Service) Revoke(ctx context.Context, tx pgx.Tx, p BlockParams) (Instrument, error) {
	return s.block(ctx, tx, p, EventRevoke, StateRevoked)
}

// Suspend suspends a verified or expired instrument (compliance staff or
// provider). There is no unsuspend API: an instrument leaves suspended only
// through a fresh same-transaction verification (database rule), and Verify
// does not accept a suspended instrument.
func (s *Service) Suspend(ctx context.Context, tx pgx.Tx, p BlockParams) (Instrument, error) {
	return s.block(ctx, tx, p, EventSuspend, StateSuspended)
}

// ApplyProviderBlock records an AUTHENTICATED provider revocation or
// suspension. The caller has verified the verifier callback or poll; this
// refuses a verifier that is not registered. It is blocking-only and lives in
// this package only (L-7).
func (s *Service) ApplyProviderBlock(ctx context.Context, tx pgx.Tx, tenantID, instrumentID uuid.UUID, verifierID, event, reasonCode string) (Instrument, error) {
	if _, ok := s.verifierByID(verifierID); !ok {
		return Instrument{}, fmt.Errorf("%w: unregistered verifier", ErrInvalidRegistration)
	}
	p := BlockParams{TenantID: tenantID, InstrumentID: instrumentID, Actor: Actor{Type: ActorProvider, ID: verifierID}, ReasonCode: reasonCode}
	switch event {
	case EventRevoke:
		return s.block(ctx, tx, p, EventRevoke, StateRevoked)
	case EventSuspend:
		return s.block(ctx, tx, p, EventSuspend, StateSuspended)
	}
	return Instrument{}, ErrBadTransition
}

// ---- sweep -----------------------------------------------------------------

// SweepResult counts what one sweep did.
type SweepResult struct {
	Expired    int
	Superseded int
}

// Sweep is the ONLY writer of verified -> verification_expired (L-4; gates
// never write expiry, they only refuse on expires_at <= now()), and it
// completes pending supersessions (a verified replacement whose predecessor
// is no longer in use).
func (s *Service) Sweep(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) (SweepResult, error) {
	var res SweepResult
	rows, err := tx.Query(ctx, `SELECT i.id FROM payout_instruments i
		JOIN payout_instrument_verifications v ON v.id = i.current_verification_id AND v.tenant_id = i.tenant_id
		WHERE i.tenant_id = $1 AND i.state = 'verified' AND v.expires_at <= now()
		ORDER BY v.expires_at LIMIT $2 FOR UPDATE OF i SKIP LOCKED`, tenantID, limit)
	if err != nil {
		return res, fmt.Errorf("payoutinstrument: sweep select: %w", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE payout_instruments SET state = 'verification_expired' WHERE id = $1 AND tenant_id = $2`, id, tenantID); err != nil {
			return res, fmt.Errorf("payoutinstrument: expire: %w", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "payout_instrument.verification_expired",
			TargetType: "payout_instrument", TargetID: id.String(), Outcome: audit.OutcomeSuccess,
		}); err != nil {
			return res, err
		}
		res.Expired++
	}

	srows, err := tx.Query(ctx, `SELECT old.id FROM payout_instruments old
		JOIN payout_instruments r ON r.supersedes_instrument_id = old.id AND r.tenant_id = old.tenant_id AND r.state = 'verified'
		WHERE old.tenant_id = $1 AND old.state IN ('verified','verification_expired','suspended')
		  AND NOT EXISTS (SELECT 1 FROM withdrawal_requests w WHERE w.tenant_id = old.tenant_id AND w.payout_instrument_id = old.id
		                    AND w.state IN ('requested','pending_review','approved','submitted'))
		ORDER BY old.id LIMIT $2 FOR UPDATE OF old SKIP LOCKED`, tenantID, limit)
	if err != nil {
		return res, fmt.Errorf("payoutinstrument: supersession select: %w", err)
	}
	var olds []uuid.UUID
	for srows.Next() {
		var id uuid.UUID
		if err := srows.Scan(&id); err != nil {
			srows.Close()
			return res, err
		}
		olds = append(olds, id)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return res, err
	}
	for _, id := range olds {
		if _, err := tx.Exec(ctx, `UPDATE payout_instruments SET state = 'superseded' WHERE id = $1 AND tenant_id = $2`, id, tenantID); err != nil {
			return res, fmt.Errorf("payoutinstrument: supersede: %w", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "payout_instrument.superseded",
			TargetType: "payout_instrument", TargetID: id.String(), Outcome: audit.OutcomeSuccess,
		}); err != nil {
			return res, err
		}
		res.Superseded++
	}
	return res, nil
}

// ---- views -----------------------------------------------------------------

// View is the only instrument shape any API returns: the mask and the state,
// never the detail, the ciphertext, the fingerprint or a seal.
type View struct {
	ID             uuid.UUID  `json:"id"`
	PlayerAccount  *uuid.UUID `json:"player_account_id,omitempty"`
	Kind           string     `json:"kind"`
	Rail           string     `json:"rail"`
	AssetCodes     []string   `json:"asset_codes"`
	DisplayMask    string     `json:"display_mask"`
	State          State      `json:"state"`
	StateChangedAt time.Time  `json:"state_changed_at"`
	CreatedAt      time.Time  `json:"created_at"`
	// Verification fields (staff and player both see their own).
	VerificationSource *VerificationSource `json:"verification_source,omitempty"`
	VerifiedAt         *time.Time          `json:"verified_at,omitempty"`
	VerificationExpiry *time.Time          `json:"verification_expires_at,omitempty"`
}

// ListForPlayer lists a player's instruments (any state), newest first.
func (s *Service) ListForPlayer(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, includePlayerID bool) ([]View, error) {
	rows, err := tx.Query(ctx, `
		SELECT i.id, i.player_account_id, i.kind, i.rail, i.asset_codes, i.display_mask, i.state, i.state_changed_at, i.created_at,
		       v.source, v.verified_at, v.expires_at
		  FROM payout_instruments i
		  LEFT JOIN payout_instrument_verifications v ON v.id = i.current_verification_id AND v.tenant_id = i.tenant_id
		 WHERE i.tenant_id = $1 AND i.player_account_id = $2
		 ORDER BY i.created_at DESC, i.id DESC LIMIT 200`, tenantID, playerAccountID)
	if err != nil {
		return nil, fmt.Errorf("payoutinstrument: list: %w", err)
	}
	defer rows.Close()
	out := []View{}
	for rows.Next() {
		var v View
		var state string
		var src *string
		var pa uuid.UUID
		if err := rows.Scan(&v.ID, &pa, &v.Kind, &v.Rail, &v.AssetCodes, &v.DisplayMask, &state, &v.StateChangedAt, &v.CreatedAt,
			&src, &v.VerifiedAt, &v.VerificationExpiry); err != nil {
			return nil, err
		}
		v.State = State(state)
		if src != nil {
			vs := VerificationSource(*src)
			v.VerificationSource = &vs
		}
		if includePlayerID {
			v.PlayerAccount = &pa
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ViewOf renders one instrument (no verification fields).
func ViewOf(i Instrument) View {
	return View{ID: i.ID, Kind: i.Kind, Rail: i.Rail, AssetCodes: i.AssetCodes, DisplayMask: i.DisplayMask, State: i.State,
		StateChangedAt: i.StateChangedAt, CreatedAt: i.CreatedAt}
}

// ---- re-fingerprint job ----------------------------------------------------

// RefingerprintResult reports one run of the re-fingerprint job.
type RefingerprintResult struct {
	Instruments int
	Conflicts   int
}

// Refingerprint is the ADR 0111 2.2 re-fingerprint job for ONE tenant: for
// every non-terminal instrument it decrypts the detail, computes the
// fingerprint under newKid and inserts the owner row under newKid, counting a
// conflict when the fingerprint under newKid is already owned by another
// Person. A new fingerprint kid may become the ACTIVE kid only after this has
// run with zero conflicts for every non-terminal instrument (runbook). It
// never changes an instrument row.
func (s *Service) Refingerprint(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, newKid string) (RefingerprintResult, error) {
	var res RefingerprintResult
	if !contains(s.keys.FingerprintKIDs(), newKid) {
		return res, ErrUnknownKID
	}
	rows, err := tx.Query(ctx, `SELECT `+instrumentColumns+` FROM payout_instruments
		WHERE tenant_id = $1 AND state IN ('pending_verification','verified','verification_expired','suspended') ORDER BY id`, tenantID)
	if err != nil {
		return res, fmt.Errorf("payoutinstrument: refingerprint select: %w", err)
	}
	var insts []Instrument
	for rows.Next() {
		i, err := scanInstrument(rows)
		if err != nil {
			rows.Close()
			return res, err
		}
		insts = append(insts, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for k := range insts {
		inst := &insts[k]
		pt, err := s.checkIntegrity(inst)
		if err != nil {
			return res, err
		}
		spec, err := s.kinds.Spec(inst.Kind)
		if err != nil {
			return res, err
		}
		norm, err := spec.Normalize(pt)
		if err != nil {
			return res, err
		}
		fp, err := s.keys.Fingerprint(newKid, tenantID, inst.Kind, norm.FingerprintInput)
		if err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payout_instrument_fingerprint_owners (tenant_id, fingerprint_kid, fingerprint, person_id)
			VALUES ($1,$2,$3,$4) ON CONFLICT (tenant_id, fingerprint_kid, fingerprint) DO NOTHING`, tenantID, newKid, fp, inst.PersonID); err != nil {
			return res, fmt.Errorf("payoutinstrument: refingerprint owner: %w", err)
		}
		var owner uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT person_id FROM payout_instrument_fingerprint_owners
			WHERE tenant_id = $1 AND fingerprint_kid = $2 AND fingerprint = $3`, tenantID, newKid, fp).Scan(&owner); err != nil {
			return res, err
		}
		res.Instruments++
		if owner != inst.PersonID {
			res.Conflicts++
		}
	}
	return res, nil
}

var _ = errors.Is
