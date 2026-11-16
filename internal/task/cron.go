package task

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A five-field cron expression: minute hour day-of-month month day-of-week.
// Supported per field: "*", lists ("1,2"), ranges ("1-5"), steps ("*/5",
// "1-20/3"), month names (JAN-DEC) and day names (SUN-SAT). Day-of-week takes
// 0-7 with both 0 and 7 meaning Sunday. When day-of-month and day-of-week are
// both restricted they are OR-ed, as every other cron does.
//
// Everything is evaluated in one location (the daemon's local zone by
// default), so a schedule follows wall-clock time across DST. A daily time
// that a spring-forward skips runs at the shifted instant, and one that an
// autumn fall-back repeats runs once, not twice: Next never returns a time at
// or before the instant it was asked about.

// Cron is a parsed expression bound to a location.
type Cron struct {
	expr  string
	loc   *time.Location
	min   [60]bool
	hour  [24]bool
	dom   [32]bool // 1-31
	month [13]bool // 1-12
	dow   [7]bool  // 0-6, Sunday = 0
	// A field left as "*" does not restrict the day; the OR rule only applies
	// when both day fields do.
	domAny, dowAny bool
}

// MaxHorizon bounds the search in Next: an expression that matches no instant
// within five years (30 FEB) yields the zero time.
const MaxHorizon = 5 * 365 * 24 * time.Hour

// shortcuts are the @-forms holaclaw accepts.
var shortcuts = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// ParseCron compiles expr for loc (nil means time.Local).
func ParseCron(expr string, loc *time.Location) (*Cron, error) {
	if loc == nil {
		loc = time.Local
	}
	raw := strings.TrimSpace(expr)
	if raw == "" {
		return nil, fmt.Errorf("cron: empty expression")
	}
	if sub, ok := shortcuts[strings.ToLower(raw)]; ok {
		raw = sub
	} else if strings.HasPrefix(raw, "@") {
		return nil, fmt.Errorf("cron: unknown shortcut %q (known: @hourly, @daily, @weekly, @monthly)", expr)
	}
	fields := strings.Fields(raw)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: %q has %d fields, want 5 (minute hour day-of-month month day-of-week)", expr, len(fields))
	}
	c := &Cron{expr: strings.TrimSpace(expr), loc: loc}
	if err := parseField(fields[0], 0, 59, nil, c.min[:]); err != nil {
		return nil, fmt.Errorf("cron: minute: %w", err)
	}
	if err := parseField(fields[1], 0, 23, nil, c.hour[:]); err != nil {
		return nil, fmt.Errorf("cron: hour: %w", err)
	}
	if err := parseField(fields[2], 1, 31, nil, c.dom[:]); err != nil {
		return nil, fmt.Errorf("cron: day of month: %w", err)
	}
	if err := parseField(fields[3], 1, 12, monthNames, c.month[:]); err != nil {
		return nil, fmt.Errorf("cron: month: %w", err)
	}
	if err := parseDOW(fields[4], c.dow[:]); err != nil {
		return nil, fmt.Errorf("cron: day of week: %w", err)
	}
	c.domAny, c.dowAny = isAny(fields[2]), isAny(fields[4])
	return c, nil
}

// String returns the expression as it was given.
func (c *Cron) String() string { return c.expr }

// isAny reports whether a day field leaves the day unrestricted. "*/1" is a
// spelling of "*"; every other step narrows the field.
func isAny(f string) bool { return f == "*" || f == "*/1" || f == "?" }

// parseField fills set (indexed by value) from one comma-separated field.
func parseField(f string, lo, hi int, names map[string]int, set []bool) error {
	if strings.TrimSpace(f) == "" {
		return fmt.Errorf("empty field")
	}
	for _, part := range strings.Split(f, ",") {
		if err := parsePart(part, lo, hi, names, set); err != nil {
			return err
		}
	}
	return nil
}

// parsePart handles one list element: "*", "a", "a-b", and any of those with
// a "/n" step.
func parsePart(part string, lo, hi int, names map[string]int, set []bool) error {
	part = strings.TrimSpace(part)
	if part == "" {
		return fmt.Errorf("empty list element")
	}
	step := 1
	if i := strings.IndexByte(part, '/'); i >= 0 {
		n, err := strconv.Atoi(strings.TrimSpace(part[i+1:]))
		if err != nil || n <= 0 {
			return fmt.Errorf("bad step in %q", part)
		}
		step, part = n, strings.TrimSpace(part[:i])
		if part == "" {
			return fmt.Errorf("bad step in %q", part)
		}
	}
	var first, last int
	switch {
	case part == "*" || part == "?":
		first, last = lo, hi
	case strings.ContainsRune(part[1:], '-'): // skip a leading "-" so "-1" fails as a value
		i := strings.IndexByte(part[1:], '-') + 1
		a, err := parseValue(part[:i], lo, hi, names)
		if err != nil {
			return err
		}
		b, err := parseValue(part[i+1:], lo, hi, names)
		if err != nil {
			return err
		}
		if a > b {
			return fmt.Errorf("range %q is inverted", part)
		}
		first, last = a, b
	default:
		v, err := parseValue(part, lo, hi, names)
		if err != nil {
			return err
		}
		// A step on a single value runs from it to the end of the field
		// ("5/10" = 5,15,25,…), as Vixie cron does.
		first, last = v, v
		if step > 1 {
			last = hi
		}
	}
	for v := first; v <= last; v += step {
		set[v] = true
	}
	return nil
}

func parseValue(s string, lo, hi int, names map[string]int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("%d is out of range %d-%d", v, lo, hi)
	}
	return v, nil
}

// parseDOW parses day-of-week, where 7 is a second spelling of Sunday.
func parseDOW(f string, set []bool) error {
	var wide [8]bool
	if err := parseField(f, 0, 7, dayNames, wide[:]); err != nil {
		return err
	}
	for i, on := range wide {
		if on {
			set[i%7] = true
		}
	}
	return nil
}

// Next returns the first instant strictly after `after` that the expression
// matches, in the cron's location. It returns the zero time when nothing
// matches within MaxHorizon (an impossible date such as "0 0 30 2 *").
func (c *Cron) Next(after time.Time) time.Time {
	limit := after.Add(MaxHorizon)
	t := after.In(c.loc)
	y, mo, d := t.Date()
	h, mi, _ := t.Clock()
	cur := time.Date(y, mo, d, h, mi, 0, 0, c.loc).Add(time.Minute)

	// Wall-clock walk, day by day: whole months and days are skipped at once,
	// and a matching day is scanned in wall-clock order. Building each
	// candidate with time.Date is what makes DST come out right: a time the
	// spring forward skips lands on the instant the clock jumped to, and a
	// wall-clock minute the autumn fall-back repeats yields one instant, so
	// the task runs once.
	for !cur.After(limit) {
		y, mo, d := cur.Date()
		if !c.month[int(mo)] {
			cur = time.Date(y, mo+1, 1, 0, 0, 0, 0, c.loc)
			continue
		}
		if !c.dayMatch(d, int(cur.Weekday())) {
			cur = time.Date(y, mo, d+1, 0, 0, 0, 0, c.loc)
			continue
		}
		if t := c.firstInDay(y, mo, d, after); !t.IsZero() {
			return t
		}
		cur = time.Date(y, mo, d+1, 0, 0, 0, 0, c.loc)
	}
	return time.Time{}
}

// firstInDay returns the first matching instant of one calendar day that is
// strictly after `after`, or the zero time when the day has none left.
func (c *Cron) firstInDay(y int, mo time.Month, d int, after time.Time) time.Time {
	for h := 0; h < 24; h++ {
		if !c.hour[h] {
			continue
		}
		for mi := 0; mi < 60; mi++ {
			if !c.min[mi] {
				continue
			}
			cand := time.Date(y, mo, d, h, mi, 0, 0, c.loc)
			// time.Date normalises a nonexistent local time forward; a
			// candidate that lands on another day is not this day's run.
			if cy, cmo, cd := cand.Date(); cy != y || cmo != mo || cd != d {
				continue
			}
			if cand.After(after) {
				return cand
			}
		}
	}
	return time.Time{}
}

// dayMatch applies the OR rule: with both day fields restricted, either one
// selects the day.
func (c *Cron) dayMatch(dom, dow int) bool {
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return c.dow[dow]
	case c.dowAny:
		return c.dom[dom]
	default:
		return c.dom[dom] || c.dow[dow]
	}
}

// MinInterval is the shortest gap between consecutive runs, sampled over the
// occurrences of the week after `after`. It bounds how often a model-created
// task may wake the daemon. A schedule with fewer than two occurrences in
// that window reports MaxHorizon.
func (c *Cron) MinInterval(after time.Time) time.Duration {
	window := after.Add(8 * 24 * time.Hour)
	shortest := MaxHorizon
	prev := c.Next(after)
	if prev.IsZero() {
		return shortest
	}
	for i := 0; i < 2000; i++ {
		next := c.Next(prev)
		if next.IsZero() || next.After(window) {
			break
		}
		if d := next.Sub(prev); d < shortest {
			shortest = d
		}
		prev = next
	}
	return shortest
}
