package channels

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

// EmailSink delivers an alert as plain-text email through the provider-neutral internal/email.Provider
// (A9). Recipients are PLATFORM configuration (ALERT_EMAIL_RECIPIENTS), never the route's recipient_ref (an
// opaque id that is never an address) and never anything in the alert. There is no real email vendor yet
// (docs/decisions/0030 section 4): in development the provider is email.MockProvider, and then this sink
// reports HumanNotification() == false, so readiness, the delivery metric and the status API all say the
// alert reached no person. NewEmailSink additionally refuses a synthetic provider in a production
// environment, independently of the process-wide MOCK-ADAPTER-PROD-1 guard.
//
// The message carries only: the Kind, the severity, the subject tenant id, the discriminator (a server-side
// stable id string restricted to [A-Za-z0-9:_.-]), the alert id and a runbook pointer. It carries NO attribute
// value (some Kinds record operator-supplied reasons) and no player or staff data. The body is never logged.
//
// Delivery is at-least-once. The sink remembers, per process, which (dedup key, recipient) pairs were sent, so
// a retry after a partial multi-recipient failure re-sends only the recipients that failed; across restarts a
// duplicate is possible and the message carries the dedup key so a human can recognise it.
type EmailSink struct {
	provider   email.Provider
	recipients []string
	synthetic  bool
	logger     *slog.Logger
	sent       *sentSet
}

// NewEmailSink builds the email channel. environment is config.GuardEnvironment(); a synthetic provider is
// refused when it is "production". At least one recipient is required (a sink with nobody to notify would
// report "sent" for nothing).
func NewEmailSink(provider email.Provider, recipients []string, environment string, logger *slog.Logger) (*EmailSink, error) {
	if provider == nil {
		return nil, errors.New("alerting: email sink: no email provider")
	}
	if len(recipients) == 0 {
		return nil, errors.New("alerting: email sink: no recipients configured")
	}
	for _, r := range recipients {
		if r == "" || strings.ContainsAny(r, "\r\n,;<> ") {
			return nil, errors.New("alerting: email sink: recipient is not a bare address")
		}
	}
	if err := providerkind.RefuseSyntheticInProduction(environment, []providerkind.Registration{
		{Domain: "alerting", Name: "email_provider", Component: provider},
	}); err != nil {
		return nil, fmt.Errorf("alerting: email sink: %w", err)
	}
	_, synthetic := provider.(providerkind.Synthetic)
	if logger == nil {
		logger = slog.Default()
	}
	return &EmailSink{
		provider: provider, recipients: append([]string(nil), recipients...),
		synthetic: synthetic, logger: logger, sent: newSentSet(4096),
	}, nil
}

func (*EmailSink) ChannelKind() alerting.ChannelKind { return alerting.ChannelEmail }

// HumanNotification is false when the provider is a synthetic (MOCK) double: nothing reached a person.
func (s *EmailSink) HumanNotification() bool { return !s.synthetic }

// Deliver implements alerting.Sink.
func (s *EmailSink) Deliver(ctx context.Context, d alerting.Delivery) (alerting.Outcome, alerting.ErrorClass) {
	if s.sent.has(d.DedupKey) {
		return alerting.OutcomeSent, alerting.ErrorClassNone
	}
	if err := ctx.Err(); err != nil {
		return alerting.OutcomeFailed, classifyContextErr(err)
	}
	subject, body := renderEmail(d)
	failed := false
	var class alerting.ErrorClass
	for _, to := range s.recipients {
		pairKey := d.DedupKey + "|" + to
		if s.sent.has(pairKey) {
			continue
		}
		err := sendWithContext(ctx, s.provider, email.Message{To: to, Subject: subject, Body: body})
		if err != nil {
			failed = true
			if class == alerting.ErrorClassNone || class == alerting.ErrorClassUnavailable {
				class = classifyEmailErr(err)
			}
			continue
		}
		s.sent.add(pairKey)
	}
	if failed {
		// Only the error CLASS is logged: a provider error can carry the address or a vendor message.
		s.logger.Warn("alert_email_delivery_failed", "alert_id", d.AlertID, "error_class", string(class))
		return alerting.OutcomeFailed, class
	}
	s.sent.add(d.DedupKey)
	s.logger.Info("alert_email_delivered", "alert_id", d.AlertID, "kind", d.Kind, "severity", d.Severity,
		"recipient_count", len(s.recipients))
	return alerting.OutcomeSent, alerting.ErrorClassNone
}

// sendWithContext makes the provider call return when ctx ends even if the provider ignores ctx.
func sendWithContext(ctx context.Context, p email.Provider, m email.Message) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("alerting: email provider panicked")
			}
		}()
		done <- p.Send(ctx, m)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func classifyContextErr(err error) alerting.ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return alerting.ErrorClassTimeout
	}
	return alerting.ErrorClassUnavailable
}

func classifyEmailErr(err error) alerting.ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return alerting.ErrorClassTimeout
	}
	return alerting.ErrorClassUnavailable
}

// renderEmail builds the fixed-template message. Every interpolated value is either from a closed registry
// (Kind, severity), a UUID, or the discriminator, whose charset is enforced at raise time
// ([A-Za-z0-9:_.-]{1,160}); it is re-checked here so a value that bypassed raise validation can never inject
// a header or a line into the message.
func renderEmail(d alerting.Delivery) (subject, body string) {
	kind := safeToken(string(d.Kind))
	sev := safeToken(strings.ToUpper(string(d.Severity)))
	disc := safeToken(d.Discriminator)
	subject = fmt.Sprintf("[%s] iGaming platform alert: %s", sev, kind)
	var b strings.Builder
	fmt.Fprintf(&b, "An alert needs attention.\n\n")
	fmt.Fprintf(&b, "Kind:          %s\n", kind)
	fmt.Fprintf(&b, "Severity:      %s\n", sev)
	if d.SubjectTenantID != uuid.Nil {
		fmt.Fprintf(&b, "Subject:       tenant %s\n", d.SubjectTenantID)
	}
	fmt.Fprintf(&b, "Discriminator: %s\n", disc)
	fmt.Fprintf(&b, "Alert id:      %s\n", d.AlertID)
	fmt.Fprintf(&b, "Dedup key:     %s\n\n", safeToken(d.DedupKey))
	fmt.Fprintf(&b, "Response: docs/runbooks/observability-and-alerting.md (alert kind %s).\n", kind)
	fmt.Fprintf(&b, "This message contains identifiers only. Acknowledge or resolve the alert in the back office.\n")
	return subject, b.String()
}

// safeToken replaces anything outside [A-Za-z0-9:_.|-] with '_' (and bounds the length), so a template value can
// never carry a newline, a header separator or markup.
func safeToken(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '_', c == '.', c == '-', c == '|':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
