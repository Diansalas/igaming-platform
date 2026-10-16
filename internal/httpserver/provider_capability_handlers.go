package httpserver

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/google/uuid"
)

type amountLimitRequest struct {
	AssetCode string `json:"asset_code"`
	MinAmount int64  `json:"min_amount"`
	MaxAmount int64  `json:"max_amount"`
}

type writeCapabilityRequest struct {
	SupportedFiatCurrencies []string             `json:"supported_fiat_currencies"`
	SupportedCryptoAssets   []string             `json:"supported_crypto_assets"`
	SupportedPaymentMethods []string             `json:"supported_payment_methods"`
	SupportedCountries      []string             `json:"supported_countries"`
	SupportsDeposit         bool                 `json:"supports_deposit"`
	SupportsWithdrawal      bool                 `json:"supports_withdrawal"`
	SupportsRefundReversal  bool                 `json:"supports_refund_reversal"`
	AmountLimits            []amountLimitRequest `json:"amount_limits"`
	Priority                int                  `json:"priority"`
	// Status must be "active" or "disabled" (payments.CapabilityStatus).
	Status string `json:"status"`
}

// newWriteProviderCapabilityHandler configures a tenant's routing
// capability for one registered provider_id (docs/decisions/0022 §2/§2.1)
// - a platform-level administrative action, always audited. It can only
// NARROW what the adapter itself declares via Capabilities() - never
// widen it or assert a capability the adapter doesn't have -
// payments.WriteCapability enforces that; this handler does not duplicate
// the check.
//
// Stage 3B ships only a mock provider, so this endpoint's only realistic
// use today is configuring routing for "mock", validated end to end by
// this same mechanism a future real adapter would use unchanged
// (docs/decisions/0022 §5's provider-independence checklist).
func newWriteProviderCapabilityHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.PaymentOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "payments are not enabled on this deployment")
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
		provider, ok := deps.PaymentOrchestrator.Provider(providerID)
		if !ok {
			apierror.Write(w, requestID, apierror.CodeNotFound, "no adapter registered for this provider id")
			return
		}

		var req writeCapabilityRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		status := payments.CapabilityStatus(req.Status)
		if status != payments.CapabilityActive && status != payments.CapabilityDisabled {
			apierror.Write(w, requestID, apierror.CodeValidation, "status must be 'active' or 'disabled'")
			return
		}
		limits := make([]payments.AmountLimit, 0, len(req.AmountLimits))
		for _, l := range req.AmountLimits {
			limits = append(limits, payments.AmountLimit{AssetCode: l.AssetCode, MinAmount: l.MinAmount, MaxAmount: l.MaxAmount})
		}

		subjectID, _ := uuid.Parse(tc.Subject)
		var capabilityID uuid.UUID
		var effective payments.ProviderCapability
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			capabilityID, err = payments.WriteCapability(ctx, tx, provider, tc.TenantID, nil, payments.CapabilityConfig{
				SupportedFiatCurrencies: req.SupportedFiatCurrencies,
				SupportedCryptoAssets:   req.SupportedCryptoAssets,
				SupportedPaymentMethods: req.SupportedPaymentMethods,
				SupportedCountries:      req.SupportedCountries,
				SupportsDeposit:         req.SupportsDeposit,
				SupportsWithdrawal:      req.SupportsWithdrawal,
				SupportsRefundReversal:  req.SupportsRefundReversal,
				AmountLimits:            limits,
				Priority:                req.Priority,
				Status:                  status,
			})
			if err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "provider_capability.configured", TargetType: "provider_capability", TargetID: capabilityID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"provider_id": providerID, "status": req.Status, "priority": req.Priority},
			}); err != nil {
				return err
			}
			// SA-5: read the stored row back inside the same transaction, so the response states what was
			// actually stored and not what was asked for.
			var found bool
			effective, found, err = payments.LoadCapability(ctx, tx, tc.TenantID, uuid.Nil, providerID)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("provider capability not found immediately after write")
			}
			return nil
		})
		if errors.Is(err, payments.ErrCapabilityWidensAdapter) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			logger.Error("write_provider_capability_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to configure provider capability")
			return
		}
		resp := toWriteCapabilityResponse(capabilityID, req, effective)
		if resp.Narrowed {
			logger.Warn("provider_capability_effective_differs_from_request", "provider_id", providerID, "request_id", requestID)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// capabilityEffectiveResponse is the configuration that was actually stored, in the shape of the request.
type capabilityEffectiveResponse struct {
	SupportedFiatCurrencies []string             `json:"supported_fiat_currencies"`
	SupportedCryptoAssets   []string             `json:"supported_crypto_assets"`
	SupportedPaymentMethods []string             `json:"supported_payment_methods"`
	SupportedCountries      []string             `json:"supported_countries"`
	SupportsDeposit         bool                 `json:"supports_deposit"`
	SupportsWithdrawal      bool                 `json:"supports_withdrawal"`
	SupportsRefundReversal  bool                 `json:"supports_refund_reversal"`
	AmountLimits            []amountLimitRequest `json:"amount_limits"`
	Priority                int                  `json:"priority"`
	Status                  string               `json:"status"`
}

// writeCapabilityResponse (SA-5): the row id, the EFFECTIVE stored configuration read back after the write, and
// narrowed=true when that differs from what the request asked for. A request wider than the adapter's declared
// manifest is refused 400 and never reaches this response, so today narrowed is false on every 200; the field makes any
// future divergence between request and stored row explicit instead of silent.
type writeCapabilityResponse struct {
	ID        string                      `json:"id"`
	Effective capabilityEffectiveResponse `json:"effective"`
	Narrowed  bool                        `json:"narrowed"`
}

func toWriteCapabilityResponse(id uuid.UUID, req writeCapabilityRequest, pc payments.ProviderCapability) writeCapabilityResponse {
	nn := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	limits := make([]amountLimitRequest, 0, len(pc.AmountLimits))
	for _, l := range pc.AmountLimits {
		limits = append(limits, amountLimitRequest{AssetCode: l.AssetCode, MinAmount: l.MinAmount, MaxAmount: l.MaxAmount})
	}
	sort.Slice(limits, func(i, j int) bool { return limits[i].AssetCode < limits[j].AssetCode })
	return writeCapabilityResponse{ID: id.String(), Narrowed: capabilityNarrowed(req, pc), Effective: capabilityEffectiveResponse{
		SupportedFiatCurrencies: nn(pc.SupportedFiatCurrencies), SupportedCryptoAssets: nn(pc.SupportedCryptoAssets),
		SupportedPaymentMethods: nn(pc.SupportedPaymentMethods), SupportedCountries: nn(pc.SupportedCountries),
		SupportsDeposit: pc.SupportsDeposit, SupportsWithdrawal: pc.SupportsWithdrawal, SupportsRefundReversal: pc.SupportsRefundReversal,
		AmountLimits: limits, Priority: pc.Priority, Status: string(pc.Status),
	}}
}

// capabilityNarrowed reports whether the stored configuration differs from the request (lists compared as sets, nil as
// empty; amount limits by asset).
func capabilityNarrowed(req writeCapabilityRequest, eff payments.ProviderCapability) bool {
	if !sameStringSet(req.SupportedFiatCurrencies, eff.SupportedFiatCurrencies) ||
		!sameStringSet(req.SupportedCryptoAssets, eff.SupportedCryptoAssets) ||
		!sameStringSet(req.SupportedPaymentMethods, eff.SupportedPaymentMethods) ||
		!sameStringSet(req.SupportedCountries, eff.SupportedCountries) ||
		req.SupportsDeposit != eff.SupportsDeposit || req.SupportsWithdrawal != eff.SupportsWithdrawal ||
		req.SupportsRefundReversal != eff.SupportsRefundReversal || req.Priority != eff.Priority ||
		req.Status != string(eff.Status) || len(req.AmountLimits) != len(eff.AmountLimits) {
		return true
	}
	stored := make(map[string]payments.AmountLimit, len(eff.AmountLimits))
	for _, l := range eff.AmountLimits {
		stored[l.AssetCode] = l
	}
	for _, l := range req.AmountLimits {
		if s, ok := stored[l.AssetCode]; !ok || s.MinAmount != l.MinAmount || s.MaxAmount != l.MaxAmount {
			return true
		}
	}
	return false
}

func sameStringSet(a, b []string) bool {
	as, bs := map[string]struct{}{}, map[string]struct{}{}
	for _, v := range a {
		as[v] = struct{}{}
	}
	for _, v := range b {
		bs[v] = struct{}{}
	}
	if len(as) != len(bs) {
		return false
	}
	for v := range as {
		if _, ok := bs[v]; !ok {
			return false
		}
	}
	return true
}
