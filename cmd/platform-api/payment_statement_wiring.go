package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// PAY-K3-STATEMENT-SOURCE-WIRING-1 (ADR 0101 §27.6, ADR 0095 §39.4).
//
// ONE list of payment statement sources feeds BOTH consumers:
//
//  1. the manual-resolution statement-source registry (payments.
//     StatementSourceRegistry), which decides from the process registry whether
//     an m2_declare_paid / m2_declare_not_paid may be submitted or executed for a
//     provider (LF O-4); and
//  2. the reconciliation scheduler's payment_statement stream
//     (reconciliation.RunSchedulerLoop(..., paySources...)).
//
// run() builds the slice once (providers.paymentStatementSources()), registers
// it with registerPaymentStatementSources before the HTTP server exists, and
// hands the very same variable to the scheduler. A provider id is therefore
// registered if and only if its stream is scheduled; TestRun_* pins the link.

// paymentStatementSources is the ONE list of payment statement sources this
// binary schedules and registers. Today: the MOCK source for the MOCK payments
// adapter only. A real PSP's source is appended HERE and nowhere else.
func (b providerBundle) paymentStatementSources() []statement.PaymentStatementSource {
	if b.PaymentsStmt == nil {
		return nil
	}
	return []statement.PaymentStatementSource{b.PaymentsStmt}
}

// isNilStatementSource is true for a nil interface or an interface holding a
// nil pointer (a typed nil would otherwise panic or look registered).
func isNilStatementSource(src statement.PaymentStatementSource) bool {
	if src == nil {
		return true
	}
	rv := reflect.ValueOf(src)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

// registerPaymentStatementSources records every scheduled source's provider id
// in reg. It FAILS STARTUP (returns an error) on a nil registry, a nil source,
// an empty/blank provider id or a duplicate provider id (two sources for one
// provider would make "the" source ambiguous). Nothing is registered unless the
// whole list is valid (all-or-nothing), so a partial registry never exists.
func registerPaymentStatementSources(reg *payments.StatementSourceRegistry, srcs []statement.PaymentStatementSource) error {
	if reg == nil {
		return fmt.Errorf("payment statement sources: nil registry")
	}
	ids := make([]string, 0, len(srcs))
	seen := map[string]bool{}
	for i, src := range srcs {
		if isNilStatementSource(src) {
			return fmt.Errorf("payment statement sources: source #%d is nil", i)
		}
		id := src.ProviderID()
		if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
			return fmt.Errorf("payment statement sources: source #%d (%T) has an empty or malformed provider id", i, src)
		}
		if seen[id] {
			return fmt.Errorf("payment statement sources: duplicate source for provider %q", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, id := range ids {
		reg.Register(id)
	}
	return nil
}

// isSyntheticComponent reports whether c carries the providerkind.Synthetic
// marker (a MOCK).
func isSyntheticComponent(c any) bool {
	_, ok := c.(providerkind.Synthetic)
	return ok
}

// checkPaymentStatementCoverage is the real-PSP startup readiness gate, in the
// style of providerkind.RefuseSyntheticInProduction (pure, no I/O, no globals,
// unit-testable with fake adapters):
//
//   - every REAL (non-Synthetic) payments adapter must have a REAL
//     (non-Synthetic) statement source with the SAME provider id in the
//     scheduled list - otherwise its deposits/payouts would have no statement
//     stream and M2 (both kinds) would silently depend on nothing; and
//   - a Synthetic (MOCK) statement source may never carry a real adapter's id,
//     so a MOCK statement can never vouch for a real PSP.
//
// It is environment-independent: a real adapter wired without its real source is
// refused everywhere. Today it is vacuous (the only adapter is the MOCK); it is
// the gate the first real PSP has to pass. The MOCK source never unlocks a real
// provider's M2 because the registry is keyed by provider id (H-W2).
func checkPaymentStatementCoverage(adapters map[string]payments.PaymentProvider, srcs []statement.PaymentStatementSource) error {
	realSource := map[string]bool{}
	mockSource := map[string]bool{}
	for _, src := range srcs {
		if isNilStatementSource(src) {
			continue // registerPaymentStatementSources refuses it
		}
		if isSyntheticComponent(src) {
			mockSource[src.ProviderID()] = true
		} else {
			realSource[src.ProviderID()] = true
		}
	}
	var problems []string
	for key, a := range adapters {
		if a == nil || isSyntheticComponent(a) {
			continue
		}
		id := a.Capabilities().ProviderID
		if id == "" {
			id = key
		}
		if !realSource[id] {
			problems = append(problems, fmt.Sprintf("real payments adapter %q has no real payment statement source with the same provider id", id))
		}
		if mockSource[id] {
			problems = append(problems, fmt.Sprintf("a MOCK payment statement source carries real payments adapter %q's provider id", id))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("payment statement coverage: %s", strings.Join(problems, "; "))
}
