// Package channels holds the provider-neutral alert delivery channels that notify a person: EmailSink (over the
// internal/email provider interface) and WebhookSink (one HMAC-signed HTTPS endpoint with SSRF protection). They
// implement alerting.Sink and are wired in cmd/platform-api only when configured (ALERT_EMAIL_RECIPIENTS,
// ALERT_WEBHOOK_URL). They live in their own package, not in internal/alerting, so that the alerting core (which the
// reconciliation scheduler imports transitively) keeps its reviewed, read-only dependency set: no HTTP client, no
// email, no egress code is reachable from the reconciliation package. See docs/runbooks/alert-delivery-channels.md.
package channels
