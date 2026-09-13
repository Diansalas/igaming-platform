package httpserver

import (
	"context"
	"errors"
	"net/http"

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
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: subjectID,
				Action: "provider_capability.configured", TargetType: "provider_capability", TargetID: capabilityID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"provider_id": providerID, "status": req.Status, "priority": req.Priority},
			})
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
		writeJSON(w, http.StatusOK, map[string]string{"id": capabilityID.String()})
	}
}
