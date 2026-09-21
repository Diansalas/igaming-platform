package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// --- Platform-wide game catalogue (platform_admin only) ---

type upsertCasinoGameRequest struct {
	ProviderID            string   `json:"provider_id"`
	ProviderGameID        string   `json:"provider_game_id"`
	Name                  string   `json:"name"`
	GameType              string   `json:"game_type"`
	RTPVariant            string   `json:"rtp_variant,omitempty"`
	Volatility            string   `json:"volatility,omitempty"`
	FeatureFlags          []string `json:"feature_flags,omitempty"`
	SupportedAssets       []string `json:"supported_assets,omitempty"`
	MobileSupported       bool     `json:"mobile_supported"`
	DemoSupported         bool     `json:"demo_supported"`
	JurisdictionBlocklist []string `json:"jurisdiction_blocklist,omitempty"`
	// Status must be "active" or "disabled" (casino.GameStatus). Empty
	// defaults to "active" (casino.UpsertGame's own default).
	Status string `json:"status,omitempty"`
}

// newUpsertCasinoGameHandler registers or updates a title in the
// PLATFORM-WIDE game catalogue (ADR 0025 §2) - never tenant-scoped, run
// under db.Pool.WithPlatformAdmin: migration 0084 (ADR 0081,
// ARCH-DB-2) gave casino_games ENABLE+FORCE row-level security with a
// write policy scoped to the app.platform_admin_principal_id GUC, so a
// WithoutTenant transaction can no longer write this table at all. Gated
// by PermCasinoCatalogueManage, held only by
// RolePlatformAdmin: a tenant administering its OWN routing/availability
// must never be able to register a brand-new title into the shared
// catalogue, only opt into one the platform has already vetted.
//
// SEC-4I-F3 fix (docs/governance/stage-4i-canonical-model.md §9.5,
// confirmed a HARD PREREQUISITE of casino's own K-3 remediation): the
// audit record now captures before/after state for every enforcement-
// relevant field - status, supported_assets, demo_supported, and
// specifically jurisdiction_blocklist, which K-3 turns into a live,
// platform-wide, single-actor, non-four-eyes denial control the moment
// this handler adds a code to it (every launch of that game then denies
// for every tenant, canonical-model §9.5). A control with that blast
// radius that leaves no diff in its own audit trail is not operable -
// before this fix the audit entry recorded only provider_id/
// provider_game_id/status, with no way to reconstruct what changed or
// who last touched jurisdiction_blocklist. Follows the before/after
// pattern already established at
// internal/httpserver/withdrawal_policy_handlers.go's
// newDeleteWithdrawalPolicyHandler (a DELETE...RETURNING before-image);
// here the read happens via GetGameByProviderRef immediately before the
// upsert, in the SAME transaction, so the before-image can never observe
// a different row than the one the upsert is about to replace. A brand-
// new game (no prior row) records before=nil, distinguishable from an
// update in the metadata shape itself.
func newUpsertCasinoGameHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var req upsertCasinoGameRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("provider_id", req.ProviderID)
		v.RequireNonEmpty("provider_game_id", req.ProviderGameID)
		v.RequireNonEmpty("name", req.Name)
		v.RequireNonEmpty("game_type", req.GameType)
		status := casino.GameStatus(req.Status)
		if status == "" {
			status = casino.GameStatusActive
		} else if status != casino.GameStatusActive && status != casino.GameStatusDisabled {
			v.Add("status", "must be 'active' or 'disabled'")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		// Migration 0084 (ADR 0081): a swallowed parse failure here
		// would silently become uuid.Nil and get rejected by
		// WithPlatformAdmin's own nil-principal guard as an opaque
		// "db: WithPlatformAdmin called with nil principal id" error -
		// so the parse error is surfaced explicitly instead, mirroring
		// admin_routes.go's identical newCreateTenantHandler pattern.
		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var game casino.Game
		err = deps.DB.WithPlatformAdmin(r.Context(), subjectID, func(ctx context.Context, tx pgx.Tx) error {
			// Before-image, read in the SAME transaction immediately ahead
			// of the write it precedes (SEC-4I-F3) - nil (not an empty
			// struct) for a brand-new title, so the metadata shape itself
			// distinguishes "created" from "updated".
			var before *casino.Game
			existing, err := casino.GetGameByProviderRef(ctx, tx, req.ProviderID, req.ProviderGameID)
			if err == nil {
				before = &existing
			} else if !errors.Is(err, casino.ErrGameNotFound) {
				return err
			}

			game, err = casino.UpsertGame(ctx, tx, casino.UpsertGameInput{
				ProviderID: req.ProviderID, ProviderGameID: req.ProviderGameID, Name: req.Name, GameType: req.GameType,
				RTPVariant: req.RTPVariant, Volatility: req.Volatility, FeatureFlags: req.FeatureFlags,
				SupportedAssets: req.SupportedAssets, MobileSupported: req.MobileSupported, DemoSupported: req.DemoSupported,
				JurisdictionBlocklist: req.JurisdictionBlocklist, Status: status,
			})
			if err != nil {
				return err
			}
			// Migration 0084 (ADR 0081 §7.1, recommended widening): name/
			// game_type are added alongside the pre-existing enforcement-
			// relevant fields - §4.2 deliberately leaves game_type mutable
			// even though it feeds bonus wagering-contribution weighting
			// (bonus.RecordCashFundedWageringContribution), so its change
			// must be visible in the audit trail too.
			metadata := map[string]any{
				"provider_id": req.ProviderID, "provider_game_id": req.ProviderGameID,
				"after": map[string]any{
					"name": game.Name, "game_type": game.GameType,
					"status": string(game.Status), "supported_assets": game.SupportedAssets,
					"demo_supported": game.DemoSupported, "jurisdiction_blocklist": game.JurisdictionBlocklist,
				},
			}
			if before != nil {
				metadata["before"] = map[string]any{
					"name": before.Name, "game_type": before.GameType,
					"status": string(before.Status), "supported_assets": before.SupportedAssets,
					"demo_supported": before.DemoSupported, "jurisdiction_blocklist": before.JurisdictionBlocklist,
				}
			} else {
				metadata["before"] = nil
			}
			return audit.Record(ctx, tx, audit.Entry{
				ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "casino_game.upserted", TargetType: "casino_game", TargetID: game.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: metadata,
			})
		})
		if err != nil {
			logger.Error("upsert_casino_game_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to upsert game")
			return
		}
		writeJSON(w, http.StatusOK, toCasinoGameResponse(game))
	}
}

// --- Tenant-scoped casino provider capability configuration ---

type writeCasinoCapabilityRequest struct {
	SupportsCatalogue  bool     `json:"supports_catalogue"`
	SupportsLaunch     bool     `json:"supports_launch"`
	SupportsBalance    bool     `json:"supports_balance"`
	SupportsBet        bool     `json:"supports_bet"`
	SupportsWin        bool     `json:"supports_win"`
	SupportsRollback   bool     `json:"supports_rollback"`
	SupportedAssets    []string `json:"supported_assets"`
	SupportedGameTypes []string `json:"supported_game_types"`
	Priority           int      `json:"priority"`
	// Status must be "active" or "disabled" (casino.CapabilityStatus).
	Status string `json:"status"`
}

// newWriteCasinoCapabilityHandler configures a tenant's routing capability
// for one registered casino provider_id (ADR 0025 §4/§11) - a tenant-level
// administrative action, always audited. Mirrors
// newWriteProviderCapabilityHandler's identical structure/rationale in
// provider_capability_handlers.go; casino.WriteCapability itself enforces
// the narrowing-only rule (a tenant configuration may never widen beyond
// what the adapter's own Capabilities() declares).
func newWriteCasinoCapabilityHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.CasinoOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "casino is not enabled on this deployment")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		providerID := r.PathValue("providerID")
		if providerID == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "provider id is required")
			return
		}
		provider, ok := deps.CasinoOrchestrator.Provider(providerID)
		if !ok {
			apierror.Write(w, requestID, apierror.CodeNotFound, "no adapter registered for this provider id")
			return
		}

		var req writeCasinoCapabilityRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		status := casino.CapabilityStatus(req.Status)
		if status != casino.CapabilityActive && status != casino.CapabilityDisabled {
			apierror.Write(w, requestID, apierror.CodeValidation, "status must be 'active' or 'disabled'")
			return
		}

		subjectID, _ := uuid.Parse(tc.Subject)
		var capabilityID uuid.UUID
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			capabilityID, err = casino.WriteCapability(ctx, tx, provider, tc.TenantID, nil, casino.CapabilityConfig{
				SupportsCatalogue: req.SupportsCatalogue, SupportsLaunch: req.SupportsLaunch, SupportsBalance: req.SupportsBalance,
				SupportsBet: req.SupportsBet, SupportsWin: req.SupportsWin, SupportsRollback: req.SupportsRollback,
				SupportedAssets: req.SupportedAssets, SupportedGameTypes: req.SupportedGameTypes,
				Priority: req.Priority, Status: status,
			})
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "casino_provider_capability.configured", TargetType: "casino_provider_capability", TargetID: capabilityID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"provider_id": providerID, "status": req.Status, "priority": req.Priority},
			})
		})
		if errors.Is(err, casino.ErrCapabilityWidensAdapter) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			logger.Error("write_casino_capability_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to configure provider capability")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": capabilityID.String()})
	}
}

// --- Tenant-scoped game availability (opt-in) configuration ---

type setCasinoGameAvailabilityRequest struct {
	Enabled bool `json:"enabled"`
}

// newSetCasinoGameAvailabilityHandler opts a tenant (tenant-wide - brand-
// specific availability is not exposed via HTTP this stage, though
// casino.SetGameAvailability itself supports it) into or out of one
// platform-catalogue title (ADR 0025 §2's opt-in layer). Never lets a
// tenant register a new game or widen a platform-level jurisdiction
// block/status - only toggle enablement for a title the platform catalogue
// already contains.
func newSetCasinoGameAvailabilityHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		gameID, err := uuid.Parse(r.PathValue("gameID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid game id")
			return
		}

		var req setCasinoGameAvailabilityRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}

		subjectID, _ := uuid.Parse(tc.Subject)
		var availability casino.GameAvailability
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := casino.GetGameByID(ctx, tx, gameID); err != nil {
				return err
			}
			var err error
			availability, err = casino.SetGameAvailability(ctx, tx, tc.TenantID, nil, gameID, req.Enabled)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "casino_game_availability.configured", TargetType: "casino_game", TargetID: gameID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"enabled": req.Enabled},
			})
		})
		if errors.Is(err, casino.ErrGameNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "game not found")
			return
		}
		if db.IsForeignKeyViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "unknown game id")
			return
		}
		if err != nil {
			logger.Error("set_casino_game_availability_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to configure game availability")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"game_id": availability.GameID.String(), "enabled": availability.Enabled,
		})
	}
}
