package main

import (
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/httpserver"
)

// toWebhookAdmissionSettings maps config.WebhookAdmissionConfig's plain
// values into httpserver.WebhookAdmissionSettings, field by field - no
// behaviour, just data (architect review AC1: "internal/config produces
// plain values; httpserver maps them to admission types" - this is that
// mapping, kept in cmd/platform-api so internal/httpserver never imports
// internal/config).
func toWebhookAdmissionSettings(c config.WebhookAdmissionConfig) httpserver.WebhookAdmissionSettings {
	toRB := func(rate map[string]float64, burst map[string]int) map[string]httpserver.WebhookRateBurst {
		out := make(map[string]httpserver.WebhookRateBurst, len(rate))
		for _, d := range config.WebhookDomains {
			out[d] = httpserver.WebhookRateBurst{Rate: rate[d], Burst: burst[d]}
		}
		return out
	}
	overrides := make([]httpserver.WebhookAdmissionOverride, 0, len(c.Overrides))
	for _, o := range c.Overrides {
		overrides = append(overrides, httpserver.WebhookAdmissionOverride{
			Domain: o.Domain, ProviderID: o.ProviderID, Tenant: o.Tenant, Rate: o.Rate, Burst: o.Burst,
		})
	}
	return httpserver.WebhookAdmissionSettings{
		Enabled:            c.Enabled,
		PerIPRPS:           c.PerIPRPS,
		PerIPBurst:         c.PerIPBurst,
		PreAuthRate:        toRB(c.PreAuthRate, c.PreAuthBurst),
		PreAuthUnknownRate: toRB(c.PreAuthUnknownRate, c.PreAuthUnknownBurst),
		VerifiedRate:       toRB(c.VerifiedRate, c.VerifiedBurst),
		InFlightGlobal:     c.InFlightGlobal,
		InFlightPerKey:     c.InFlightPerKey,
		InFlightUnknown:    c.InFlightUnknown,
		DBGateGlobal:       c.DBGateGlobal,
		DBGatePerKey:       c.DBGatePerKey,
		DBGateUnknown:      c.DBGateUnknown,
		DBGateWait:         c.DBGateWait,
		DomainTxPerTenant:  c.DomainTxPerTenant,
		DomainWait:         c.DomainWait,
		BodyReadTimeout:    c.BodyReadTimeout,
		DirectoryRefresh:   c.DirectoryRefresh,
		DirectoryCap:       c.DirectoryCap,
		IdleEvict:          c.IdleEvict,
		VerifiedMaxKeys:    c.VerifiedMaxKeys,
		PerIPMaxKeys:       c.PerIPMaxKeys,
		Overrides:          overrides,
	}
}
