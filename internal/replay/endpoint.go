package replay

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/optimm/Gmeter/internal/runner"
)

// OtherEndpoints labels the requests of every endpoint that did not get a row of
// its own. It cannot be mistaken for a real endpoint, which always has a method
// and a path.
const OtherEndpoints = "(other)"

// Endpoint names the endpoint a request is to: its method and its path, such
// as "GET /orders/:id".
//
// The query string is left out, because that is where tokens and search terms
// are and because every different one would otherwise be a different endpoint.
// For the same reason a path segment that is an identifier becomes ":id": one
// made only of digits, a UUID, or a hexadecimal string of 16 characters or more
// (a hash). Everything else is kept as it is.
func Endpoint(method, target string) string {
	if method == "" {
		method = "GET"
	}
	path, _, _ := strings.Cut(target, "?")
	segments := strings.Split(path, "/")
	for i, s := range segments {
		if isIdentifier(s) {
			segments[i] = ":id"
		}
	}
	return strings.ToUpper(method) + " " + strings.Join(segments, "/")
}

func isIdentifier(s string) bool {
	switch {
	case s == "":
		return false
	case isUUID(s):
		return true
	case len(s) >= 16:
		return strings.IndexFunc(s, func(c rune) bool { return !isHex(c) }) < 0
	default:
		return strings.IndexFunc(s, func(c rune) bool { return c < '0' || c > '9' }) < 0
	}
}

func isHex(c rune) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// isUUID reports whether s is in the 8-4-4-4-12 form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !isHex(c) {
			return false
		}
	}
	return true
}

// endpointOf names r's endpoint by the first group that matches it, or by its
// shape (Endpoint) if none does.
func endpointOf(r runner.Request, groups []Group) string {
	for _, g := range groups {
		if name, ok := g.Name(r.Method, r.Target); ok {
			return name
		}
	}
	return Endpoint(r.Method, r.Target)
}

// LabelEndpoints sets every request's Label to its endpoint: the first of
// groups that matches it, or else its Endpoint, so that a run can
// report each endpoint on its own, using at most max different labels: the
// max-1 endpoints with the most requests, and OtherEndpoints for all the rest.
// With room for every endpoint there is no OtherEndpoints. Endpoints with equal
// counts are ranked by name, so the result does not depend on the order of the
// trace.
//
// Call it after KeepMethods, so that the ranking counts what will be sent.
func (t *Trace) LabelEndpoints(max int, groups ...Group) error {
	if max < 2 {
		return fmt.Errorf("the endpoint breakdown needs room for at least 2 rows (the busiest endpoints, and the rest together), got %d", max)
	}
	// One string per endpoint, shared by its requests: a trace has millions of
	// requests and a few dozen endpoints.
	names := make([]string, len(t.Requests))
	count := map[string]int{}
	shared := map[string]string{}
	for i, r := range t.Requests {
		name := endpointOf(r, groups)
		if s, ok := shared[name]; ok {
			name = s
		} else {
			shared[name] = name
		}
		names[i] = name
		count[name]++
	}

	keep := map[string]bool{}
	if len(count) <= max {
		for name := range count {
			keep[name] = true
		}
	} else {
		ranked := make([]string, 0, len(count))
		for name := range count {
			ranked = append(ranked, name)
		}
		slices.SortFunc(ranked, func(a, b string) int {
			return cmp.Or(cmp.Compare(count[b], count[a]), cmp.Compare(a, b))
		})
		for _, name := range ranked[:max-1] {
			keep[name] = true
		}
	}
	for i := range t.Requests {
		t.Requests[i].Label = OtherEndpoints
		if keep[names[i]] {
			t.Requests[i].Label = names[i]
		}
	}
	return nil
}

// Group is a rule, given by the user, that names the endpoint of the requests
// whose path matches it, for paths that the shape of their segments does not
// group: a slug such as /products/blue-shoes, or a file name.
type Group struct {
	method   string   // only requests of this method match; "" any method
	segments []string // the pattern's path segments; "*" or ":name" match any one non-empty segment
	rest     bool     // the pattern ended in "**", which matches whatever follows
	pattern  string   // the path as given, which is the endpoint's name
}

// ParseGroup reads a rule such as "/products/:slug", "/static/**" or
// "POST /orders/*/items". A segment "*" or ":name" stands for any one segment,
// a last segment "**" for the rest of the path, and anything else for itself. A
// method limits the rule to requests of that method.
func ParseGroup(s string) (Group, error) {
	f := strings.Fields(s)
	var g Group
	switch len(f) {
	case 1:
		g.pattern = f[0]
	case 2:
		g.method, g.pattern = strings.ToUpper(f[0]), f[1]
		if !isMethod(g.method) {
			return Group{}, fmt.Errorf("group %q: %q is not an HTTP method", s, f[0])
		}
	default:
		return Group{}, fmt.Errorf(`group %q: want a path, optionally after a method, such as "/products/:slug" or "GET /static/**"`, s)
	}
	if !strings.HasPrefix(g.pattern, "/") {
		return Group{}, fmt.Errorf("group %q: the path must start with \"/\"", s)
	}
	if strings.ContainsAny(g.pattern, "?#") {
		return Group{}, fmt.Errorf("group %q: a path only; the query string is never part of an endpoint", s)
	}
	g.segments = strings.Split(g.pattern, "/")[1:]
	for i, seg := range g.segments {
		if seg == "**" {
			if i != len(g.segments)-1 {
				return Group{}, fmt.Errorf("group %q: ** can only be the last segment", s)
			}
			g.segments, g.rest = g.segments[:i], true
		}
	}
	return g, nil
}

// Name is the endpoint g gives a request, its method and the rule's path, and
// whether the request matches.
func (g Group) Name(method, target string) (string, bool) {
	method = strings.ToUpper(method)
	if method == "" {
		method = "GET"
	}
	if g.method != "" && g.method != method {
		return "", false
	}
	path, _, _ := strings.Cut(target, "?")
	segments := strings.Split(path, "/")[1:]
	if len(segments) < len(g.segments) || (!g.rest && len(segments) != len(g.segments)) {
		return "", false
	}
	for i, want := range g.segments {
		switch {
		case want == "*" || strings.HasPrefix(want, ":"):
			if segments[i] == "" {
				return "", false
			}
		case want != segments[i]:
			return "", false
		}
	}
	return method + " " + g.pattern, true
}
