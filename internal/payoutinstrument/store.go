package payoutinstrument

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// instrumentColumns is the full column list in scan order.
const instrumentColumns = `id, tenant_id, brand_id, player_account_id, person_id, kind, rail, asset_codes,
	detail_ciphertext, detail_nonce, detail_key_kid, detail_schema_version, display_mask, fingerprint, fingerprint_kid,
	supersedes_instrument_id, instrument_seal, seal_kid, state, current_verification_id, created_at, state_changed_at`

func scanInstrument(row pgx.Row) (Instrument, error) {
	var i Instrument
	var state string
	err := row.Scan(&i.ID, &i.TenantID, &i.BrandID, &i.PlayerAccountID, &i.PersonID, &i.Kind, &i.Rail, &i.AssetCodes,
		&i.DetailCiphertext, &i.DetailNonce, &i.DetailKeyKID, &i.DetailSchemaVersion, &i.DisplayMask, &i.Fingerprint, &i.FingerprintKID,
		&i.SupersedesInstrument, &i.InstrumentSeal, &i.SealKID, &state, &i.CurrentVerificationID, &i.CreatedAt, &i.StateChangedAt)
	i.State = State(state)
	return i, err
}

type lockMode string

const (
	lockNone   lockMode = ""
	lockShare  lockMode = " FOR SHARE"
	lockUpdate lockMode = " FOR UPDATE"
)

// loadInstrument reads one instrument of the tenant, optionally locking it.
func loadInstrument(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, lock lockMode) (Instrument, error) {
	i, err := scanInstrument(tx.QueryRow(ctx,
		`SELECT `+instrumentColumns+` FROM payout_instruments WHERE id = $1 AND tenant_id = $2`+string(lock), id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Instrument{}, ErrNotFound
	}
	if err != nil {
		return Instrument{}, fmt.Errorf("payoutinstrument: load instrument: %w", err)
	}
	return i, nil
}

const verificationColumns = `id, tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id,
	verifier_reference_hash, outcome, verified_at, expires_at, verification_seal, seal_kid`

func scanVerification(row pgx.Row) (Verification, error) {
	var v Verification
	var src string
	err := row.Scan(&v.ID, &v.TenantID, &v.InstrumentID, &src, &v.OwnershipAssertion, &v.VerifierProviderID,
		&v.VerifierReferenceHash, &v.Outcome, &v.VerifiedAt, &v.ExpiresAt, &v.Seal, &v.SealKID)
	v.Source = VerificationSource(src)
	return v, err
}

// latestVerified returns the instrument's latest outcome=verified row.
func latestVerified(ctx context.Context, tx pgx.Tx, tenantID, instrumentID uuid.UUID) (Verification, bool, error) {
	v, err := scanVerification(tx.QueryRow(ctx,
		`SELECT `+verificationColumns+` FROM payout_instrument_verifications
		  WHERE tenant_id = $1 AND instrument_id = $2 AND outcome = 'verified'
		  ORDER BY verified_at DESC, id DESC LIMIT 1`, tenantID, instrumentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, false, nil
	}
	if err != nil {
		return Verification{}, false, fmt.Errorf("payoutinstrument: latest verification: %w", err)
	}
	return v, true, nil
}

func loadBlockingEvents(ctx context.Context, tx pgx.Tx, tenantID, instrumentID uuid.UUID) ([]BlockingEvent, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, tenant_id, instrument_id, event, actor_type, actor_id, reason_code, occurred_at, event_seal, seal_kid
		   FROM payout_instrument_blocking_events WHERE tenant_id = $1 AND instrument_id = $2 ORDER BY occurred_at, id`,
		tenantID, instrumentID)
	if err != nil {
		return nil, fmt.Errorf("payoutinstrument: load blocking events: %w", err)
	}
	defer rows.Close()
	var out []BlockingEvent
	for rows.Next() {
		var e BlockingEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.InstrumentID, &e.Event, &e.ActorType, &e.ActorID, &e.ReasonCode,
			&e.OccurredAt, &e.Seal, &e.SealKID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// txNow is the transaction time (PostgreSQL now()), which the database forces
// verified_at / occurred_at to equal.
func txNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var t time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("payoutinstrument: transaction time: %w", err)
	}
	return t, nil
}

func newID(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("payoutinstrument: allocate id: %w", err)
	}
	return id, nil
}

func insertVerification(ctx context.Context, tx pgx.Tx, v Verification) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO payout_instrument_verifications
		   (id, tenant_id, instrument_id, source, ownership_assertion, verifier_provider_id, verifier_reference_hash,
		    outcome, verified_at, expires_at, verification_seal, seal_kid, created_txid)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,0)`,
		v.ID, v.TenantID, v.InstrumentID, string(v.Source), v.OwnershipAssertion, v.VerifierProviderID,
		v.VerifierReferenceHash, v.Outcome, v.VerifiedAt, v.ExpiresAt, v.Seal, v.SealKID)
	if err != nil {
		return fmt.Errorf("payoutinstrument: insert verification: %w", err)
	}
	return nil
}

func insertBlockingEvent(ctx context.Context, tx pgx.Tx, e BlockingEvent) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO payout_instrument_blocking_events
		   (id, tenant_id, instrument_id, event, actor_type, actor_id, reason_code, occurred_at, event_seal, seal_kid, created_txid)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,0)`,
		e.ID, e.TenantID, e.InstrumentID, e.Event, e.ActorType, e.ActorID, e.ReasonCode, e.OccurredAt, e.Seal, e.SealKID)
	if err != nil {
		return fmt.Errorf("payoutinstrument: insert blocking event: %w", err)
	}
	return nil
}
