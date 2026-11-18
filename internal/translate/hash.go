package translate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-ical"
)

// volatileProps are excluded from ContentHash: DTSTAMP changes on every
// render, and LAST-MODIFIED follows Google's `updated`, which moves when any
// other guest responds.
var volatileProps = map[string]bool{
	ical.PropDateTimeStamp: true,
	ical.PropLastModified:  true,
}

// ContentHash is a sha256 over the calendar's content minus DTSTAMP,
// LAST-MODIFIED, PARTSTAT, VALARMs and the properties carried over from the
// iCloud object (X-APPLE-*, X-MOZ-*, X-WR-*). Two renders of the same Google
// state hash equal regardless of what Apple added to the stored object.
// Compute it on calbridge's own rendering only, never on an object read back
// from iCloud (P0-5: Apple rewrites parameters, TRANSP and PRODID).
func ContentHash(cal *ical.Calendar) string {
	h := sha256.New()
	if cal != nil {
		hashComponent(h, cal.Component)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type writer interface{ Write([]byte) (int, error) }

func hashComponent(w writer, c *ical.Component) {
	_, _ = w.Write([]byte("BEGIN:" + c.Name + "\n"))
	names := make([]string, 0, len(c.Props))
	for n := range c.Props {
		if volatileProps[n] || isPreserved(n) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		lines := make([]string, 0, len(c.Props[n]))
		for _, p := range c.Props[n] {
			lines = append(lines, canonicalProp(p))
		}
		sort.Strings(lines)
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n"))
		}
	}
	for _, child := range c.Children {
		if child.Name == ical.CompAlarm {
			continue
		}
		hashComponent(w, child)
	}
	_, _ = w.Write([]byte("END:" + c.Name + "\n"))
}

func canonicalProp(p ical.Prop) string {
	var b strings.Builder
	b.WriteString(p.Name)
	keys := make([]string, 0, len(p.Params))
	for k := range p.Params {
		if p.Name == ical.PropAttendee && k == ical.ParamParticipationStatus {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(";" + k + "=" + strings.Join(p.Params[k], ","))
	}
	b.WriteString(":" + p.Value)
	return b.String()
}

// Encode serialises cal as iCalendar text with lines folded at 75 octets
// (RFC 5545 §3.1), never splitting a UTF-8 sequence. go-ical's encoder does
// not fold.
func Encode(cal *ical.Calendar) ([]byte, error) {
	var raw bytes.Buffer
	if err := ical.NewEncoder(&raw).Encode(cal); err != nil {
		return nil, err
	}
	return Fold(raw.Bytes()), nil
}

// Fold folds every CRLF-terminated line longer than 75 octets.
func Fold(b []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(b) + len(b)/70*3)
	for len(b) > 0 {
		i := bytes.Index(b, []byte("\r\n"))
		var line []byte
		if i < 0 {
			line, b = b, nil
		} else {
			line, b = b[:i], b[i+2:]
		}
		limit := 75
		for len(line) > limit {
			cut := limit
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			out.Write(line[:cut])
			out.WriteString("\r\n ")
			line = line[cut:]
			limit = 74 // the leading space counts
		}
		out.Write(line)
		out.WriteString("\r\n")
	}
	return out.Bytes()
}
