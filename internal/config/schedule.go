package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression: minute, hour, day-of-month,
// month, day-of-week.
//
// This is deliberately a small local implementation rather than a dependency.
// The only expression most operators will ever write is the default "0 3 * * *",
// and a scheduler is not where a registry should be taking on supply-chain
// surface (SEC-15).
type Schedule struct {
	minutes     uint64 // bitmask 0-59
	hours       uint64 // bitmask 0-23
	daysOfMonth uint64 // bitmask 1-31
	months      uint64 // bitmask 1-12
	daysOfWeek  uint64 // bitmask 0-6, Sunday = 0

	// domRestricted and dowRestricted record whether each field was narrowed
	// from "*". Cron's rule is that when both day fields are restricted, a day
	// matching either one matches — an inconsistency inherited from Vixie cron
	// that every implementation must reproduce or surprise people.
	domRestricted bool
	dowRestricted bool

	expr string
}

type cronField struct {
	name     string
	min, max uint
	// names maps optional textual aliases, such as month and weekday names.
	names map[string]uint
}

var (
	fieldMinute = cronField{name: "minute", min: 0, max: 59}
	fieldHour   = cronField{name: "hour", min: 0, max: 23}
	fieldDOM    = cronField{name: "day of month", min: 1, max: 31}
	fieldMonth  = cronField{name: "month", min: 1, max: 12, names: map[string]uint{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	fieldDOW = cronField{name: "day of week", min: 0, max: 6, names: map[string]uint{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

// ParseSchedule parses a five-field cron expression. The common @-shorthands
// are accepted because operators reach for them.
func ParseSchedule(expr string) (*Schedule, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, fmt.Errorf("a five-field cron expression such as \"0 3 * * *\" (daily at 03:00)")
	}
	if shorthand, ok := shorthands[strings.ToLower(trimmed)]; ok {
		trimmed = shorthand
	}

	parts := strings.Fields(trimmed)
	if len(parts) != 5 {
		return nil, fmt.Errorf("a five-field cron expression (minute hour day-of-month month day-of-week), "+
			"got %d field(s) in %q", len(parts), expr)
	}

	s := &Schedule{expr: expr}
	var err error
	if s.minutes, _, err = parseField(parts[0], fieldMinute); err != nil {
		return nil, err
	}
	if s.hours, _, err = parseField(parts[1], fieldHour); err != nil {
		return nil, err
	}
	if s.daysOfMonth, s.domRestricted, err = parseField(parts[2], fieldDOM); err != nil {
		return nil, err
	}
	if s.months, _, err = parseField(parts[3], fieldMonth); err != nil {
		return nil, err
	}
	if s.daysOfWeek, s.dowRestricted, err = parseField(parts[4], fieldDOW); err != nil {
		return nil, err
	}
	return s, nil
}

var shorthands = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

// parseField parses one comma-separated cron field, returning its bitmask and
// whether it was restricted from "*".
func parseField(field string, spec cronField) (uint64, bool, error) {
	var mask uint64
	restricted := true

	for _, term := range strings.Split(field, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			return 0, false, fmt.Errorf("empty term in the %s field %q", spec.name, field)
		}

		step := uint(1)
		if base, stepStr, found := strings.Cut(term, "/"); found {
			n, err := strconv.ParseUint(stepStr, 10, 32)
			if err != nil || n == 0 {
				return 0, false, fmt.Errorf("step %q in the %s field is not a positive integer", stepStr, spec.name)
			}
			step = uint(n)
			term = base
		}

		var lo, hi uint
		switch {
		case term == "*":
			lo, hi = spec.min, spec.max
			if step == 1 {
				restricted = false
			}
		default:
			loStr, hiStr, isRange := strings.Cut(term, "-")
			var err error
			if lo, err = parseValue(loStr, spec); err != nil {
				return 0, false, err
			}
			if isRange {
				if hi, err = parseValue(hiStr, spec); err != nil {
					return 0, false, err
				}
			} else if step > 1 {
				// "5/15" means "from 5 to the end of the range, every 15".
				hi = spec.max
			} else {
				hi = lo
			}
			if lo > hi {
				return 0, false, fmt.Errorf("range %q in the %s field runs backwards", term, spec.name)
			}
		}

		for v := lo; v <= hi; v += step {
			mask |= 1 << v
		}
	}

	if mask == 0 {
		return 0, false, fmt.Errorf("the %s field %q matches nothing", spec.name, field)
	}
	return mask, restricted, nil
}

func parseValue(s string, spec cronField) (uint, error) {
	s = strings.TrimSpace(s)
	if spec.names != nil {
		if v, ok := spec.names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid %s value (expected %d-%d)", s, spec.name, spec.min, spec.max)
	}
	v := uint(n)
	// Cron traditionally accepts 7 for Sunday alongside 0.
	if spec.name == fieldDOW.name && v == 7 {
		v = 0
	}
	if v < spec.min || v > spec.max {
		return 0, fmt.Errorf("%s value %d is out of range (expected %d-%d)", spec.name, v, spec.min, spec.max)
	}
	return v, nil
}

// Next returns the first matching time strictly after t, in t's location.
// It returns the zero time if no match exists within five years, which happens
// only for impossible dates such as "0 0 30 2 *".
func (s *Schedule) Next(t time.Time) time.Time {
	// Advance to the start of the next minute; the schedule has minute resolution.
	next := t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)

	for next.Before(limit) {
		if s.months&(1<<uint(next.Month())) == 0 {
			// Skip to the first day of the next month.
			next = time.Date(next.Year(), next.Month(), 1, 0, 0, 0, 0, next.Location()).AddDate(0, 1, 0)
			continue
		}
		if !s.matchesDay(next) {
			next = time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, next.Location()).AddDate(0, 0, 1)
			continue
		}
		if s.hours&(1<<uint(next.Hour())) == 0 {
			next = time.Date(next.Year(), next.Month(), next.Day(), next.Hour(), 0, 0, 0, next.Location()).Add(time.Hour)
			continue
		}
		if s.minutes&(1<<uint(next.Minute())) == 0 {
			next = next.Add(time.Minute)
			continue
		}
		return next
	}
	return time.Time{}
}

// matchesDay applies the day-of-month / day-of-week rule: if both fields are
// restricted the match is a union, otherwise it is an intersection.
func (s *Schedule) matchesDay(t time.Time) bool {
	domMatch := s.daysOfMonth&(1<<uint(t.Day())) != 0
	dowMatch := s.daysOfWeek&(1<<uint(t.Weekday())) != 0
	if s.domRestricted && s.dowRestricted {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}

func (s *Schedule) String() string { return s.expr }
