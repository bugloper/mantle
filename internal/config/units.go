package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Bytes is a byte quantity that parses human units from YAML: 10GiB, 512MB,
// 4MiB, or a bare integer. Operators write sizes with units; a config format
// that demands 10737418240 is a config format people get wrong.
type Bytes int64

var byteUnits = []struct {
	suffix string
	factor int64
}{
	// Longest suffixes first so "KiB" is not matched as "B".
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
	{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	{"B", 1},
}

// ParseBytes parses a byte quantity. Both IEC (MiB) and SI (MB) units are
// accepted and mean what they say — 1 MB is 1,000,000 bytes and 1 MiB is
// 1,048,576. Conflating them is a classic source of "why is my quota wrong".
func ParseBytes(s string) (Bytes, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty byte quantity")
	}
	upper := strings.ToUpper(s)
	for _, u := range byteUnits {
		if !strings.HasSuffix(upper, u.suffix) {
			continue
		}
		numPart := strings.TrimSpace(upper[:len(upper)-len(u.suffix)])
		if numPart == "" {
			continue
		}
		n, err := strconv.ParseFloat(numPart, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a valid byte quantity: %w", s, err)
		}
		if n < 0 {
			return 0, fmt.Errorf("%q is negative", s)
		}
		return Bytes(n * float64(u.factor)), nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid byte quantity: expected a number "+
			"optionally suffixed with KiB, MiB, GiB, TiB, KB, MB, GB or TB", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return Bytes(n), nil
}

func (b *Bytes) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		v, err := ParseBytes(s)
		if err != nil {
			return err
		}
		*b = v
		return nil
	}
	var n int64
	if err := unmarshal(&n); err != nil {
		return fmt.Errorf("expected a byte quantity such as 10GiB, or a plain integer")
	}
	if n < 0 {
		return fmt.Errorf("byte quantity %d is negative", n)
	}
	*b = Bytes(n)
	return nil
}

func (b Bytes) MarshalYAML() (any, error) { return b.String(), nil }

// String renders a byte count in IEC units, choosing the largest unit that
// leaves a value at or above one.
func (b Bytes) String() string {
	const unit = 1024
	n := int64(b)
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.4g%ciB", float64(n)/float64(div), "KMGT"[exp])
}

func (b Bytes) Int64() int64 { return int64(b) }

// Duration wraps time.Duration so YAML can carry "24h", "90d", or "5m".
// time.ParseDuration has no day unit, which is inconvenient for retention
// windows that operators think about in days.
type Duration time.Duration

// ParseDuration parses a Go duration, additionally accepting a "d" (days) and
// "w" (weeks) suffix on an otherwise plain number.
func ParseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		v, err := strconv.ParseFloat(n, 64)
		if err == nil {
			return Duration(time.Duration(v * float64(24*time.Hour))), nil
		}
	}
	if n, ok := strings.CutSuffix(s, "w"); ok {
		v, err := strconv.ParseFloat(n, 64)
		if err == nil {
			return Duration(time.Duration(v * float64(7*24*time.Hour))), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid duration: expected a value such as 30s, 5m, 24h, 7d or 2w", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q is negative", s)
	}
	return Duration(d), nil
}

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf("expected a duration such as 24h or 7d")
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }
func (d Duration) String() string            { return time.Duration(d).String() }
func (d Duration) Std() time.Duration        { return time.Duration(d) }
