package casino

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// launchTokenBytes matches auth.refreshTokenBytes' own entropy choice -
// 256 bits of crypto/rand, base64url-encoded. Reusing the SAME generation
// mechanism as internal/auth's refresh tokens (never the same KEY
// MATERIAL, never the same TABLE) is deliberate: it is a proven, already-
// reviewed pattern for "opaque, unguessable, single-use credential,
// verified by comparing a stored hash" - ADR 0025 §3 chose this over a
// signed JWT specifically so a casino provider is never handed anything
// verifiable against the platform's own internal signing keys.
const launchTokenBytes = 32

// DefaultLaunchTokenTTL bounds how long an un-consumed launch token
// remains resolvable - short, per ADR 0025 §3 ("short TTL").
const DefaultLaunchTokenTTL = 2 * time.Minute

// defaultLaunchCallTimeout is CallContext.Deadline's default budget for
// phase B's provider.Launch call (ADR 0095 §15.1/§9.1). It is informational
// only for the MOCK adapter (which never does real I/O); a real adapter
// consults it to bound its own transport timeout.
const defaultLaunchCallTimeout = 10 * time.Second

// CallContext is casino's own copy of ADR 0095 §9.1's call-context shape,
// scoped to this domain (payments/kyc get their own until PRH-I1 lands the
// shared version - see the ADR 0095 §15.1 implementation note). Every
// outbound Launch request carries one, built fresh per call by LaunchGame's
// phase B - never cached, never reused across sessions.
type CallContext struct {
	// TenantID is taken from the launch session's own tenant (server-side),
	// never a payload.
	TenantID   uuid.UUID
	ProviderID string
	// Credential is resolved per call (PROV-OUTBOUND-CRED-1) outside any
	// database transaction; MOCK adapters get a synthetic credential
	// (providercred.NewMockOutboundCredential) instead of a real handle-
	// table read.
	Credential providercred.OutboundCredential
	// IdempotencyKey is "cas:" + the launch session's own id - the
	// deterministic external reference/idempotency key ADR 0095 §15.1
	// names for casino launch.
	IdempotencyKey string
	Deadline       time.Time
}

// callContextRedacted renders only the credential's own already-redacted
// form plus the non-sensitive fields - never a secret (ADR 0095 §9.1,
// mirroring OutboundCredential's identical renderer set).
func (c CallContext) callContextRedacted() string {
	return fmt.Sprintf("CallContext{TenantID:%s ProviderID:%s Credential:%s IdempotencyKey:%s Deadline:%s}",
		c.TenantID, c.ProviderID, c.Credential.String(), c.IdempotencyKey, c.Deadline)
}

func (c CallContext) String() string { return c.callContextRedacted() }

func (c CallContext) GoString() string { return c.callContextRedacted() }

func (c CallContext) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.callContextRedacted())) }

// LogValue implements slog.LogValuer (never the secret).
func (c CallContext) LogValue() slog.Value { return slog.StringValue(c.callContextRedacted()) }

// MarshalJSON implements json.Marshaler (never the secret).
func (c CallContext) MarshalJSON() ([]byte, error) { return json.Marshal(c.callContextRedacted()) }

// OutboundCredentialResolver is what LaunchGame's phase B needs to resolve
// a per-call outbound credential (PROV-OUTBOUND-CRED-1) before calling
// CasinoProvider.Launch. providercred's own *(*Subsystem).Outbound("casino")
// return value (*providercred.OutboundResolver) satisfies this exactly, by
// having an identical method signature; MockOutboundResolver (mock.go) is
// the "MOCK: synthetic credential" case ADR 0095 §9.1 names, wired only
// behind a synthetic/MOCK provider adapter.
type OutboundCredentialResolver interface {
	Resolve(ctx context.Context, pool providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error)
}

// syntheticCasinoAdapter is providerkind.Synthetic restated structurally,
// mirroring webhookauth.syntheticAdapter's identical role for INBOUND
// credentials.
type syntheticCasinoAdapter interface{ SyntheticComponent() }

// OutboundKindSplitResolver is security review RV-PRH-I2 C3's fix: the
// outbound-credential resolver is chosen by the ADAPTER's own kind
// (synthetic/MOCK vs real), never by "is any mock wired anywhere in this
// process" - mirrors webhookauth.KindSplitResolver's identical role for
// INBOUND credentials exactly, including its fail-closed-on-unregistered-
// id behavior (a provider id absent from adapters gets neither resolver).
type OutboundKindSplitResolver struct {
	synthetic map[string]bool
	mock      OutboundCredentialResolver
	real      OutboundCredentialResolver
}

// NewOutboundKindSplitResolver builds the split over adapters (the same
// registry NewOrchestrator was built with). mock serves every provider id
// whose adapter is synthetic (nil means none is wired: those launches fail
// closed with "no outbound credential resolver configured", the same
// convention LaunchGame's own nil check already uses); real serves every
// other registered adapter (nil likewise). Returns a TRUE nil interface
// when both are nil, so LaunchGame's own nil-resolver branch still
// applies.
func NewOutboundKindSplitResolver(adapters map[string]CasinoProvider, mock, real OutboundCredentialResolver) OutboundCredentialResolver {
	if mock == nil && real == nil {
		return nil
	}
	s := &OutboundKindSplitResolver{synthetic: map[string]bool{}, mock: mock, real: real}
	for id, a := range adapters {
		_, isSynthetic := any(a).(syntheticCasinoAdapter)
		s.synthetic[id] = isSynthetic
	}
	return s
}

// Resolve implements OutboundCredentialResolver. A provider id never
// registered in the adapters map this resolver was built from fails
// closed (ErrOutboundCredentialUnavailable), exactly like
// webhookauth.KindSplitResolver.target's identical unregistered-id case -
// this can only be reached if LaunchGame's own registry check (phase A)
// and this resolver were built from different adapter maps, which never
// happens in this codebase's wiring, but the fail-closed default is kept
// rather than assumed.
func (s *OutboundKindSplitResolver) Resolve(ctx context.Context, pool providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	isSynthetic, registered := s.synthetic[providerID]
	if !registered {
		return providercred.OutboundCredential{}, providercred.ErrOutboundCredentialUnavailable
	}
	target := s.real
	if isSynthetic {
		target = s.mock
	}
	if target == nil {
		return providercred.OutboundCredential{}, providercred.ErrOutboundCredentialUnavailable
	}
	return target.Resolve(ctx, pool, tenantID, providerID)
}

func generateLaunchToken() (string, error) {
	b := make([]byte, launchTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("casino: generate launch token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashLaunchToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// LaunchSession mirrors a casino_launch_sessions row.
type LaunchSession struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID
	GameID          uuid.UUID
	ProviderID      string
	ProviderGameID  string
	AssetCode       string
	Mode            GameMode
	// JurisdictionCode is the jurisdiction LaunchGame resolved at mint
	// time (Stage 4G-FINAL Part C - migration 0042), denormalized here
	// exactly like ProviderGameID/AssetCode so the round's own
	// jurisdiction context stays fixed for its whole lifetime even if the
	// player's resolved jurisdiction could theoretically change mid-round.
	// Empty when LaunchGame itself had no resolved jurisdiction to persist
	// (TODO(jurisdiction) - see LaunchGameParams' own doc comment) - never
	// silently defaulted to a real code. Populated for a DEMO-mode session
	// too (LaunchGame computes it unconditionally, before branching on
	// Mode) - harmless: postBet already rejects any non-real-mode session
	// before ever reading this field, so a demo session's persisted value
	// is inert data, never consulted by risk (casino integration review).
	JurisdictionCode string
	Status           LaunchSessionStatus
	ExpiresAt        time.Time
}

// CreateLaunchSessionParams is CreateLaunchSession's input. All identity
// fields (TenantID, BrandID, PlayerAccountID, WalletID) MUST be resolved
// server-side from the authenticated player session - never from a
// request body - mirroring payment-orchestration.md §3's identical rule
// for DepositScope. GameID/ProviderID/ProviderGameID/AssetCode come from
// the already-resolved, eligibility-checked Game (ResolveLaunchEligibility),
// never re-derived from client input at this point.
type CreateLaunchSessionParams struct {
	TenantID         uuid.UUID
	BrandID          uuid.UUID
	PlayerAccountID  uuid.UUID
	WalletID         uuid.UUID
	GameID           uuid.UUID
	ProviderID       string
	ProviderGameID   string
	AssetCode        string
	Mode             GameMode
	JurisdictionCode string
	TTL              time.Duration
}

// CreateLaunchSession mints a brand-new, single-use, opaque launch
// token and its backing casino_launch_sessions row (ADR 0025 §3). Returns
// the RAW token exactly once - like auth.IssueSession, only the SHA-256
// hash is ever persisted, so the raw value cannot be recovered from the
// database even by a caller with direct SQL access.
func CreateLaunchSession(ctx context.Context, tx pgx.Tx, params CreateLaunchSessionParams) (LaunchSession, string, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.WalletID == uuid.Nil || params.GameID == uuid.Nil {
		return LaunchSession{}, "", fmt.Errorf("%w: launch session requires fully-populated, server-derived identity fields", ErrInvalidInput)
	}
	if params.ProviderID == "" || params.ProviderGameID == "" || params.AssetCode == "" {
		return LaunchSession{}, "", fmt.Errorf("%w: provider_id, provider_game_id, and asset_code are required", ErrInvalidInput)
	}
	if params.Mode != ModeReal && params.Mode != ModeDemo {
		return LaunchSession{}, "", fmt.Errorf("%w: mode must be 'real' or 'demo'", ErrInvalidInput)
	}
	ttl := params.TTL
	if ttl <= 0 {
		ttl = DefaultLaunchTokenTTL
	}

	token, err := generateLaunchToken()
	if err != nil {
		return LaunchSession{}, "", err
	}
	id := uuid.New()
	expiresAt := time.Now().UTC().Add(ttl)
	_, err = tx.Exec(ctx,
		`INSERT INTO casino_launch_sessions
			(id, tenant_id, brand_id, player_account_id, wallet_id, game_id, provider_id, provider_game_id,
			 asset_code, mode, jurisdiction_code, token_hash, status, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), $12, 'active', $13)`,
		id, params.TenantID, params.BrandID, params.PlayerAccountID, params.WalletID, params.GameID,
		params.ProviderID, params.ProviderGameID, params.AssetCode, params.Mode, params.JurisdictionCode,
		hashLaunchToken(token), expiresAt,
	)
	if err != nil {
		return LaunchSession{}, "", fmt.Errorf("casino: create launch session: %w", err)
	}

	return LaunchSession{
		ID: id, TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID,
		WalletID: params.WalletID, GameID: params.GameID, ProviderID: params.ProviderID, ProviderGameID: params.ProviderGameID,
		AssetCode: params.AssetCode, Mode: params.Mode, JurisdictionCode: params.JurisdictionCode,
		Status: LaunchSessionActive, ExpiresAt: expiresAt,
	}, token, nil
}

// ResolveLaunchToken looks up the casino_launch_sessions row for a raw
// token by its hash and, if it is genuinely active and not expired,
// atomically marks it 'consumed' - single-use enforcement (ADR 0025 §3).
// Two concurrent resolution attempts for the SAME token can never both
// succeed: the UPDATE ... WHERE status = 'active' below is the atomic
// compare-and-swap, not a separate read-then-write (the same race the
// withdrawal/payments packages close via a row lock, applied here via a
// conditional UPDATE instead, since there is no multi-statement
// transition to protect beyond this single flip).
//
// tx must already be tenant-scoped - the caller resolves which tenant a
// callback/provider request belongs to BEFORE calling this (the token
// itself carries no tenant claim; RLS is what actually prevents a token
// hash collision, astronomically unlikely at 256 bits of entropy, from
// ever resolving across a tenant boundary).
func ResolveLaunchToken(ctx context.Context, tx pgx.Tx, rawToken string) (LaunchSession, error) {
	if rawToken == "" {
		return LaunchSession{}, fmt.Errorf("%w: launch token is required", ErrInvalidInput)
	}
	hash := hashLaunchToken(rawToken)

	var s LaunchSession
	var status LaunchSessionStatus
	var jurisdictionCode *string
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, player_account_id, wallet_id, game_id, provider_id, provider_game_id,
			asset_code, mode, jurisdiction_code, status, expires_at
		 FROM casino_launch_sessions WHERE token_hash = $1`,
		hash,
	).Scan(&s.ID, &s.TenantID, &s.BrandID, &s.PlayerAccountID, &s.WalletID, &s.GameID, &s.ProviderID, &s.ProviderGameID,
		&s.AssetCode, &s.Mode, &jurisdictionCode, &status, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LaunchSession{}, ErrLaunchSessionNotFound
	}
	if err != nil {
		return LaunchSession{}, fmt.Errorf("casino: resolve launch token: %w", err)
	}
	if jurisdictionCode != nil {
		s.JurisdictionCode = *jurisdictionCode
	}

	if status != LaunchSessionActive {
		return LaunchSession{}, ErrLaunchSessionNotActive
	}
	if time.Now().UTC().After(s.ExpiresAt) {
		// Lazily transition to 'expired' on first post-expiry access,
		// rather than requiring a background sweep - the row is already
		// unusable via the status check above regardless of whether this
		// write succeeds, so its own error is not fatal to the caller's
		// actual question ("is this token usable" - no).
		_, _ = tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1 AND status = 'active'`, s.ID)
		return LaunchSession{}, ErrLaunchSessionNotActive
	}

	tag, err := tx.Exec(ctx,
		`UPDATE casino_launch_sessions SET status = 'consumed', consumed_at = now() WHERE id = $1 AND status = 'active'`,
		s.ID,
	)
	if err != nil {
		return LaunchSession{}, fmt.Errorf("casino: consume launch session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Lost the race to a concurrent resolution of the same token
		// between the SELECT above and this UPDATE.
		return LaunchSession{}, ErrLaunchSessionNotActive
	}

	s.Status = LaunchSessionConsumed
	return s, nil
}

// GetLaunchSessionByID reads a casino_launch_sessions row by its platform
// id, WITHOUT consuming it - the single-use consume-on-resolve semantics
// in ResolveLaunchToken apply only to the raw launch TOKEN (bootstrapping
// the game client), never to the session ROW itself, which remains the
// round's own identity anchor for as long as the round runs (ADR 0025 §3/
// §6 - specialist review finding: bet/win/rollback callbacks throughout a
// round must resolve player/wallet/asset/mode from this platform-owned
// row, never trust a payload-supplied player_account_id). tx must already
// be tenant-scoped; RLS is what actually prevents this from ever resolving
// a different tenant's session.
func GetLaunchSessionByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (LaunchSession, error) {
	var s LaunchSession
	var status LaunchSessionStatus
	var jurisdictionCode *string
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, brand_id, player_account_id, wallet_id, game_id, provider_id, provider_game_id,
			asset_code, mode, jurisdiction_code, status, expires_at
		 FROM casino_launch_sessions WHERE id = $1`,
		id,
	).Scan(&s.ID, &s.TenantID, &s.BrandID, &s.PlayerAccountID, &s.WalletID, &s.GameID, &s.ProviderID, &s.ProviderGameID,
		&s.AssetCode, &s.Mode, &jurisdictionCode, &status, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LaunchSession{}, ErrLaunchSessionNotFound
	}
	if err != nil {
		return LaunchSession{}, fmt.Errorf("casino: get launch session by id: %w", err)
	}
	if jurisdictionCode != nil {
		s.JurisdictionCode = *jurisdictionCode
	}
	s.Status = status
	return s, nil
}

// RevokeLaunchSession marks an active OR consumed session unusable without
// leaving it playable - never a DELETE, since the row is the audit-visible
// record of a launch attempt having occurred at all.
//
// CAS-REVOKE-CONSUMED-1 (docs/plans/payment-readiness/rv-prh-i2-casino-
// security.md "Re-review (FH-7, 2026-09-28)", required fix; migration
// 0108): before 0108, this CAS only matched status='active', so a launch
// phase C recorded as failed AFTER the vendor had already consumed the
// token left the session 'consumed' and indefinitely bet-eligible - the
// C4 gap ADR 0095 §15.1.5 reopened and the now-inverted
// TestLaunchGame_FailedLaunchOnConsumedSession_RevokesAndRejectsBet
// (formerly the characterization test) pins closed. Migration 0108's
// trigger now permits exactly the consumed -> revoked transition (with
// every other column frozen), so this CAS is widened to match.
//
// SELECT ... FOR UPDATE first (rather than folding the whole thing into a
// single UPDATE ... WHERE) so the prior status is always known and
// returned to the caller, even when the CAS itself cannot proceed (e.g. a
// concurrent resolver flips 'active' to 'consumed' between this SELECT and
// the UPDATE below - the UPDATE's own WHERE status IN (...) is what
// actually makes the transition atomic; the row lock only serializes
// concurrent revokes/reads against the same session so two callers never
// observe two different "prior" statuses for the same physical
// transition).
//
// Returns the prior status and whether the CAS actually matched a row
// (revoked=false means the session was already 'expired' or 'revoked' -
// e.g. a hypothetical race, or a second revoke attempt - so the caller can
// record the true outcome rather than assuming success; security review
// RV-PRH-I2 C1/F3).
func RevokeLaunchSession(ctx context.Context, tx pgx.Tx, id uuid.UUID) (priorStatus LaunchSessionStatus, revoked bool, err error) {
	err = tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1 FOR UPDATE`, id).Scan(&priorStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrLaunchSessionNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("casino: revoke launch session: select for update: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE casino_launch_sessions SET status = 'revoked' WHERE id = $1 AND status IN ('active', 'consumed')`,
		id,
	)
	if err != nil {
		return priorStatus, false, fmt.Errorf("casino: revoke launch session: %w", err)
	}
	return priorStatus, tag.RowsAffected() == 1, nil
}
