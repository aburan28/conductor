package admin

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a lifetime an administrator sets: "12h", "30d", "90d". Days are accepted
// because token policies are written in days, and time.ParseDuration stops at hours. It
// marshals as days when it is a whole number of them, so what an administrator typed is
// what they read back.
type Duration time.Duration

// Std returns the duration as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string {
	t := time.Duration(d)
	if t > 0 && t%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(t/(24*time.Hour)), 10) + "d"
	}
	return t.String()
}

// ParseDuration reads a Go duration, or a whole number of days ("90d"), or "0".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a duration (use 12h, 30d, …)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (use 12h, 30d, …)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return d, nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"12h\" or \"30d\"")
	}
	v, err := ParseDuration(s)
	*d = Duration(v)
	return err
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a duration is a string such as 12h or 30d", n.Line)
	}
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}
