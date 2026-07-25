package ical

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// iso8601Duration matches the RFC 5545 DURATION form used by a VALARM TRIGGER, with an optional
// leading sign (a reminder is normally "-PT15M" = 15 minutes before the event).
var iso8601Duration = regexp.MustCompile(`^([+-]?)P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// ParseTrigger parses a relative VALARM TRIGGER (an ISO-8601 duration such as "-PT15M", "-P1D")
// into an offset to add to the occurrence start: negative = before the event. It reports ok=false
// for an empty/absolute/unparseable trigger (absolute DATE-TIME triggers are not handled here —
// the reminder scheduler simply skips them).
func ParseTrigger(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	m := iso8601Duration.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	// Require at least one component so a bare "P"/"PT" isn't treated as zero.
	if m[2] == "" && m[3] == "" && m[4] == "" && m[5] == "" && m[6] == "" {
		return 0, false
	}
	// Sum in seconds with overflow detection: an oversized/garbage component (e.g. a 20-digit week
	// count) must be rejected (ok=false), not silently wrapped into a bogus, sign-flipped offset.
	const maxSec = int64(1<<63-1) / int64(time.Second) // largest whole-second duration
	var secs int64
	units := []struct{ grp, mul int64 }{{2, 7 * 24 * 3600}, {3, 24 * 3600}, {4, 3600}, {5, 60}, {6, 1}}
	for _, u := range units {
		if m[u.grp] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[u.grp], 10, 64)
		if err != nil || n < 0 || n > maxSec/u.mul {
			return 0, false // out of range: reject rather than wrap
		}
		add := n * u.mul
		if secs > maxSec-add {
			return 0, false
		}
		secs += add
	}
	d := time.Duration(secs) * time.Second
	if m[1] == "-" {
		d = -d
	}
	return d, true
}
