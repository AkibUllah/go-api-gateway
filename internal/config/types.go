package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from Go duration strings ("250ms", "1m30s").
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string like \"5s\"", n.Line)
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, s)
	}
	if v < 0 {
		return fmt.Errorf("line %d: duration %q must not be negative", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

// ByteSize is a byte count that unmarshals from integers or strings such as
// "512KB", "10MB", "1MiB". Decimal (KB/MB/GB) and binary (KiB/MiB/GiB) units are supported.
type ByteSize int64

var byteUnits = []struct {
	suffix string
	mult   int64
}{
	// Longest suffixes first so "KiB" is not read as "B".
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
	{"B", 1},
}

// ParseByteSize parses a human readable size.
func ParseByteSize(s string) (ByteSize, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	for _, u := range byteUnits {
		if strings.HasSuffix(t, u.suffix) {
			mult = u.mult
			t = strings.TrimSpace(strings.TrimSuffix(t, u.suffix))
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return ByteSize(n * mult), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: invalid size", n.Line)
	}
	v, err := ParseByteSize(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

// ExpandEnv replaces ${VAR} and ${VAR:-default} in s. A reference to an unset
// variable without a default is an error, so a missing secret fails loudly at
// startup instead of becoming an empty string. "$$" escapes a literal dollar.
func ExpandEnv(s string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	var missing []string
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			b.WriteByte(c)
			continue
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("config: unterminated ${ at offset %d", i)
		}
		expr := s[i+2 : i+2+end]
		name, def, hasDef := strings.Cut(expr, ":-")
		if val, ok := lookup(name); ok && val != "" {
			b.WriteString(val)
		} else if hasDef {
			b.WriteString(def)
		} else {
			missing = append(missing, name)
		}
		i += end + 2
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("config: environment variables not set: %s", strings.Join(missing, ", "))
	}
	return b.String(), nil
}
