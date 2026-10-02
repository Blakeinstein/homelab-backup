// Package schedule parses 5-field cron expressions and converts them to
// systemd OnCalendar values for generated timer units.
package schedule

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var dowNames = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// Cron represents a parsed 5-field cron expression.
type Cron struct {
	Min  []int
	Hour []int
	Dom  []int // -1 = wildcard
	Mon  []int // -1 = wildcard
	Dow  []int // -1 = wildcard
	Raw  string
}

func Parse(expr string) (*Cron, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression %q must have 5 fields (min hour dom mon dow), got %d", expr, len(fields))
	}
	c := &Cron{Raw: expr}
	var err error
	if c.Min, err = parseField(fields[0], 0, 59); err != nil {
		return nil, err
	}
	if c.Hour, err = parseField(fields[1], 0, 23); err != nil {
		return nil, err
	}
	c.Dom, err = parseFieldOptional(fields[2], 1, 31)
	if err != nil {
		return nil, err
	}
	c.Mon, err = parseFieldOptional(fields[3], 1, 12)
	if err != nil {
		return nil, err
	}
	c.Dow, err = parseFieldOptional(fields[4], 0, 7)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func parseField(field string, lo, hi int) ([]int, error) {
	vals, err := parseFieldOptional(field, lo, hi)
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		// "*"
		return fieldRange(lo, hi), nil
	}
	return vals, nil
}

func parseFieldOptional(field string, lo, hi int) ([]int, error) {
	// Dow: 7 == 0 (both Sunday)
	rng := regexp.MustCompile(`^(\*|\d+(?:-\d+)?)(?:/(\d+))?$`)
	var out []int
	addRange := func(a, b, step int) {
		for v := a; v <= b; v += step {
			if v == hi && lo == 0 && strings.Contains(field, "-") {
				// dow 7-0 style; still fine
			}
			out = append(out, v)
		}
	}
	for _, part := range strings.Split(field, ",") {
		m := rng.FindStringSubmatch(part)
		if m == nil {
			return nil, fmt.Errorf("invalid cron field %q", part)
		}
		step := 1
		if m[2] != "" {
			step, _ = strconv.Atoi(m[2])
			if step < 1 {
				step = 1
			}
		}
		if m[1] == "*" {
			if len(out) == 0 {
				return nil, nil // wildcard marker: empty slice means wild
			}
			continue
		}
		if strings.Contains(m[1], "-") {
			bits := strings.SplitN(m[1], "-", 2)
			a, _ := strconv.Atoi(bits[0])
			b, _ := strconv.Atoi(bits[1])
			if a > b && lo == 0 && hi == 7 && b == 0 {
				b = 7 // allow 5-0 wraparound for dow (Fri..Sun)
			}
			if a < lo || b > hi || a > b {
				return nil, fmt.Errorf("cron field %q out of range [%d,%d]", part, lo, hi)
			}
			addRange(a, b, step)
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if v == 7 && hi == 7 {
			v = 0
		}
		if v < lo || v > hi {
			return nil, fmt.Errorf("cron field %q out of range [%d,%d]", part, lo, hi)
		}
		addRange(v, v, step)
	}
	return out, nil
}

func fieldRange(lo, hi int) []int {
	out := []int{}
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

// Next returns the next matching time strictly after `from`.
// (Vixie semantics for dom/dow interaction are simplified: if either is
// restricted, match whichever one matches.)
func (c *Cron) Next(from time.Time) time.Time {
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(2, 0, 0)
	for ; t.Before(limit); t = t.Add(time.Minute) {
		if !intListHas(c.Min, t.Minute()) {
			continue
		}
		if !intListHas(c.Hour, t.Hour()) {
			continue
		}
		domOK := c.Dom == nil || intListHas(c.Dom, t.Day())
		dowOK := c.Dow == nil || intListHas(c.Dow, int(t.Weekday()))
		monOK := c.Mon == nil || intListHas(c.Mon, int(t.Month()))
		if !monOK {
			continue
		}
		if c.Dom != nil && c.Dow != nil {
			if !domOK && !dowOK {
				continue
			}
		} else if !domOK || !dowOK {
			continue
		}
		return t
	}
	return t // unreachable for sane expressions
}

func intListHas(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// OnCalendar converts the cron expression to one or more systemd
// OnCalendar strings (systemd lists can't combine * with values in a
// field, so we emit multiple lines when only some fields are wild).
func (c *Cron) OnCalendar() []string {
	domWild := len(c.Dom) == 0 // empty = wildcard
	monWild := len(c.Mon) == 0
	dowWild := len(c.Dow) == 0

	timePart := fmt.Sprintf("%s:%s:00", padJoin(c.Hour, 2), padJoin(c.Min, 2))

	var dates []string
	switch {
	case domWild && monWild:
		dates = []string{"*-*-*"}
	case monWild:
		dates = []string{fmt.Sprintf("*-*-%s", padJoin(c.Dom, 2))}
	case domWild:
		dates = []string{fmt.Sprintf("*-%s-*", padJoin(c.Mon, 2))}
	default:
		doms := padJoin(c.Dom, 2)
		for _, mon := range c.Mon {
			dates = append(dates, fmt.Sprintf("*-%02d-%s", mon, doms))
		}
	}

	dow := ""
	if !dowWild {
		names := []string{}
		for _, d := range c.Dow {
			names = append(names, dowNames[d%7])
		}
		dow = strings.Join(names, ",") + " "
	}

	lines := make([]string, 0, len(dates))
	for _, d := range dates {
		lines = append(lines, dow+d+" "+timePart)
	}
	sort.Strings(lines)
	return lines
}

func padJoin(vals []int, width int) string {
	sorted := make([]int, len(vals))
	copy(sorted, vals)
	sort.Ints(sorted)
	var parts []string
	seen := map[int]bool{}
	for _, v := range sorted {
		if seen[v] {
			continue
		}
		seen[v] = true
		parts = append(parts, fmt.Sprintf("%0*d", width, v))
	}
	return strings.Join(parts, ",")
}
