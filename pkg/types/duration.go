package types

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// FlexibleDuration is a time.Duration that accepts a Go duration string ("45m")
// or an integer nanosecond count in JSON and YAML.
type FlexibleDuration time.Duration

// Std returns the duration, or 0 when unset.
func (d *FlexibleDuration) Std() time.Duration {
	if d == nil {
		return 0
	}
	return time.Duration(*d)
}

// UnmarshalJSON accepts "45m" or a nanosecond integer.
func (d *FlexibleDuration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		return d.parseString(s)
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*d = FlexibleDuration(time.Duration(n))
	return nil
}

// MarshalJSON writes a Go duration string.
func (d FlexibleDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalYAML accepts a duration string or an integer.
func (d *FlexibleDuration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string or integer")
	}
	if node.Tag == "!!int" {
		var n int64
		if err := node.Decode(&n); err != nil {
			return err
		}
		*d = FlexibleDuration(time.Duration(n))
		return nil
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	return d.parseString(s)
}

// MarshalYAML writes a Go duration string.
func (d FlexibleDuration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

func (d *FlexibleDuration) parseString(s string) error {
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = FlexibleDuration(parsed)
	return nil
}
