package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration that round-trips through JSON and YAML as a human string
// ("90s", "5m"). Config files and API payloads both carry durations, and a bare integer is
// ambiguous about its unit — a mistake that turns a 90-second lease into a 90-nanosecond
// one.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case string:
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("duration %q: %w", v, err)
		}
		*d = Duration(parsed)
		return nil
	case float64:
		// Bare numbers are seconds. project.yaml uses `leaseTtlSeconds: 90`, and this keeps
		// that spelling working without a separate field type.
		*d = Duration(time.Duration(v) * time.Second)
		return nil
	case nil:
		*d = 0
		return nil
	default:
		return fmt.Errorf("duration: unsupported type %T", raw)
	}
}

// OrDefault returns fallback when d is zero, so a partially-specified config still yields a
// working policy rather than an instantly-expiring lease.
func (d Duration) OrDefault(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}
