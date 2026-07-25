package ical

import (
	"testing"
	"time"
)

func TestParseTrigger(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"-PT15M", -15 * time.Minute, true},
		{"-PT30M", -30 * time.Minute, true},
		{"-PT1H", -time.Hour, true},
		{"-P1D", -24 * time.Hour, true},
		{"-P2W", -14 * 24 * time.Hour, true},
		{"-P30D", -30 * 24 * time.Hour, true},
		{"PT0S", 0, true},                                 // zero (at start)
		{"PT45M", 45 * time.Minute, true},                 // positive (after start)
		{"-P1DT2H30M", -(24*time.Hour + 2*time.Hour + 30*time.Minute), true},
		{"", 0, false},                                    // empty
		{"P", 0, false},                                   // no component
		{"PT", 0, false},                                  // no component
		{"20260710T120000Z", 0, false},                   // absolute → rejected
		{"garbage", 0, false},                             // malformed
		{"-P99999999999999999999W", 0, false},            // overflow → rejected, not wrapped
		{"PT9223372036854775807S", 0, false},             // seconds overflow → rejected
	}
	for _, c := range cases {
		got, ok := ParseTrigger(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseTrigger(%q) = (%v, %v); want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
