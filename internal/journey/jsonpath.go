package journey

import (
	"fmt"
	"strconv"
	"strings"
)

// parsePath reads the deliberately small JSONPath subset of a scenario: "$",
// then ".field" and "[n]" segments, e.g. "$.token" or "$[0].id". A field name
// is letters, digits, "_" or "-".
func parsePath(s string) ([]pathSeg, error) {
	if s == "" {
		return nil, fmt.Errorf("an empty path")
	}
	if s[0] != '$' {
		return nil, fmt.Errorf("path %q must start with \"$\"", s)
	}
	var segs []pathSeg
	for rest := s[1:]; len(rest) > 0; {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			i := strings.IndexAny(rest, ".[")
			name := rest
			if i >= 0 {
				name = rest[:i]
			}
			if name == "" {
				return nil, fmt.Errorf("path %q has an empty field", s)
			}
			for _, c := range name {
				if !isFieldChar(c) {
					return nil, fmt.Errorf("path %q: %q is not a valid field name", s, name)
				}
			}
			segs = append(segs, pathSeg{field: name})
			rest = rest[len(name):]
		case '[':
			i := strings.IndexByte(rest, ']')
			if i < 0 {
				return nil, fmt.Errorf("path %q has an unclosed \"[\"", s)
			}
			n, err := strconv.Atoi(rest[1:i])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("path %q has an invalid index %q", s, rest[1:i])
			}
			segs = append(segs, pathSeg{index: n, isIndex: true})
			rest = rest[i+1:]
		default:
			return nil, fmt.Errorf("path %q: unexpected %q after \"$\" or a segment", s, string(rest[0]))
		}
	}
	return segs, nil
}

func isFieldChar(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
}

type pathSeg struct {
	field   string
	index   int
	isIndex bool
}

// evalPath evaluates segments against a decoded JSON value. ok is false when
// any segment does not resolve: a missing field or an out-of-range index.
func evalPath(root any, segs []pathSeg) (any, bool) {
	v := root
	for _, seg := range segs {
		if seg.isIndex {
			arr, ok := v.([]any)
			if !ok || seg.index >= len(arr) {
				return nil, false
			}
			v = arr[seg.index]
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok = m[seg.field]
		if !ok {
			return nil, false
		}
	}
	return v, true
}

// normalizeJSONPath accepts the design doc's bare keys in `json:` checks and
// extracts: "token" and ".token" mean "$.token". Explicit "$..." paths are
// kept as they are.
func normalizeJSONPath(s string) string {
	if strings.HasPrefix(s, "$") {
		return s
	}
	return "$." + strings.TrimPrefix(s, ".")
}

// stringifyJSON turns a decoded JSON value into the string a session variable
// holds. Numbers are integral when they are, so 42 reads "42", not "42".
func stringifyJSON(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", fmt.Errorf("the value is null")
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("the value is not a scalar: %T", v)
	}
}

// stringifyYAML is the same idea for values from the scenario file, so
// `equals: 42` and a JSON number compare equal.
func stringifyYAML(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}
