package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// A11 (PAY-K3-STATEMENT-SOURCE-WIRING-1, R3-OBS-BOUND-1): the payment statement SOURCE REGISTRY.
//
// paymentStatementSources() (payment_statement_wiring.go) is still the ONE list of sources this binary
// schedules. This registry turns that list into the lookup the reconciliation sweep uses per tenant: provider
// id -> the source that speaks for it. The sweep (reconciliation.RunSweepScoped) asks, for each tenant, which
// payment providers the tenant has configured, and resolves each through SourceFor; a configured provider the
// registry cannot resolve is NOT reconciled and the sweep says so (an audited failed run, a durable
// reconciliation.run_failed alert with phase no_statement_source, and the
// reconciliation_payment_unreconciled gauge) - it is never skipped silently and never reads as clean.
//
// Construction is the production guard for statement sources, in the style of
// providerkind.RefuseSyntheticInProduction (and in addition to it, which already refuses every Synthetic
// component the bundle registers):
//
//   - production: a Synthetic (MOCK) source, or a source that does not implement providerkind.ProductionEligible,
//     is REFUSED (startup error). A MOCK statement can never vouch for a provider in production;
//   - everywhere: a nil source, a blank/malformed or duplicate provider id is refused (two sources for one
//     provider would make "the" source ambiguous), and a NON-Synthetic source must attest a read-only
//     credential (statement.ReadOnlyAttested): a statement source never holds the payment/payout credential;
//   - non-production: a Synthetic source is admitted, and only there (AllowSynthetic follows the environment,
//     and the sweep independently treats a Synthetic source as ineligible when AllowSynthetic is false).
//
// All-or-nothing: an invalid list yields no registry.

type paymentStatementRegistry struct {
	byProvider     map[string]statement.PaymentStatementSource
	order          []string
	allowSynthetic bool
}

func newPaymentStatementRegistry(environment string, srcs []statement.PaymentStatementSource) (*paymentStatementRegistry, error) {
	production := environment == "production"
	r := &paymentStatementRegistry{byProvider: map[string]statement.PaymentStatementSource{}, allowSynthetic: !production}
	var problems []string
	for i, src := range srcs {
		if isNilStatementSource(src) {
			problems = append(problems, fmt.Sprintf("source #%d is nil", i))
			continue
		}
		id := src.ProviderID()
		if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
			problems = append(problems, fmt.Sprintf("source #%d (%T) has an empty or malformed provider id", i, src))
			continue
		}
		if _, dup := r.byProvider[id]; dup {
			problems = append(problems, fmt.Sprintf("duplicate source for provider %q", id))
			continue
		}
		synthetic := isSyntheticComponent(src)
		bad := false
		if production {
			if synthetic {
				problems = append(problems, fmt.Sprintf("provider %q: a Synthetic (MOCK) statement source is refused in production (%T)", id, src))
				bad = true
			} else if _, ok := any(src).(providerkind.ProductionEligible); !ok {
				problems = append(problems, fmt.Sprintf("provider %q: statement source %T is not production-eligible", id, src))
				bad = true
			}
		}
		if !synthetic {
			if ro, ok := any(src).(statement.ReadOnlyAttested); !ok || !ro.ReadOnlyStatementCredential() {
				problems = append(problems, fmt.Sprintf("provider %q: statement source %T does not attest a read-only credential", id, src))
				bad = true
			}
		}
		if bad {
			continue
		}
		r.byProvider[id] = src
		r.order = append(r.order, id)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("payment statement source registry: %s", strings.Join(problems, "; "))
	}
	sort.Strings(r.order)
	return r, nil
}

// SourceFor implements reconciliation.PaymentSourceResolver. A nil registry resolves nothing (fail closed).
func (r *paymentStatementRegistry) SourceFor(providerID string) (statement.PaymentStatementSource, bool) {
	if r == nil {
		return nil, false
	}
	src, ok := r.byProvider[providerID]
	return src, ok
}

// Providers returns the registered provider ids, sorted.
func (r *paymentStatementRegistry) Providers() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.order...)
}

// SweepConfig is the reconciliation sweep configuration this registry drives: the resolver, the per-run line
// bound and whether Synthetic sources are admitted (only where the registry admitted them).
func (r *paymentStatementRegistry) SweepConfig(runLineBound int) reconciliation.PaymentSweepConfig {
	cfg := reconciliation.PaymentSweepConfig{RunLineBound: runLineBound}
	if r != nil {
		cfg.Resolver = r
		cfg.AllowSynthetic = r.allowSynthetic
	}
	return cfg
}
