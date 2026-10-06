package payments

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// B5 (PAY-POLL-ECHO-HARDENING-1): a provider-echoed asset code is attacker-influenced
// text (the PSP controls it) that would otherwise be written RAW and UNBOUNDED into
// append-only audit, where it can never be scrubbed. assetEchoRE is the shape of a real
// asset-registry code (upper-case letters and digits, with `_`/`-` separators for
// network-qualified codes, at most 16 bytes); anything else is recorded only as its length
// and a hash prefix, exactly like echoAuditMeta does for an echoed reference.
var assetEchoRE = regexp.MustCompile(`^[A-Z0-9_-]{1,16}$`)

// withAssetEcho adds the echoed asset code under key to meta (returned for chaining):
// the value itself only when it has the asset-code shape, otherwise key_len and
// key_sha256_prefix (never the value). An empty echo is recorded as empty (nothing to hide).
func withAssetEcho(meta map[string]any, key, v string) map[string]any {
	if v == "" || assetEchoRE.MatchString(v) {
		meta[key] = v
		return meta
	}
	sum := sha256.Sum256([]byte(v))
	meta[key+"_len"] = len(v)
	meta[key+"_sha256_prefix"] = hex.EncodeToString(sum[:])[:12]
	return meta
}
