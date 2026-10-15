// Package marketgate is the ONE server-side operating-market gate (A8,
// owner directive "wire operating-market blocking into registration,
// deposit, launch"). It answers, for an in-flight player request, "is this
// platform permitted to serve this operation to this tenant/brand from the
// country this request is evidenced to come from?" by consulting the
// EXISTING operating-market model (internal/operatingmarket: licence
// ceiling -> tenant -> brand -> operation rungs, versioned configuration
// rows maintained by staff) - it adds no policy source, no table and no
// migration.
//
// # Package placement (ADR 0045 INV-M-1)
//
// internal/operatingmarket may never import internal/geolocation (or
// identity/kyc/rg), and internal/jurisdiction may never import it. This
// package is the composition point that is allowed to know both the
// policy mechanism and the evidence seam. It imports operatingmarket,
// geolocation (the Locator interface only), audit and validation.
//
// # Evidence rule (conservative reading of ADR 0045 / 0083 / the 4I
// canonical model)
//
// The country given to the policy resolver is NEVER a client-supplied
// value (the stage-4i exit register's call-site table: "CountryCode (from
// evidence, not client-declared)"; ADR 0083 5.3.3: "never from a
// client"). It is the country a geolocation.Locator RESOLVED for this
// request, validated against the ISO-3166-1 alpha-2 registry. Anything
// else - no Locator, a Go error, a non-resolved outcome, a malformed
// country, a panic, a timeout - is UNKNOWN evidence, and unknown evidence
// REFUSES (fail closed). Because no geolocation vendor exists, the shipped
// Locator (geolocation.StaticLocator) reports unknown by default: this
// gate therefore refuses everything unless a development/test deployment
// configures a static country. That is the recorded, deliberate state
// (docs/plans/a8-market-blocking-record.md): the open human decisions
// (HDR-J-7/J-8 on which evidence is authoritative, PC-GAP-1) are not
// answered by engineering.
//
// # Where it runs
//
// Locate performs the (future network) evidence lookup OUTSIDE any
// transaction. Check runs INSIDE the transaction in which the guarded
// state is read/written (registration: the account-creation transaction;
// deposit: InitiateDepositAttempt phase A; launch: LaunchGame phase A),
// reads the policy rows through that transaction (so a committed
// configuration change is visible to the very next request - there is no
// cache) and stamps AsOf from the database clock.
//
// # Refusal contract
//
// A refusal is a RESULT (Decision.Allowed == false), never a Go error, so
// the caller can commit the refusal's audit row (a returned error would
// roll it back - the same convention LaunchGameResult.Denied documents).
// There are exactly two stable internal reason codes
// (ReasonEvidenceUnavailable, ReasonNotPermitted); the player-facing
// surface collapses both, and every one of the operating-market outcomes,
// into ONE status, ONE code and ONE message (ADR 0045 section 6).
// The audit row carries ids and closed codes only: no country, no IP, no
// player evidence (geolocation/provider.go binding requirement 4).
package marketgate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/geolocation"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// Operation is the closed set of gated actions. The operating-market
// operation/product codes each maps to are COMPILE-TIME constants
// (operatingmarket.Query's rule: never a wire string).
type Operation string

const (
	OpRegistration Operation = "registration"
	OpDeposit      Operation = "deposit"
	// OpLaunch is a casino game launch (and the launch-session / token
	// mint it performs): operation "wagering", product "casino".
	OpLaunch Operation = "launch"
	// OpSportsbookWager is a sportsbook bet placement (SR8-1): operation
	// "wagering", product "sportsbook" (ADR 0083 5.3.3's contract).
	OpSportsbookWager Operation = "sportsbook_wagering"
	// OpCasinoBet is a NEW casino bet (a vendor callback, so it has no
	// player request evidence): operation "wagering", product "casino".
	// Used only by the brand-level AnyCountryPermits check, never by Check.
	OpCasinoBet Operation = "casino_wagering"
)

const (
	productCasino     = "casino"
	productSportsbook = "sportsbook"
)

// Stable internal refusal reason codes (audit and logs only; never sent to
// the player).
const (
	ReasonEvidenceUnavailable = "MKT_EVIDENCE_UNAVAILABLE"
	ReasonNotPermitted        = "MKT_NOT_PERMITTED"
)

// AuditActionRefused is the audit action of every refusal.
const AuditActionRefused = "market.access_refused"

// PlayerMessage is the ONE player-facing sentence for every refusal.
const PlayerMessage = "this service is not available in your region"

// ErrInvalidRequest is a caller bug (missing tenant/brand, unknown
// operation). Callers must treat any error from Check as a refusal.
var ErrInvalidRequest = errors.New("marketgate: invalid request")

// ErrGuardRequired is returned by an orchestrator entry point (LaunchGame,
// InitiateDepositAttempt, PlaceBet) called with an empty MarketGuard (SR8-3):
// the guard is mandatory and an empty one REFUSES. A library or test caller
// that deliberately runs without a market gate passes UngatedForTests.
var ErrGuardRequired = errors.New("marketgate: a market guard is required (an empty guard refuses; tests pass marketgate.UngatedForTests)")

// UngatedForTests is the explicit, named "no market gate" guard for tests and
// tooling. It allows everything and writes nothing. No production code may
// reference it: TestProductionCodeNeverUsesTheUngatedSentinels scans the module.
var UngatedForTests Guard = func(context.Context, pgx.Tx) (Decision, error) {
	return Decision{Allowed: true}, nil
}

// DefaultLocateTimeout bounds the evidence lookup so a slow vendor cannot
// hold a player request open. A timeout is unknown evidence (refuse).
const DefaultLocateTimeout = 2 * time.Second

// LocateRequest is what evidence collection may use. IPAddress is
// request-scoped and is never stored or logged by this package.
type LocateRequest struct {
	TenantID uuid.UUID
	// BrandID may be uuid.Nil when the caller has not resolved the brand yet
	// (deposit and launch gather evidence before their identity read).
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID // uuid.Nil at registration
	RequestID       string
	IPAddress       string
}

// Evidence is the server-derived country evidence for ONE request. Its
// fields are unexported and the only constructor is Gate.Locate, so a
// caller cannot assert a country; the zero value is UNKNOWN (refuses).
type Evidence struct {
	country string
	known   bool
}

// Known reports whether a validated country was resolved.
func (e Evidence) Known() bool { return e.known }

// Request is a guard's subject: server-derived identifiers only.
type Request struct {
	TenantID uuid.UUID
	BrandID  uuid.UUID
	// PlayerAccountID is uuid.Nil at registration.
	PlayerAccountID uuid.UUID
	Operation       Operation
	RequestID       string
}

// Decision is a guard's verdict. Code is empty when Allowed.
type Decision struct {
	Allowed bool
	Code    string
}

// Guard evaluates the gate inside the caller's tenant-scoped transaction.
// It is a function value so payments/casino can accept it without
// importing this package's Gate (and so tests can inject one).
type Guard func(ctx context.Context, tx pgx.Tx) (Decision, error)

// Checker is the dependency handlers hold: evidence outside the
// transaction, a Guard inside it.
type Checker interface {
	Locate(ctx context.Context, req LocateRequest) Evidence
	Guard(req Request, ev Evidence) Guard
}

// Gate is the production Checker.
type Gate struct {
	locator geolocation.Locator
	timeout time.Duration
	slots   chan struct{}
}

var _ Checker = (*Gate)(nil)

// New returns a Gate over locator. A nil locator is legal and means every
// request has unknown evidence (refuse).
func New(locator geolocation.Locator) *Gate {
	return &Gate{locator: locator, timeout: DefaultLocateTimeout, slots: make(chan struct{}, maxInflightLookups)}
}

// maxInflightLookups bounds the lookups a Gate lets run past their deadline
// (SR8-4). A Locator that ignores its context keeps its goroutine until it
// returns; the semaphore caps how many such goroutines can exist at once, and a
// request that cannot get a slot is treated as UNKNOWN immediately (refuse), so
// a hung vendor can cost at most this many parked goroutines, never unbounded
// memory, and never delays a player request past the deadline.
const maxInflightLookups = 128

// Locate resolves the request's country evidence. It never errors and
// never panics, and it NEVER waits longer than the Gate's timeout even if
// the Locator ignores its context: the lookup runs in its own goroutine and a
// late answer is discarded. Every failure is UNKNOWN.
func (g *Gate) Locate(ctx context.Context, req LocateRequest) Evidence {
	if g == nil || g.locator == nil {
		return Evidence{}
	}
	select {
	case g.slots <- struct{}{}:
	default:
		return Evidence{} // too many abandoned lookups in flight: unknown
	}
	lctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	done := make(chan Evidence, 1)
	go func() {
		defer func() { <-g.slots }()
		defer func() {
			if recover() != nil {
				done <- Evidence{}
			}
		}()
		res, err := g.locator.GetSignal(lctx, geolocation.SignalRequest{
			TenantID: req.TenantID, BrandID: req.BrandID, PlayerAccountID: req.PlayerAccountID,
			RequestID: req.RequestID, IPAddress: req.IPAddress,
		})
		if err != nil || lctx.Err() != nil ||
			res.Outcome != geolocation.SignalResolved || !validation.IsISO3166Alpha2(res.CountryCode) {
			done <- Evidence{}
			return
		}
		done <- Evidence{country: res.CountryCode, known: true}
	}()
	select {
	case ev := <-done:
		return ev
	case <-lctx.Done():
		return Evidence{}
	}
}

// Guard returns the in-transaction check for req over ev.
func (g *Gate) Guard(req Request, ev Evidence) Guard {
	return func(ctx context.Context, tx pgx.Tx) (Decision, error) {
		return Check(ctx, tx, req, ev)
	}
}

// opCodes maps a gated Operation to its operating-market vocabulary.
func opCodes(op Operation) (operation string, product *string, ok bool) {
	switch op {
	case OpRegistration:
		return "registration", nil, true
	case OpDeposit:
		return "deposit", nil, true
	case OpLaunch, OpCasinoBet:
		p := productCasino
		return "wagering", &p, true
	case OpSportsbookWager:
		p := productSportsbook
		return "wagering", &p, true
	}
	return "", nil, false
}

// Check is the single gate. tx MUST be a tenant-scoped, non-player-scoped
// transaction for req.TenantID (operatingmarket asserts this and returns
// ErrTransactionScope otherwise). A non-nil error means "could not decide"
// and the caller MUST refuse the action (fail closed); a refusal is
// returned as Decision{Allowed:false}, after its audit row has been
// written in tx, so the caller commits the refusal.
func Check(ctx context.Context, tx pgx.Tx, req Request, ev Evidence) (Decision, error) {
	opCode, product, ok := opCodes(req.Operation)
	if !ok || req.TenantID == uuid.Nil || req.BrandID == uuid.Nil {
		return Decision{}, fmt.Errorf("%w: operation/tenant/brand are required server-derived values", ErrInvalidRequest)
	}

	if !ev.known {
		return refuse(ctx, tx, req, ReasonEvidenceUnavailable, "unknown", "")
	}

	// AsOf from the database clock, the same clock that stamped every
	// policy row's effective_from: no application clock skew can make a
	// just-committed version "not yet effective". The resolver itself
	// never reads a clock (ADR 0045 INV-M-3).
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&asOf); err != nil {
		return Decision{}, fmt.Errorf("marketgate: read database clock: %w", err)
	}

	brand := req.BrandID
	if req.Operation == OpRegistration {
		// The ONE sanctioned anonymous-path projection (bool only).
		permitted, err := operatingmarket.IsRegistrationPermitted(ctx, tx, operatingmarket.RegistrationQuery{
			TenantID: req.TenantID, BrandID: &brand, CountryCode: ev.country, AsOf: asOf,
		})
		if err != nil {
			return Decision{}, fmt.Errorf("marketgate: registration market check: %w", err)
		}
		if permitted {
			return Decision{Allowed: true}, nil
		}
		return refuse(ctx, tx, req, ReasonNotPermitted, "resolved", "")
	}

	result, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{
		TenantID: req.TenantID, BrandID: &brand, CountryCode: ev.country,
		OperationCode: opCode, ProductCode: product, AsOf: asOf,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("marketgate: market check: %w", err)
	}
	if err := result.AssertScope(req.TenantID, &brand); err != nil {
		return Decision{}, fmt.Errorf("marketgate: market result scope: %w", err)
	}
	if result.Permitted() {
		return Decision{Allowed: true}, nil
	}
	return refuse(ctx, tx, req, ReasonNotPermitted, "resolved", string(result.Outcome()))
}

func refuse(ctx context.Context, tx pgx.Tx, req Request, reason, evidence, marketOutcome string) (Decision, error) {
	write, suppressed := refusalAudit.take(refusalKey{
		tenant: req.TenantID, brand: req.BrandID, player: req.PlayerAccountID, op: req.Operation, reason: reason,
	})
	if !write {
		// The refusal stands; only its audit row is sampled away (SR8-5).
		return Decision{Allowed: false, Code: reason}, nil
	}
	meta := map[string]any{
		"operation":   string(req.Operation),
		"reason_code": reason,
		"evidence":    evidence, // "resolved" | "unknown" - never the country
		"brand_id":    req.BrandID.String(),
	}
	if suppressed > 0 {
		meta["suppressed_since_last_row"] = suppressed
	}
	if marketOutcome != "" {
		meta["market_outcome"] = marketOutcome
		meta["policy_version"] = operatingmarket.PolicyVersion
	}
	entry := audit.Entry{
		TenantID: req.TenantID, ActorType: audit.ActorSystem,
		Action: AuditActionRefused, TargetType: "brand", TargetID: req.BrandID.String(),
		Outcome: audit.OutcomeDenied, RequestID: req.RequestID, Metadata: meta,
	}
	if req.PlayerAccountID != uuid.Nil {
		entry.ActorType, entry.ActorID = audit.ActorPlayer, req.PlayerAccountID
	}
	if err := audit.Record(ctx, tx, entry); err != nil {
		return Decision{}, fmt.Errorf("marketgate: audit refusal: %w", err)
	}
	return Decision{Allowed: false, Code: reason}, nil
}
