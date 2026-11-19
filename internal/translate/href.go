package translate

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// ObjectHref is the deterministic iCloud href for a UID (§7.1):
// <calendar>/calbridge-<base32(sha256(uid))[:26]>.ics. Deterministic so a
// crash between PUT and the store commit is recoverable (§8.6), and opaque
// so the UID never needs escaping.
func ObjectHref(calendarHref, uid string) string {
	sum := sha256.Sum256([]byte(uid))
	enc := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
	return strings.TrimSuffix(calendarHref, "/") + "/calbridge-" + enc[:26] + ".ics"
}
