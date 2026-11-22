package journey

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// checkOp is one check-op as the runner evaluates it: exists, equals or
// matches, compiled. The value comes in as a string, with a presence flag:
// for a header, header.Get plus "the header was present"; for the body, the
// text plus whether it was non-empty; for a JSON path, the stringified value
// plus whether the path resolved.
type checkOp struct {
	exists    *bool
	equals    any
	matches   string
	re        *regexp.Regexp
	hasExists, hasEquals, hasMatches bool
}

func (o checkOp) check(present bool, s string) error {
	switch {
	case o.hasExists:
		if *o.exists && !present {
			return fmt.Errorf("there is no such value")
		}
		if !*o.exists && present {
			return fmt.Errorf("there should be no value, got %q", truncate(s))
		}
	}
	if o.hasEquals {
		if !present {
			return fmt.Errorf("there is no value to check equals against")
		}
		if want := stringifyYAML(o.equals); s != want {
			return fmt.Errorf("got %q, want %q", truncate(s), want)
		}
	}
	if o.hasMatches {
		if !present {
			return fmt.Errorf("there is no value to check matches against")
		}
		if !o.re.MatchString(s) {
			return fmt.Errorf("%q does not match %q", truncate(s), o.re.String())
		}
	}
	return nil
}

func truncate(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

func (o *checkOp) compile(spec OpSpec) error {
	o.exists, o.equals, o.matches = spec.Exists, spec.Equals, spec.Matches
	o.hasExists = spec.Exists != nil
	o.hasEquals = spec.Equals != nil
	if spec.Matches != "" {
		o.hasMatches = true
		re, err := regexp.Compile(spec.Matches)
		if err != nil {
			return fmt.Errorf("matches: %w", err)
		}
		o.re = re
	}
	if !o.hasExists && !o.hasEquals && !o.hasMatches {
		return fmt.Errorf("an op needs exists, equals or matches")
	}
	return nil
}

// checker is one step's compiled checks.
type checker struct {
	statuses []checkRange
	headers  []struct {
		name string
		op   checkOp
	}
	body  *checkOp
	jsons []jsonCheck
}

type jsonCheck struct {
	raw  string
	path []pathSeg
	op   checkOp
}

type checkRange struct{ lo, hi int }

func newChecker(spec *CheckSpec) (*checker, error) {
	if spec == nil {
		return nil, nil
	}
	c := &checker{}
	switch t := spec.Status.(type) {
	case nil:
	case []any:
		for _, v := range t {
			r, err := parseStatus(v)
			if err != nil {
				return nil, err
			}
			c.statuses = append(c.statuses, r)
		}
	default:
		r, err := parseStatus(t)
		if err != nil {
			return nil, err
		}
		c.statuses = append(c.statuses, r)
	}
	for name, op := range spec.Header {
		if name == "" {
			return nil, fmt.Errorf("header checks need a header name")
		}
		var co checkOp
		if err := co.compile(op); err != nil {
			return nil, fmt.Errorf("header %s: %w", name, err)
		}
		c.headers = append(c.headers, struct {
			name string
			op   checkOp
		}{name, co})
	}
	if spec.Body != nil {
		var co checkOp
		if err := co.compile(*spec.Body); err != nil {
			return nil, fmt.Errorf("body: %w", err)
		}
		c.body = &co
	}
	for path, op := range spec.JSON {
		path = normalizeJSONPath(path)
		segs, err := parsePath(path)
		if err != nil {
			return nil, fmt.Errorf("json: %w", err)
		}
		var co checkOp
		if err := co.compile(op); err != nil {
			return nil, fmt.Errorf("json %s: %w", path, err)
		}
		c.jsons = append(c.jsons, jsonCheck{raw: path, path: segs, op: co})
	}
	return c, nil
}

func parseStatus(v any) (checkRange, error) {
	switch t := v.(type) {
	case int:
		return checkRange{t, t}, nil
	case string:
		if i := strings.Index(t, ".."); i >= 0 {
			lo, err := strconv.Atoi(strings.TrimSpace(t[:i]))
			if err != nil {
				return checkRange{}, fmt.Errorf("status %q is not a range like 200..399", t)
			}
			hi, err := strconv.Atoi(strings.TrimSpace(t[i+2:]))
			if err != nil || lo > hi {
				return checkRange{}, fmt.Errorf("status %q is not a range like 200..399", t)
			}
			return checkRange{lo, hi}, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return checkRange{}, fmt.Errorf("status %q is not a status or range like 200..399", t)
		}
		return checkRange{n, n}, nil
	default:
		return checkRange{}, fmt.Errorf("a status is a number or a range like 200..399, got %v", v)
	}
}

// check reports the response's status, headers and body against the compiled
// checks.
func (c *checker) check(status int, header http.Header, body []byte) error {
	if len(c.statuses) > 0 {
		ok := false
		for _, r := range c.statuses {
			if status >= r.lo && status <= r.hi {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("status %d is not one of the expected statuses", status)
		}
	}
	for _, h := range c.headers {
		vals := header.Values(h.name)
		if err := h.op.check(len(vals) > 0, strings.Join(vals, ", ")); err != nil {
			return fmt.Errorf("header %s: %w", h.name, err)
		}
	}
	if c.body != nil {
		if err := c.body.check(len(body) > 0, string(body)); err != nil {
			return fmt.Errorf("body: %w", err)
		}
	}
	for _, j := range c.jsons {
		var v any
		if len(body) > 0 {
			_ = json.Unmarshal(body, &v)
		}
		got, ok := evalPath(v, j.path)
		var str string
		if ok {
			str, _ = stringifyJSON(got)
		}
		if err := j.op.check(ok, str); err != nil {
			return fmt.Errorf("json %s: %w", j.raw, err)
		}
	}
	return nil
}

// extract is one extracted session variable: from a JSON path, a named
// header, or a regex over the body or a header.
type extract struct {
	name     string // the variable it becomes
	from     string // "json", "header" or "regex"
	path     string // JSON path, header name or regex text
	segs     []pathSeg
	re       *regexp.Regexp
	group    int
	header   string // for regex: a named header instead of the body
	optional bool
}

func compileExtract(name string, spec *ExtractSpec) (*extract, error) {
	if spec == nil {
		return nil, fmt.Errorf("an extract needs from and path")
	}
	if name == "" {
		return nil, fmt.Errorf("an extract needs a name")
	}
	e := &extract{name: name, from: spec.From, path: spec.Path, optional: spec.Optional}
	if e.from == "" {
		e.from = "json"
	}
	switch e.from {
	case "json":
		if e.path == "" {
			return nil, fmt.Errorf("extract %q: a json extract needs a path", name)
		}
		e.path = normalizeJSONPath(e.path)
		segs, err := parsePath(e.path)
		if err != nil {
			return nil, fmt.Errorf("extract %q: %w", name, err)
		}
		e.segs = segs
	case "header":
		if e.path == "" {
			return nil, fmt.Errorf("extract %q: a header extract needs a header name", name)
		}
	case "regex":
		if e.path == "" {
			return nil, fmt.Errorf("extract %q: a regex extract needs a pattern", name)
		}
		re, err := regexp.Compile(e.path)
		if err != nil {
			return nil, fmt.Errorf("extract %q: %w", name, err)
		}
		e.re = re
		group := spec.Group
		if group == 0 && len(re.SubexpNames()) > 1 {
			group = 1 // the default is the first group when the regex has one
		}
		if group < 0 || group >= len(re.SubexpNames()) {
			group = 0
		}
		e.group = group
		e.header = spec.Header
	default:
		return nil, fmt.Errorf("extract %q: from must be json, header or regex, got %q", name, spec.From)
	}
	return e, nil
}

// apply sets e's value on vars when it can be read from the response.
func (e *extract) apply(vars map[string]any, header http.Header, body []byte) error {
	var (
		val     string
		present bool
	)
	switch e.from {
	case "json":
		var v any
		if len(body) > 0 {
			_ = json.Unmarshal(body, &v)
		}
		if v != nil {
			if got, ok := evalPath(v, e.segs); ok {
				if s, err := stringifyJSON(got); err == nil {
					present, val = true, s
				}
			}
		}
	case "header":
		if vals := header.Values(e.path); len(vals) > 0 {
			present, val = true, strings.Join(vals, ", ")
		}
	case "regex":
		src := string(body)
		if e.header != "" {
			src = header.Get(e.header)
		}
		if m := e.re.FindStringSubmatch(src); m != nil && e.group < len(m) {
			present, val = true, m[e.group]
		}
	}
	if !present {
		if e.optional {
			return nil
		}
		return fmt.Errorf("extract %q: no value found to read", e.name)
	}
	vars[e.name] = val
	return nil
}
