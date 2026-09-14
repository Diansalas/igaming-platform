package identityresolution

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// RegisterPlayerWithResolutionParams is RegisterPlayerWithResolution's
// input. TenantID/BrandID are carried on brand itself; Verified is
// whatever verified identity evidence is available (empty for every
// registration today - see VerifiedAttributes.IsEmpty's own doc comment).
type RegisterPlayerWithResolutionParams struct {
	Brand        identity.Brand
	Email        string
	PasswordHash string
	Verified     VerifiedAttributes
	// RequestID/IPAddress/UserAgent are optional HTTP request context,
	// carried through only so the identity_resolution.performed audit
	// record (below) can be correlated with the sibling player.registered
	// record the caller writes in the same transaction - CLAUDE.md
	// requires actor/entity/IP on every mutating action's audit record,
	// and every other audit write in this codebase already carries them.
	RequestID string
	IPAddress string
	UserAgent string
}

// RegisterPlayerOutcome reports which of the three registration paths
// RegisterPlayerWithResolution actually took - callers (the HTTP
// handler) use this only to decide audit metadata; it carries no
// additional authority of its own (the PlayerAccount's own Status is
// what internal/rg.EvaluateEligibility actually enforces).
type RegisterPlayerOutcome string

const (
	OutcomeNewPerson      RegisterPlayerOutcome = "new_person"
	OutcomeLinkedToPerson RegisterPlayerOutcome = "linked_to_existing_person"
	OutcomePendingReview  RegisterPlayerOutcome = "pending_identity_review"
)

// RegisterPlayerWithResolution is the ONE registration entry point every
// caller (the HTTP register handler) uses from Stage 4E onward - Person
// creation is never performed "blindly" ahead of identity resolution
// again (ADR 0027 §6, closing the Stage 4D-RG P0 finding). It consults
// resolver BEFORE deciding whether to create a new Person, link to an
// existing one, or land the registration in a safe review state, then
// performs the SAME atomic (transaction-scoped) work
// internal/identity.RegisterPlayer always did, writing an audit record
// for the resolution outcome in the same transaction.
//
// tx must already be tenant-scoped (db.WithTenant(ctx, brand.TenantID,
// ...)) - identical requirement to internal/identity.RegisterPlayer's own.
//
// resolver is called INSIDE tx, mirroring internal/casino.LaunchGame's
// own established precedent of calling an external provider from inside
// the enclosing transaction (ADR 0025) - for MockPersonResolver this is
// free (no real I/O), but a REAL future vendor integration should
// revisit whether holding a DB transaction open across a network call is
// still acceptable once real latency/failure modes are in play (recorded
// as an open consideration in ADR 0027, mirroring the identical
// open note Stage 4D-RG's own specialist review left for LaunchGame
// holding the person advisory lock across provider.Launch).
func RegisterPlayerWithResolution(ctx context.Context, tx pgx.Tx, resolver PersonResolver, params RegisterPlayerWithResolutionParams) (identity.PlayerAccount, RegisterPlayerOutcome, error) {
	if resolver == nil {
		return identity.PlayerAccount{}, "", fmt.Errorf("identityresolution: a PersonResolver is required")
	}

	result, resolveErr := resolver.Resolve(ctx, ResolutionInput{
		TenantID: params.Brand.TenantID,
		BrandID:  params.Brand.ID,
		Raw:      RawClaims{Email: params.Email, BrandSlug: params.Brand.Slug},
		Verified: params.Verified,
	})

	// Every branch below writes exactly one audit record, in this SAME
	// transaction - directive §17's "resolution requested, match, no
	// match, uncertain, resolution failure" all land here, keyed on
	// outcome. Never logs raw identity evidence - only the resolver's own
	// Reason string (which resolvers must keep non-sensitive - see
	// ResolutionResult's own doc comment) and, for Match, the matched
	// person id (an opaque identifier, not evidence). Carries
	// RequestID/IPAddress/UserAgent (when the caller supplied them) so this
	// record correlates with the sibling player.registered audit record
	// the HTTP handler writes in the same transaction (security specialist
	// review finding, Stage 4E) - previously these could only be joined by
	// timestamp.
	auditResolution := func(outcome audit.Outcome, outcomeLabel string, extra map[string]any) error {
		metadata := map[string]any{"resolution_outcome": outcomeLabel}
		for k, v := range extra {
			metadata[k] = v
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: params.Brand.TenantID, ActorType: audit.ActorSystem,
			Action: "identity_resolution.performed", TargetType: "brand", TargetID: params.Brand.ID.String(),
			Outcome: outcome, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
			Metadata: metadata,
		})
	}

	if resolveErr != nil {
		if !errors.Is(resolveErr, ErrResolverUnavailable) {
			// An error that is NOT the documented "unavailable" sentinel
			// is a genuine bug in the resolver implementation (it must
			// always wrap ErrResolverUnavailable for a failure) - fail
			// the whole request rather than guess which safe path applies.
			return identity.PlayerAccount{}, "", fmt.Errorf("identityresolution: resolver returned a non-conformant error: %w", resolveErr)
		}
		// OutcomeFailure, not OutcomeSuccess (security specialist review
		// finding, Stage 4E fix): the RESOLUTION itself genuinely failed to
		// complete here, even though the overall registration still
		// proceeds safely via the review-required path below - collapsing
		// this to "success" would hide every resolver outage from anyone
		// scanning audit_log for failures.
		if err := auditResolution(audit.OutcomeFailure, "resolver_unavailable", nil); err != nil {
			return identity.PlayerAccount{}, "", err
		}
		account, err := identity.RegisterPlayerPendingReview(ctx, tx, params.Brand, params.Email, params.PasswordHash)
		return account, OutcomePendingReview, err
	}

	switch result.Outcome {
	case Match:
		if result.MatchedPersonID == uuid.Nil {
			return identity.PlayerAccount{}, "", fmt.Errorf("identityresolution: resolver reported Match with no MatchedPersonID")
		}
		if err := auditResolution(audit.OutcomeSuccess, "match", map[string]any{"matched_person_id": result.MatchedPersonID.String(), "reason": result.Reason}); err != nil {
			return identity.PlayerAccount{}, "", err
		}
		account, err := identity.RegisterPlayerLinkedToPerson(ctx, tx, params.Brand, params.Email, params.PasswordHash, result.MatchedPersonID)
		return account, OutcomeLinkedToPerson, err

	case NoMatch:
		if err := auditResolution(audit.OutcomeSuccess, "no_match", map[string]any{"reason": result.Reason}); err != nil {
			return identity.PlayerAccount{}, "", err
		}
		account, err := identity.RegisterPlayer(ctx, tx, params.Brand, params.Email, params.PasswordHash)
		return account, OutcomeNewPerson, err

	case Uncertain:
		if err := auditResolution(audit.OutcomeSuccess, "uncertain", map[string]any{"reason": result.Reason}); err != nil {
			return identity.PlayerAccount{}, "", err
		}
		account, err := identity.RegisterPlayerPendingReview(ctx, tx, params.Brand, params.Email, params.PasswordHash)
		return account, OutcomePendingReview, err

	default:
		// Fail closed on an unrecognized outcome value too - a resolver
		// (including a future real one) that returns something outside
		// the four documented Outcome values is itself a bug, and the
		// safe response is identical to Uncertain, never NoMatch.
		if err := auditResolution(audit.OutcomeFailure, "unrecognized_outcome", map[string]any{"raw_outcome": string(result.Outcome)}); err != nil {
			return identity.PlayerAccount{}, "", err
		}
		account, err := identity.RegisterPlayerPendingReview(ctx, tx, params.Brand, params.Email, params.PasswordHash)
		return account, OutcomePendingReview, err
	}
}
