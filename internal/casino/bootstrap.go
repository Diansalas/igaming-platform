// CAS-PLAY-BOOTSTRAP-1 (PRH-2 workstream B; ADR 0103, ACCEPTED). The
// vendor launch-token bootstrap/consume endpoint: the FIRST production
// caller that ever moves a casino_launch_sessions row from 'active' to
// 'consumed'. Everything here follows ADR 0103 section by section; each
// function/branch below cites the section it implements.
//
// ResolveLaunchToken (launch.go) is deliberately NOT reused (ADR 0103 §2,
// §10 "Alternatives rejected"): it checks no provider/mode/asset binding,
// lazily writes 'expired' on refusal, and its consume predicate is id+
// status only - all three are exactly what S-5 forbids for a vendor-facing
// bootstrap. It remains in place for two callers: LaunchGame's own tests
// exercising the pre-bootstrap contract, and any future non-HTTP internal
// use; casino's own call is to delete it once B has shipped (ADR 0103 §11
// item 5) - not done in this change, since ResolveLaunchToken still has
// test callers this change does not touch.
package casino

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// bootstrapRequestIDPattern is ADR 0103 §3.1's own request_id charset,
// mirrored exactly by migration 0115's CHECK constraint.
var bootstrapRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// BootstrapRefusalReason is ADR 0103 §3.6's closed, server-side-log-only
// vocabulary - never sent to the vendor (every step 1-3 refusal and every
// non-matching replay gets the SAME uniform response, BS-8).
type BootstrapRefusalReason string

const (
	BootstrapRefusalNotFound        BootstrapRefusalReason = "not_found"
	BootstrapRefusalBindingMismatch BootstrapRefusalReason = "binding_mismatch"
	BootstrapRefusalNotActive       BootstrapRefusalReason = "not_active"
	BootstrapRefusalExpired         BootstrapRefusalReason = "expired"
	BootstrapRefusalReplayMismatch  BootstrapRefusalReason = "replay_mismatch"
	BootstrapRefusalReplayRevoked   BootstrapRefusalReason = "replay_revoked"
)

// ErrBootstrapRefused is the sentinel every step 1-3 refusal and every
// non-matching replay wraps (ADR 0103 §3.6/BS-8). The HTTP layer maps
// errors.Is(err, ErrBootstrapRefused) - and separately, any
// *webhookauth.AuthError from redeemVerified - to the SAME uniform 401
// "callback rejected".
var ErrBootstrapRefused = errors.New("casino: launch bootstrap refused")

// BootstrapRefusedError carries the closed server-side reason (never
// rendered to the vendor) behind ErrBootstrapRefused.
type BootstrapRefusedError struct {
	Reason BootstrapRefusalReason
}

func (e *BootstrapRefusedError) Error() string {
	return fmt.Sprintf("casino: launch bootstrap refused: %s", e.Reason)
}
func (e *BootstrapRefusedError) Unwrap() error { return ErrBootstrapRefused }

// ErrBootstrapInvariantBroken is returned (mapped to 5xx, never the
// uniform 401 or the constant 403) when a definitive gate denial's own
// RevokeLaunchSession call does not return the expected prior status and
// revoked=true under the row lock (ADR 0103 §3.3: "The revoke must return
// true. A false return under the row lock means an invariant was broken:
// roll back and return 5xx.").
var ErrBootstrapInvariantBroken = errors.New("casino: launch bootstrap revoke invariant broken")

// BootstrapResult is BootstrapLaunch's success/denial outcome - "Denied is
// a result, never a Go error" (LaunchGameResult's own established
// convention in this package, reused here for ADR 0103 §3.3's identical
// shape: a gate denial is a considered, COMMITTED decision, not a rolled-
// back transaction).
type BootstrapResult struct {
	SessionID      uuid.UUID
	PlayerRef      uuid.UUID
	ProviderGameID string
	AssetCode      string
	Mode           GameMode
	// ResponseJSON is the exact byte sequence the HTTP layer returns -
	// marshaled once on a fresh success, or read back verbatim from
	// casino_launch_bootstraps.response on a replay (BS-4: "a replay
	// returns the stored response... byte-identical").
	ResponseJSON []byte
	// Replayed is true when this result came from an existing
	// idempotency row (ADR 0103 §3.4), never a fresh CAS.
	Replayed bool
	// Denied is true for a step-4 definitive gate denial (ADR 0103 §3.3).
	// The session was REVOKED and the denial audited in the SAME,
	// COMMITTED transaction - this is not a Go error.
	Denied bool
	// DeniedReason is one of "game_inactive", "capability_denied",
	// "rg_ineligible" - set only when Denied is true.
	DeniedReason string
}

// bootstrapRequestBody is ADR 0103 §3.1's exact signed-body shape.
// DisallowUnknownFields refuses a body carrying tenant_id or any other
// field the ADV suite names - the tenant is server-side only (the
// credential/route), never accepted from the body.
type bootstrapRequestBody struct {
	LaunchToken    string `json:"launch_token"`
	RequestID      string `json:"request_id"`
	ProviderGameID string `json:"provider_game_id"`
	AssetCode      string `json:"asset_code"`
	Mode           string `json:"mode"`
}

// bootstrapResponseBody is ADR 0103 §3.2 step 7's exact response shape -
// also, verbatim, the JSONB stored in casino_launch_bootstraps.response
// and replayed byte-for-byte on a matching retry.
type bootstrapResponseBody struct {
	SessionID      string `json:"session_id"`
	PlayerRef      string `json:"player_ref"`
	ProviderGameID string `json:"provider_game_id"`
	AssetCode      string `json:"asset_code"`
	Mode           string `json:"mode"`
}

// bootstrapRequestDigest is ADR 0103 §3.4/SB-6's request_digest:
// SHA-256 over a length-prefixed canonical encoding of the verified,
// parsed fields (never the token - token_hash is the separate, only
// token-derived binding). Length-prefixing each field (mirroring
// internal/idempotency's ComposeOccurrenceKey rationale) means no two
// distinct field tuples can ever collide on the same digest through
// concatenation ambiguity.
func bootstrapRequestDigest(providerID, requestID, providerGameID, assetCode, mode string) string {
	h := sha256.New()
	for _, f := range []string{providerID, requestID, providerGameID, assetCode, mode} {
		_, _ = fmt.Fprintf(h, "%d:", len(f))
		_, _ = h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// bootstrapRecord mirrors one casino_launch_bootstraps row - just the
// columns BootstrapLaunch itself needs to read back.
type bootstrapRecord struct {
	LaunchSessionID uuid.UUID
	TokenHash       string
	RequestDigest   string
	PlayerRef       uuid.UUID
	Response        []byte
}

// getBootstrapByRequestID looks up the idempotency row for (tenantID,
// providerID, requestID) - ADR 0103 §3.2 step 2b's second lookup. tx must
// already be tenant-scoped.
func getBootstrapByRequestID(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, requestID string) (bootstrapRecord, bool, error) {
	var r bootstrapRecord
	err := tx.QueryRow(ctx,
		`SELECT launch_session_id, token_hash, request_digest, player_ref, response
		 FROM casino_launch_bootstraps
		 WHERE tenant_id = $1 AND provider_id = $2 AND request_id = $3`,
		tenantID, providerID, requestID,
	).Scan(&r.LaunchSessionID, &r.TokenHash, &r.RequestDigest, &r.PlayerRef, &r.Response)
	if errors.Is(err, pgx.ErrNoRows) {
		return bootstrapRecord{}, false, nil
	}
	if err != nil {
		return bootstrapRecord{}, false, fmt.Errorf("casino: bootstrap idempotency lookup: %w", err)
	}
	return r, true, nil
}

// getLaunchSessionForBootstrap is ADR 0103 §3.2 step 2b's first lookup:
// SELECT ... WHERE token_hash = $h AND tenant_id = $t FOR UPDATE - no
// provider/mode/asset predicate here (that is step 3's job, and step 5's
// CAS predicate again, in depth); the row lock is what serializes every
// concurrent bootstrap attempt against the SAME token (ADR 0103 §4).
func getLaunchSessionForBootstrap(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, tokenHash string) (LaunchSession, bool, error) {
	var s LaunchSession
	var status LaunchSessionStatus
	var jurisdictionCode *string
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, player_account_id, wallet_id, game_id, provider_id, provider_game_id,
			asset_code, mode, jurisdiction_code, status, expires_at
		 FROM casino_launch_sessions
		 WHERE token_hash = $1 AND tenant_id = $2
		 FOR UPDATE`,
		tokenHash, tenantID,
	).Scan(&s.ID, &s.TenantID, &s.BrandID, &s.PlayerAccountID, &s.WalletID, &s.GameID, &s.ProviderID, &s.ProviderGameID,
		&s.AssetCode, &s.Mode, &jurisdictionCode, &status, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LaunchSession{}, false, nil
	}
	if err != nil {
		return LaunchSession{}, false, fmt.Errorf("casino: bootstrap session lookup: %w", err)
	}
	if jurisdictionCode != nil {
		s.JurisdictionCode = *jurisdictionCode
	}
	s.Status = status
	return s, true, nil
}

// casConsumeSessionForBootstrap is ADR 0103 §3.2 step 5's binding-aware
// CAS (SB-2): every binding predicate - provider, tenant, mode, asset,
// PROVIDER GAME - sits inside the UPDATE's own WHERE, not checked
// separately beforehand and trusted. Zero rows updated (a concurrent
// resolver won the race, or the row changed under us between step 3's
// read and this UPDATE) returns uuid.Nil, nil - the caller treats that as
// the uniform refusal, never a Go error.
func casConsumeSessionForBootstrap(ctx context.Context, tx pgx.Tx, id, tenantID uuid.UUID, providerID string, mode GameMode, assetCode, providerGameID, tokenHash string) (uuid.UUID, error) {
	var consumedID uuid.UUID
	err := tx.QueryRow(ctx,
		`UPDATE casino_launch_sessions
		    SET status = 'consumed', consumed_at = now()
		  WHERE id = $1 AND token_hash = $2 AND tenant_id = $3 AND provider_id = $4
		    AND mode = $5 AND asset_code = $6 AND provider_game_id = $7
		    AND status = 'active' AND expires_at > now()
		 RETURNING id`,
		id, tokenHash, tenantID, providerID, string(mode), assetCode, providerGameID,
	).Scan(&consumedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("casino: bootstrap cas consume: %w", err)
	}
	return consumedID, nil
}

// upsertProviderPlayerRef is ADR 0103 §3.5: created on first bootstrap,
// stable thereafter. INSERT ... ON CONFLICT DO NOTHING, then always SELECT
// - never an UPDATE (casino_provider_player_refs is append-only, migration
// 0115; an ON CONFLICT DO UPDATE arm would fire the table's own deny-
// mutation trigger).
func upsertProviderPlayerRef(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, playerAccountID uuid.UUID) (uuid.UUID, error) {
	if _, err := tx.Exec(ctx,
		`INSERT INTO casino_provider_player_refs (tenant_id, provider_id, player_account_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, provider_id, player_account_id) DO NOTHING`,
		tenantID, providerID, playerAccountID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("casino: upsert provider player ref: %w", err)
	}
	var ref uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT player_ref FROM casino_provider_player_refs WHERE tenant_id = $1 AND provider_id = $2 AND player_account_id = $3`,
		tenantID, providerID, playerAccountID,
	).Scan(&ref); err != nil {
		return uuid.Nil, fmt.Errorf("casino: read provider player ref: %w", err)
	}
	return ref, nil
}

// bootstrapGateDenialReason re-checks ADR 0103 §3.2 step 4's three gates,
// in Q4's own explicit scope: game status and capability (mirroring
// LaunchGame's identical checks exactly), and RG eligibility. It does NOT
// re-run risk or jurisdiction resolution (ADR 0103 §11 item 4, Q4
// ACCEPTED) - postBet is what re-checks risk with the session's own
// frozen jurisdiction on every bet. Returns ("", nil) when every gate
// passes.
func bootstrapGateDenialReason(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, session LaunchSession) (string, error) {
	game, err := GetGameByID(ctx, tx, session.GameID)
	if err != nil {
		return "", err
	}
	if game.Status != GameStatusActive {
		return "game_inactive", nil
	}

	capability, found, err := LoadCapability(ctx, tx, tenantID, session.BrandID, session.ProviderID)
	if err != nil {
		return "", err
	}
	if !found || capability.Status != CapabilityActive || !capability.SupportsLaunch || !containsString(capability.SupportedAssets, session.AssetCode) {
		return "capability_denied", nil
	}
	if session.Mode == ModeReal && !capability.SupportsBet {
		return "capability_denied", nil
	}

	decision, err := evaluateAndAuditEligibility(ctx, tx, tenantID, session.BrandID, session.PlayerAccountID, session.WalletID, "casino.launch_bootstrap_denied_by_rg")
	if err != nil {
		return "", err
	}
	if !decision.Allowed {
		return "rg_ineligible", nil
	}
	return "", nil
}

// BootstrapLaunch is the vendor launch-token bootstrap/consume endpoint's
// domain logic (ADR 0103 §3.2), phase 2 of the two-phase verification
// (ADR 0094 §4.1) - the HTTP handler runs phase 1 (VerifyCallback, no
// transaction held) and passes the resulting *VerifiedCallback here. This
// function owns the ONE transaction the whole contract runs inside
// (deps.DB.WithTenant in the ADR's own words).
//
// Every step 1-3 refusal and every non-matching replay returns a non-nil
// error (*BootstrapRefusedError, or the *webhookauth.AuthError
// redeemVerified/Redeem itself can return) - the transaction rolls back,
// and nothing is written (BS-2). A step-4 definitive gate denial returns
// (BootstrapResult{Denied: true, ...}, nil) - the transaction COMMITS (the
// revoke and its audit are a considered, permanent decision, ADR 0103
// §3.3). A genuine evaluation error (a DB error, or the revoke's own
// invariant check failing) returns a plain wrapped error - the
// transaction rolls back and the caller maps it to 5xx, never the uniform
// 401 or the constant 403 (BS-2).
func (o *Orchestrator) BootstrapLaunch(ctx context.Context, pool providercred.TenantTxRunner, tenantID uuid.UUID, providerID string, v *VerifiedCallback) (BootstrapResult, error) {
	if pool == nil {
		return BootstrapResult{}, fmt.Errorf("%w: casino launch bootstrap has no transaction runner configured", ErrProviderUnavailable)
	}

	var result BootstrapResult
	err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Step 2a (SB-1, BS-0): Redeem+Recheck is the FIRST statement.
		// Any failure is the uniform credential_unavailable
		// *webhookauth.AuthError; nothing else in this transaction has
		// run yet.
		_, in, _, err := o.redeemVerified(ctx, tx, tenantID, providerID, v)
		if err != nil {
			return err
		}

		// The body is parsed ONLY from the verified bytes Redeem
		// returned (SB-1) - never any earlier, unverified copy.
		// DisallowUnknownFields refuses a body carrying tenant_id (the
		// ADV "unknown field" case) exactly like every other malformed
		// body: the uniform refusal, nothing written.
		var body bootstrapRequestBody
		dec := json.NewDecoder(bytes.NewReader(in.Body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			return &BootstrapRefusedError{Reason: BootstrapRefusalNotFound}
		}
		mode := GameMode(body.Mode)
		if body.LaunchToken == "" || body.ProviderGameID == "" || body.AssetCode == "" ||
			!bootstrapRequestIDPattern.MatchString(body.RequestID) ||
			(mode != ModeReal && mode != ModeDemo) {
			return &BootstrapRefusedError{Reason: BootstrapRefusalNotFound}
		}

		tokenHash := hashLaunchToken(body.LaunchToken)
		digest := bootstrapRequestDigest(providerID, body.RequestID, body.ProviderGameID, body.AssetCode, body.Mode)

		// Step 2b: the session lookup FOR UPDATE, then the idempotency
		// lookup (order does not affect correctness here - see
		// bootstrap_integration_test.go's own concurrency notes - but
		// the session lookup runs first because it is the one that
		// actually serializes concurrent attempts against the SAME
		// token, per ADR 0103 §4).
		session, sessionFound, err := getLaunchSessionForBootstrap(ctx, tx, tenantID, tokenHash)
		if err != nil {
			return err
		}
		existing, existingFound, err := getBootstrapByRequestID(ctx, tx, tenantID, providerID, body.RequestID)
		if err != nil {
			return err
		}

		if existingFound {
			// ADR 0103 §3.4 (BS-4): a replay is admitted only when BOTH
			// the token hash and the request digest match, AND the
			// session is CURRENTLY 'consumed' (never resurrecting a
			// session A's own revoke has since moved to 'revoked').
			if existing.TokenHash != tokenHash || existing.RequestDigest != digest {
				return &BootstrapRefusedError{Reason: BootstrapRefusalReplayMismatch}
			}
			if !sessionFound || session.ID != existing.LaunchSessionID || session.Status != LaunchSessionConsumed {
				return &BootstrapRefusedError{Reason: BootstrapRefusalReplayRevoked}
			}
			// BS-4 requires a byte-identical replay. Postgres's JSONB
			// storage does NOT preserve the original key order or
			// whitespace of what was inserted (it re-serializes in its
			// own canonical form) - existing.Response is stored for
			// audit/debugging visibility, but is NOT what is replayed
			// back to the vendor. Re-marshaling the SAME Go struct type,
			// in the SAME field order, from the SAME underlying values
			// (the session row's own denormalized columns, never
			// user-suppliable) is what actually guarantees byte-identical
			// output - json.Marshal is deterministic for a fixed struct
			// type and fixed field values.
			respJSON, err := json.Marshal(bootstrapResponseBody{
				SessionID: session.ID.String(), PlayerRef: existing.PlayerRef.String(),
				ProviderGameID: session.ProviderGameID, AssetCode: session.AssetCode, Mode: string(session.Mode),
			})
			if err != nil {
				return fmt.Errorf("casino: marshal replayed bootstrap response: %w", err)
			}
			result = BootstrapResult{
				SessionID: session.ID, PlayerRef: existing.PlayerRef, ProviderGameID: session.ProviderGameID,
				AssetCode: session.AssetCode, Mode: session.Mode, ResponseJSON: respJSON, Replayed: true,
			}
			return nil
		}

		// Step 3: the binding check, uniform refusal on any mismatch -
		// nothing is written (no lazy 'expired' write, unlike
		// ResolveLaunchToken).
		if !sessionFound {
			return &BootstrapRefusedError{Reason: BootstrapRefusalNotFound}
		}
		if session.ProviderID != providerID || string(session.Mode) != body.Mode ||
			session.AssetCode != body.AssetCode || session.ProviderGameID != body.ProviderGameID {
			return &BootstrapRefusedError{Reason: BootstrapRefusalBindingMismatch}
		}
		if session.Status != LaunchSessionActive {
			return &BootstrapRefusedError{Reason: BootstrapRefusalNotActive}
		}
		if !time.Now().UTC().Before(session.ExpiresAt) {
			return &BootstrapRefusedError{Reason: BootstrapRefusalExpired}
		}

		// Step 4: re-check the gates. A definitive denial revokes and
		// audits, then COMMITS (ADR 0103 §3.3) - never a Go error.
		reason, err := bootstrapGateDenialReason(ctx, tx, tenantID, session)
		if err != nil {
			return err
		}
		if reason != "" {
			prior, revoked, err := RevokeLaunchSession(ctx, tx, session.ID)
			if err != nil {
				return err
			}
			if prior != LaunchSessionActive || !revoked {
				return fmt.Errorf("%w: revoke did not match the expected active session (prior=%s revoked=%v)", ErrBootstrapInvariantBroken, prior, revoked)
			}
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino.launch_bootstrap_denied",
				TargetType: "casino_launch_session", TargetID: session.ID.String(), Outcome: audit.OutcomeDenied,
				Metadata: map[string]any{
					"provider_id": providerID, "request_id": body.RequestID,
					"prior_status": string(prior), "reason": reason,
				},
			}); err != nil {
				return err
			}
			result = BootstrapResult{Denied: true, DeniedReason: reason, SessionID: session.ID}
			return nil
		}

		// Step 5 (SB-2): the binding-aware CAS. Zero rows -> the uniform
		// refusal (a concurrent resolver won, or the row moved under us
		// between step 3's read and here).
		consumedID, err := casConsumeSessionForBootstrap(ctx, tx, session.ID, tenantID, providerID, mode, body.AssetCode, body.ProviderGameID, tokenHash)
		if err != nil {
			return err
		}
		if consumedID == uuid.Nil {
			return &BootstrapRefusedError{Reason: BootstrapRefusalNotActive}
		}

		// Step 6: the player ref, then the idempotency record.
		playerRef, err := upsertProviderPlayerRef(ctx, tx, tenantID, providerID, session.PlayerAccountID)
		if err != nil {
			return err
		}
		respJSON, err := json.Marshal(bootstrapResponseBody{
			SessionID: session.ID.String(), PlayerRef: playerRef.String(),
			ProviderGameID: body.ProviderGameID, AssetCode: body.AssetCode, Mode: body.Mode,
		})
		if err != nil {
			return fmt.Errorf("casino: marshal bootstrap response: %w", err)
		}

		conflict, _, err := db.IdempotentInsert(ctx, tx, func(stx pgx.Tx) error {
			_, err := stx.Exec(ctx,
				`INSERT INTO casino_launch_bootstraps
					(tenant_id, provider_id, request_id, token_hash, request_digest, launch_session_id, player_ref, response)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				tenantID, providerID, body.RequestID, tokenHash, digest, session.ID, playerRef, respJSON)
			return err
		})
		if err != nil {
			return err
		}
		if conflict {
			// ADR 0103 §3.2 step 6: "A unique violation -> roll back,
			// retry once, and the retry takes the replay branch." The
			// SAVEPOINT rollback above (db.IdempotentInsert) IS the
			// "roll back"; this lookup IS the retry - both happen inside
			// THIS SAME transaction, so Redeem (single-use by design)
			// never runs a second time for one incoming request. This
			// path is reached only by a genuine concurrent race on the
			// SAME request_id against a DIFFERENT token (a different
			// session row, so no row lock serialized the two attempts
			// against each other before now) - the same-token case is
			// already fully serialized by the session row lock above and
			// never reaches here (the second caller blocks, then finds
			// the row via the EARLIER existingFound lookup instead).
			raced, racedFound, err := getBootstrapByRequestID(ctx, tx, tenantID, providerID, body.RequestID)
			if err != nil {
				return err
			}
			if !racedFound {
				return fmt.Errorf("casino: launch bootstrap insert conflicted but no row found on retry (request_id=%s)", body.RequestID)
			}
			if raced.TokenHash != tokenHash || raced.RequestDigest != digest {
				// A different token raced us to the same request_id -
				// our own CAS above must not stand: returning a non-nil
				// error rolls back this WHOLE transaction, undoing our
				// own consume, exactly as if we had lost the race before
				// ever reaching step 5.
				return &BootstrapRefusedError{Reason: BootstrapRefusalReplayMismatch}
			}
			// Unreachable in normal operation: a matching hash+digest
			// here would mean the SAME token raced past the session row
			// lock above, which cannot happen under FOR UPDATE. Fail
			// closed rather than guess.
			return fmt.Errorf("casino: launch bootstrap insert conflict for request_id=%s matched a request that should have been caught by the session row lock", body.RequestID)
		}

		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino.launch_bootstrapped",
			TargetType: "casino_launch_session", TargetID: session.ID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"provider_id": providerID, "request_id": body.RequestID,
				"mode": body.Mode, "asset_code": body.AssetCode,
			},
		}); err != nil {
			return err
		}

		result = BootstrapResult{
			SessionID: session.ID, PlayerRef: playerRef, ProviderGameID: body.ProviderGameID,
			AssetCode: body.AssetCode, Mode: mode, ResponseJSON: respJSON,
		}
		return nil
	})
	if err != nil {
		return BootstrapResult{}, err
	}
	return result, nil
}
