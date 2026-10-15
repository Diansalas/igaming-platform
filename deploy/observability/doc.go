// Package observability holds the alert rule files in this directory, the example Prometheus scrape
// configuration and compose file, and their tests (rules_parity_test.go, exposition_test.go). The metric rules
// are backed by a real, scrapeable exposition (internal/observability, METRICS_ADDR) but are NOT DEPLOYED to any
// backend; the log-event rules and pending signals are NOT WIRED. ALERT-DELIVERY-1 is OPEN.
package observability
