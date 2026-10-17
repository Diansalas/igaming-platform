package providerkind

import (
	"fmt"
	"sort"
	"strings"
)

// IDClaim is one provider_id a binary registers for one adapter family
// ("payments", "casino", "sportsbook", "kyc", ...). Several components of the
// SAME family may legitimately claim the same id (a payments adapter and its
// statement source); the same id claimed by two DIFFERENT families is a
// namespace collision.
type IDClaim struct {
	Family string
	ID     string
	// Source is for the error message only (e.g. "payments/provider").
	Source string
}

// RefuseProviderIDCollisions enforces that provider_id namespaces are
// DISJOINT across adapter families (ledger-finance C-2 / security S-2).
//
// Why: migration 0175 treats a provider id that has a payment attempt or a
// payment provider event in the tenant as a PAYMENT provider and then demands
// deposit-reversal proof for its tombstones. A casino provider id equal to a
// payment provider id would therefore make every unseen-original casino
// rollback tombstone fail closed (AP001) permanently, because the payment
// tables are append-only and only owner DDL can undo it.
//
// The function is pure (no I/O, no globals). Ids are compared trimmed and
// case-folded, which is stricter than the database's exact-text comparison.
// Empty ids are ignored (other startup checks own those). The error names
// every colliding id and the families and sources that claim it.
func RefuseProviderIDCollisions(claims []IDClaim) error {
	type owner struct{ family, source string }
	byID := map[string][]owner{}
	display := map[string]string{}
	for _, c := range claims {
		k := strings.ToLower(strings.TrimSpace(c.ID))
		if k == "" {
			continue
		}
		if _, ok := display[k]; !ok {
			display[k] = c.ID
		}
		byID[k] = append(byID[k], owner{c.Family, c.Source})
	}
	var msgs []string
	for k, owners := range byID {
		fams := map[string]bool{}
		for _, o := range owners {
			fams[o.family] = true
		}
		if len(fams) < 2 {
			continue
		}
		var parts []string
		for _, o := range owners {
			parts = append(parts, fmt.Sprintf("%s (%s)", o.family, o.source))
		}
		sort.Strings(parts)
		msgs = append(msgs, fmt.Sprintf("provider_id %q is claimed by more than one adapter family: %s", display[k], strings.Join(parts, ", ")))
	}
	if len(msgs) == 0 {
		return nil
	}
	sort.Strings(msgs)
	return fmt.Errorf("provider-id namespace collision (provider ids must be disjoint across payments, casino, sportsbook and kyc; "+
		"a casino id equal to a payment id permanently breaks casino rollback tombstones, migration 0175): %s", strings.Join(msgs, "; "))
}
