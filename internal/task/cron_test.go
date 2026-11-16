package task

import (
	"testing"
	"time"
	_ "time/tzdata" // Europe/Madrid must resolve on a machine without tzdata
)

func madrid(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Fatalf("load Europe/Madrid: %v", err)
	}
	return loc
}

// mustParse parses an RFC3339 time in loc.
func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func TestCronNext(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		name, expr, from, want string
	}{
		{"every minute", "* * * * *", "2026-01-01 10:00", "2026-01-01 10:01"},
		{"every minute rolls the hour", "* * * * *", "2026-01-01 10:59", "2026-01-01 11:00"},
		{"fixed minute", "30 * * * *", "2026-01-01 10:00", "2026-01-01 10:30"},
		{"fixed minute next hour", "30 * * * *", "2026-01-01 10:30", "2026-01-01 11:30"},
		{"strictly after", "0 9 * * *", "2026-01-01 09:00", "2026-01-02 09:00"},
		{"daily", "0 9 * * *", "2026-01-01 08:59", "2026-01-01 09:00"},
		{"list of minutes", "0,15,45 * * * *", "2026-01-01 10:20", "2026-01-01 10:45"},
		{"list wraps", "0,15,45 * * * *", "2026-01-01 10:45", "2026-01-01 11:00"},
		{"minute range", "10-12 * * * *", "2026-01-01 10:10", "2026-01-01 10:11"},
		{"minute range end", "10-12 * * * *", "2026-01-01 10:12", "2026-01-01 11:10"},
		{"step from star", "*/15 * * * *", "2026-01-01 10:01", "2026-01-01 10:15"},
		{"step from star at zero", "*/15 * * * *", "2026-01-01 10:45", "2026-01-01 11:00"},
		{"range with step", "0-30/10 * * * *", "2026-01-01 10:05", "2026-01-01 10:10"},
		{"range with step ends", "0-30/10 * * * *", "2026-01-01 10:30", "2026-01-01 11:00"},
		{"value with step", "5/20 * * * *", "2026-01-01 10:06", "2026-01-01 10:25"},
		{"hour step", "0 */6 * * *", "2026-01-01 01:00", "2026-01-01 06:00"},
		{"hour list", "0 8,20 * * *", "2026-01-01 09:00", "2026-01-01 20:00"},
		{"hour range", "0 9-17 * * *", "2026-01-01 18:30", "2026-01-02 09:00"},
		{"day of month", "0 0 15 * *", "2026-01-16 00:00", "2026-02-15 00:00"},
		{"day of month rolls the year", "0 0 1 1 *", "2026-06-01 00:00", "2027-01-01 00:00"},
		{"month name", "0 0 1 MAR *", "2026-01-01 00:00", "2026-03-01 00:00"},
		{"month name lowercase", "0 0 1 dec *", "2026-01-01 00:00", "2026-12-01 00:00"},
		{"month name range", "0 0 1 jun-aug *", "2026-01-01 00:00", "2026-06-01 00:00"},
		{"month number", "0 0 1 7 *", "2026-01-01 00:00", "2026-07-01 00:00"},
		{"month step", "0 0 1 */3 *", "2026-02-01 00:00", "2026-04-01 00:00"},
		{"weekday number", "0 0 * * 1", "2026-01-01 00:00", "2026-01-05 00:00"}, // Monday
		{"weekday name", "0 0 * * MON", "2026-01-01 00:00", "2026-01-05 00:00"},
		{"weekday name lowercase", "0 0 * * fri", "2026-01-01 00:00", "2026-01-02 00:00"},
		{"weekday 0 is sunday", "0 0 * * 0", "2026-01-01 00:00", "2026-01-04 00:00"},
		{"weekday 7 is sunday", "0 0 * * 7", "2026-01-01 00:00", "2026-01-04 00:00"},
		{"weekday range", "0 0 * * MON-FRI", "2026-01-03 00:00", "2026-01-05 00:00"},
		{"weekday list", "0 0 * * sat,sun", "2026-01-01 00:00", "2026-01-03 00:00"},
		{"weekday step", "0 0 * * */2", "2026-01-01 00:00", "2026-01-03 00:00"}, // sun, tue, thu, sat
		{"dom and dow are OR-ed", "0 0 13 * FRI", "2026-01-01 00:00", "2026-01-02 00:00"},
		{"dom and dow OR picks the dom", "0 0 5 * SUN", "2026-01-03 00:00", "2026-01-04 00:00"},
		{"dom alone", "0 0 13 * *", "2026-01-01 00:00", "2026-01-13 00:00"},
		{"dow alone", "0 0 * * 2", "2026-01-01 00:00", "2026-01-06 00:00"},
		{"feb 29 on a leap year", "0 0 29 2 *", "2026-01-01 00:00", "2028-02-29 00:00"},
		{"feb 29 from a leap year", "0 0 29 2 *", "2024-03-01 00:00", "2028-02-29 00:00"},
		{"feb 30 never happens", "0 0 30 2 *", "2026-01-01 00:00", ""},
		{"apr 31 never happens", "0 0 31 4 *", "2026-01-01 00:00", ""},
		{"hourly shortcut", "@hourly", "2026-01-01 10:30", "2026-01-01 11:00"},
		{"daily shortcut", "@daily", "2026-01-01 10:30", "2026-01-02 00:00"},
		{"weekly shortcut", "@weekly", "2026-01-01 10:30", "2026-01-04 00:00"},
		{"monthly shortcut", "@monthly", "2026-01-15 10:30", "2026-02-01 00:00"},
		{"new year rollover", "0 0 1 * *", "2026-12-15 00:00", "2027-01-01 00:00"},
		{"whitespace is tolerated", "  0   9  *  *  *  ", "2026-01-01 00:00", "2026-01-01 09:00"},
		{"question mark is a star", "0 9 ? * ?", "2026-01-01 00:00", "2026-01-01 09:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCron(tc.expr, loc)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", tc.expr, err)
			}
			got := c.Next(at(t, loc, tc.from))
			if tc.want == "" {
				if !got.IsZero() {
					t.Fatalf("Next = %s, want the zero time", got)
				}
				return
			}
			want := at(t, loc, tc.want)
			if !got.Equal(want) {
				t.Fatalf("Next = %s, want %s", got, want)
			}
		})
	}
}

func TestCronDST(t *testing.T) {
	loc := madrid(t)
	cases := []struct {
		name, expr, from, want string
	}{
		// Spring forward: 2026-03-29, 02:00 CET becomes 03:00 CEST. A 02:30
		// job runs at the instant the wall clock jumps to.
		{"forward skipped time runs shifted", "30 2 * * *", "2026-03-28 03:00", "2026-03-29 03:30"},
		{"forward before the jump", "30 1 * * *", "2026-03-29 00:00", "2026-03-29 01:30"},
		{"forward after the jump", "30 4 * * *", "2026-03-29 00:00", "2026-03-29 04:30"},
		{"forward day continues", "30 2 * * *", "2026-03-29 12:00", "2026-03-30 02:30"},
		// Autumn back: 2026-10-25, 03:00 CEST becomes 02:00 CET, so 02:30
		// happens twice. The job runs once.
		{"back runs once", "30 2 * * *", "2026-10-24 12:00", "2026-10-25 02:30"},
		{"back does not repeat", "30 2 * * *", "2026-10-25 02:31", "2026-10-26 02:30"},
		{"back hourly keeps moving", "0 * * * *", "2026-10-25 02:00", "2026-10-25 03:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCron(tc.expr, loc)
			if err != nil {
				t.Fatalf("ParseCron: %v", err)
			}
			from := at(t, loc, tc.from)
			got := c.Next(from)
			want := at(t, loc, tc.want)
			if !got.Equal(want) {
				t.Fatalf("Next(%s) = %s, want %s", from, got, want)
			}
			if !got.After(from) {
				t.Fatalf("Next returned %s, which is not after %s", got, from)
			}
		})
	}
}

// Cron is wall-clock: on the autumn fall-back an hourly job runs once per
// wall-clock hour, so 02:00 comes up once and the repeated hour is not a
// second run. Occurrences stay strictly increasing.
func TestCronFallBackHourlyRunsOncePerWallHour(t *testing.T) {
	loc := madrid(t)
	c, err := ParseCron("0 * * * *", loc)
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	cur := at(t, loc, "2026-10-25 00:30")
	var seen []time.Time
	for i := 0; i < 5; i++ {
		cur = c.Next(cur)
		seen = append(seen, cur)
	}
	twos := 0
	for i, s := range seen {
		if i > 0 && !s.After(seen[i-1]) {
			t.Fatalf("occurrence %d (%s) is not after %s", i, s, seen[i-1])
		}
		if h, m, _ := s.Clock(); h == 2 && m == 0 {
			twos++
		}
	}
	if twos != 1 {
		t.Fatalf("02:00 appeared %d times in %v, want once", twos, seen)
	}
}

func TestParseCronErrors(t *testing.T) {
	cases := []struct{ name, expr string }{
		{"empty", ""},
		{"too few fields", "0 9 * *"},
		{"too many fields", "0 9 * * * *"},
		{"unknown shortcut", "@yearly"},
		{"minute out of range", "60 * * * *"},
		{"negative minute", "-1 * * * *"},
		{"hour out of range", "0 24 * * *"},
		{"day out of range", "0 0 32 * *"},
		{"day zero", "0 0 0 * *"},
		{"month out of range", "0 0 1 13 *"},
		{"weekday out of range", "0 0 * * 8"},
		{"bad name", "0 0 1 XXX *"},
		{"bad weekday name", "0 0 * * funday"},
		{"inverted range", "0 20-10 * * *"},
		{"zero step", "*/0 * * * *"},
		{"negative step", "*/-1 * * * *"},
		{"empty list element", "0,,5 * * * *"},
		{"not a number", "abc * * * *"},
		{"empty step target", "/5 * * * *"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if c, err := ParseCron(tc.expr, time.UTC); err == nil {
				t.Fatalf("ParseCron(%q) = %v, want an error", tc.expr, c)
			}
		})
	}
}

func TestCronMinInterval(t *testing.T) {
	cases := []struct {
		expr string
		want time.Duration
	}{
		{"* * * * *", time.Minute},
		{"*/5 * * * *", 5 * time.Minute},
		{"0,1 * * * *", time.Minute},
		{"0 * * * *", time.Hour},
		{"@daily", 24 * time.Hour},
		{"0 0 1 1 *", MaxHorizon}, // fewer than two occurrences in a day
	}
	from := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		c, err := ParseCron(tc.expr, time.UTC)
		if err != nil {
			t.Fatalf("ParseCron(%q): %v", tc.expr, err)
		}
		if got := c.MinInterval(from); got != tc.want {
			t.Fatalf("MinInterval(%q) = %s, want %s", tc.expr, got, tc.want)
		}
	}
}

func TestCronHorizon(t *testing.T) {
	c, err := ParseCron("0 0 30 2 *", time.UTC)
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if got := c.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); !got.IsZero() {
		t.Fatalf("Next = %s, want the zero time", got)
	}
	if got := c.String(); got != "0 0 30 2 *" {
		t.Fatalf("String = %q", got)
	}
}
