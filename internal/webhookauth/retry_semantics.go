package webhookauth

import "fmt"

// WebhookRetrySemantics is the per-adapter, vendor-property declaration
// ADR 0097 §6.3/§20 AC6 requires before a webhook adapter's callback path
// can be rate-limited safely: it tells the admission layer (B1, ADR 0097
// §6.1) whether the vendor actually redelivers a 429/503, so a limited
// callback is never silently lost.
//
// This is a STATIC, per-adapter declaration - no DB, no per-tenant
// variance, and tenant configuration may never relax it (AC6). It is a
// placeholder for the ADR 0095 adapter capability manifest field of the
// same name and shape; AC6 explicitly rejects a second, interim
// per-provider config key as a source of truth, so this type and its
// registration-time guard (RequireRetrySemantics) are the only mechanism
// PRH-I4 ships. Until ADR 0095 lands and a real vendor contract exists,
// no ProductionEligible adapter declares this - only MOCK adapters exist,
// and MOCK adapters get the default admission behaviour (§6.3 "MOCK
// adapters: 429/503 as in §6.1") without needing to declare anything.
type WebhookRetrySemantics struct {
	// Retries429 reports whether the vendor redelivers a callback answered
	// with HTTP 429.
	Retries429 bool
	// Retries503 reports whether the vendor redelivers a callback answered
	// with HTTP 503.
	Retries503 bool
	// HonorsRetryAfter reports whether the vendor waits at least
	// Retry-After before redelivering.
	HonorsRetryAfter bool
	// RetryWindow is how long the vendor keeps retrying before giving up
	// and treating the event as failed. Feeds alerting only (ADR 0097 §8),
	// never an admission decision.
	RetryWindow int64 // nanoseconds; time.Duration, kept as int64 to avoid importing "time" for a single field
}

// RetrySemanticsSource is implemented by a webhook adapter that declares
// its WebhookRetrySemantics. It is optional: an adapter's Go type need not
// implement it at all (the common case today - every adapter is MOCK).
// RequireRetrySemantics uses a type assertion, not this interface being
// embedded in CasinoProvider/PaymentProvider/KYCProvider, so adding it
// never breaks an existing adapter's compile.
type RetrySemanticsSource interface {
	WebhookRetrySemantics() (WebhookRetrySemantics, bool)
}

// syntheticMarker restates providerkind.Synthetic structurally, exactly
// like scheme.go's syntheticComponent, so this package need not import
// internal/providerkind (keeping the dependency direction the same as the
// rest of this file: providerkind depends on nothing internal, and
// webhookauth already avoids importing it for the scheme-set checks).
type syntheticMarker interface {
	SyntheticComponent()
}

// RequireRetrySemantics is the fail-closed registration guard ADR 0097
// §20 AC6 requires: "a non-Synthetic webhook adapter without a
// declaration fails registration". A Synthetic (MOCK) adapter needs no
// declaration - it always gets the ADR 0097 §6.1 default behaviour. Any
// other adapter (unmarked, or a future ProductionEligible one) MUST
// implement RetrySemanticsSource and return ok=true, or this returns an
// error naming the offending provider id so startup fails instead of
// silently shipping a webhook adapter whose retry behaviour admission
// cannot reason about.
//
// domain is used only in the error message (e.g. "payments", "casino",
// "kyc"). adapters is the domain's provider_id -> adapter registry, exactly
// the map already passed to webhookauth.MustAdapterSchemeSet - callers
// wire this alongside that call, in the same orchestrator constructor.
func RequireRetrySemantics[A any](domain string, adapters map[string]A) error {
	for id, a := range adapters {
		if any(a) == nil {
			continue
		}
		if _, synthetic := any(a).(syntheticMarker); synthetic {
			continue
		}
		src, ok := any(a).(RetrySemanticsSource)
		if !ok {
			return fmt.Errorf("webhookauth: %s provider %q: adapter %T is not a Synthetic component and does not implement RetrySemanticsSource - a non-MOCK webhook adapter must declare WebhookRetrySemantics before registration (ADR 0097 §6.3/§20 AC6)", domain, id, a)
		}
		if _, declared := src.WebhookRetrySemantics(); !declared {
			return fmt.Errorf("webhookauth: %s provider %q: adapter %T implements RetrySemanticsSource but declared ok=false - a non-MOCK webhook adapter must declare WebhookRetrySemantics before registration (ADR 0097 §6.3/§20 AC6)", domain, id, a)
		}
	}
	return nil
}

// MustRequireRetrySemantics is RequireRetrySemantics for orchestrator
// constructors, which run at process start: any error panics, exactly
// like MustAdapterSchemeSet.
func MustRequireRetrySemantics[A any](domain string, adapters map[string]A) {
	if err := RequireRetrySemantics(domain, adapters); err != nil {
		panic(err.Error())
	}
}
