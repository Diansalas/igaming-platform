package httpserver

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/assetregistry"
)

// assetExponents resolves the decimal exponents (ADR 0007 / the Asset registry) of the given asset codes through the
// existing registry lookup (assetregistry.GetAsset). The registry is platform-wide reference data readable by any
// session (assets_read USING (true)), so a plain non-tenant read transaction is used. An unknown asset is an error,
// never a guessed exponent.
func assetExponents(ctx context.Context, deps Deps, codes ...string) (map[string]int16, error) {
	out := map[string]int16{}
	err := deps.DB.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, code := range codes {
			if _, ok := out[code]; ok {
				continue
			}
			a, err := assetregistry.GetAsset(ctx, tx, code)
			if err != nil {
				return err
			}
			out[code] = a.DecimalExponent
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// exponentRef returns the exponent for code as a pointer, or nil when it is not in exps (fail closed: the field is
// then JSON null and a client must not format the amount).
func exponentRef(exps map[string]int16, code string) *int16 {
	e, ok := exps[code]
	if !ok {
		return nil
	}
	return &e
}

// resolutionExponentsOrFail is for READ routes: an asset that cannot be resolved is a 500 and no body is returned.
func resolutionExponentsOrFail(ctx context.Context, deps Deps, w http.ResponseWriter, logger observabilityLogger, requestID string, codes ...string) (map[string]int16, bool) {
	exps, err := assetExponents(ctx, deps, codes...)
	if err != nil {
		logger.Error("resolution_asset_exponent_lookup_failed")
		apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
		return nil, false
	}
	return exps, true
}

// resolutionExponentsBestEffort is for routes that have ALREADY committed a change (request, decide, cancel): failing
// the response would hide a committed decision, so a failed lookup yields a null exponent instead (never a guess).
func resolutionExponentsBestEffort(ctx context.Context, deps Deps, logger observabilityLogger, codes ...string) map[string]int16 {
	exps, err := assetExponents(ctx, deps, codes...)
	if err != nil {
		logger.Error("resolution_asset_exponent_lookup_failed")
		return map[string]int16{}
	}
	return exps
}
