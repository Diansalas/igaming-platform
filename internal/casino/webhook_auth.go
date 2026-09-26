package casino

import (
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// casinoScheme is the casino domain's platform-defined MOCK wire scheme
// (webhookauth package doc: never a vendor format). Stage 10.2
// (CAS-WH-TENANT-1, ADR 0091, design §A/§C).
var casinoScheme = webhookauth.CasinoScheme()
