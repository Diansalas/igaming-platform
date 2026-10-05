package alerting

// Derived delivery states shown by the status and list APIs. They are derived
// from the latest alert_deliveries row; alerts.state (open/acked/resolved) is
// the human workflow and is never written by the dispatcher (AL-10).
const (
	DeliveryStatePending          = "pending"
	DeliveryStateRetrying         = "retrying"
	DeliveryStateDelivered        = "delivered" // ONLY via a human-notification channel
	DeliveryStateRecordedNonHuman = "recorded_non_human"
	DeliveryStateUnrouted         = "unrouted"
	DeliveryStateDeliveryFailed   = "delivery_failed"
	DeliveryStateSuppressed       = "suppressed"
)

// KnownHumanChannelKinds lists the channel kinds KNOWN to notify a person.
// It is empty: no human-notification channel is implemented. Display (the
// derived delivery state and `human_notification`/`notified_a_person`) follows
// THIS list, so an unknown or unclassified kind can never read as "delivered".
// A real kind adds itself here together with its adapter and four-eyes.
var KnownHumanChannelKinds = map[ChannelKind]bool{}

// ChannelKindIsHumanNotification mirrors the database function
// alerting_channel_kind_is_human_notification: fail-closed, only log and mock
// are known non-human; any other kind is presumed to reach a person. A "sent"
// through a non-human kind is NEVER presented as a human notification.
func ChannelKindIsHumanNotification(k ChannelKind) bool {
	switch k {
	case ChannelLog, ChannelMock:
		return false
	default:
		return true
	}
}

// NotEvaluated is the readiness view before any evaluation has happened (or
// when no dispatcher runs in this process): every severity NOT ready.
func NotEvaluated() []SeverityReadiness { return notReadyAll(ReadinessNotEvaluated, nil) }

// DeliveryState derives the delivery state of an alert from its latest
// delivery event (empty when there is none) and that row's channel kind.
func DeliveryState(latestEvent string, channelKind ChannelKind) string {
	switch latestEvent {
	case "", "claimed":
		return DeliveryStatePending
	case "failed":
		return DeliveryStateRetrying
	case "sent":
		if KnownHumanChannelKinds[channelKind] {
			return DeliveryStateDelivered
		}
		return DeliveryStateRecordedNonHuman
	case "unrouted":
		return DeliveryStateUnrouted
	case "dead":
		return DeliveryStateDeliveryFailed
	case "suppressed_simulation":
		return DeliveryStateSuppressed
	default:
		return DeliveryStatePending
	}
}
