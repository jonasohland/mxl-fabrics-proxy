// Package ports owns the agent's fabric port allocation.
//
// Port allocation is the agent's job, not the server's (§7.4): the agent has ground truth —
// it can probe, and it knows what it already started — while the server cannot verify a port
// it hands out. The range is configured per node so operators can write firewall rules, and
// note that the fabric connection is inbound to the *destination* node, so the range must be
// open there.
//
// This file defines the range type only. The allocator lands in M5.
package ports

import (
	"fmt"
	"strconv"
	"strings"
)

// Range is an inclusive range of TCP/UDP port numbers.
type Range struct {
	Low  uint16
	High uint16
}

// ParseRange parses "low-high", or "port" for a single-port range.
func ParseRange(s string) (Range, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Range{}, fmt.Errorf("port range: empty")
	}

	low, high, found := strings.Cut(s, "-")
	if !found {
		high = low
	}

	lo, err := parsePort(low)
	if err != nil {
		return Range{}, err
	}
	hi, err := parsePort(high)
	if err != nil {
		return Range{}, err
	}

	if lo > hi {
		return Range{}, fmt.Errorf("port range: low %d is above high %d", lo, hi)
	}

	return Range{Low: lo, High: hi}, nil
}

func parsePort(s string) (uint16, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil {
		return 0, fmt.Errorf("port range: invalid port %q", strings.TrimSpace(s))
	}
	if v == 0 {
		return 0, fmt.Errorf("port range: port 0 is not usable; the worker binds whatever `service` says and has no fallback")
	}
	return uint16(v), nil
}

// IsZero reports whether the range was never set.
//
// Worth a method of its own: the zero Range is {0, 0}, whose Count is 1, so a Count-based
// emptiness check would treat an unset range as a usable one-port range and allocate port 0.
// ParseRange rejects port 0, so a zero Range can only come from a struct that was never
// populated — an omitted YAML key, say.
func (r Range) IsZero() bool {
	return r.Low == 0 && r.High == 0
}

// Count returns the number of ports in the range.
func (r Range) Count() int {
	if r.High < r.Low {
		return 0
	}
	return int(r.High) - int(r.Low) + 1
}

// Contains reports whether port is within the range.
func (r Range) Contains(port uint16) bool {
	return port >= r.Low && port <= r.High
}

func (r Range) String() string {
	if r.Low == r.High {
		return strconv.FormatUint(uint64(r.Low), 10)
	}
	return strconv.FormatUint(uint64(r.Low), 10) + "-" + strconv.FormatUint(uint64(r.High), 10)
}

// UnmarshalText lets Range be used directly as a kong flag and a YAML/JSON field.
func (r *Range) UnmarshalText(text []byte) error {
	parsed, err := ParseRange(string(text))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// MarshalText is the inverse of UnmarshalText.
func (r Range) MarshalText() ([]byte, error) {
	return []byte(r.String()), nil
}
