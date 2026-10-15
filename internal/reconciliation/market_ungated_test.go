package reconciliation

import "github.com/Diansalas/igaming-platform/internal/marketgate"

// testUngatedGuard is the explicit, named "no market gate" value the tests of this package pass where
// a MarketGuard is mandatory (SR8-3) but the test's subject is not market blocking. It is the only way
// to run an orchestrator entry point ungated; production code can never reference it (see
// internal/marketgate TestProductionCodeNeverUsesTheUngatedSentinels).
var testUngatedGuard = marketgate.UngatedForTests

var _ = testUngatedGuard
