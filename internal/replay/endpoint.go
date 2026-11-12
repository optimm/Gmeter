package replay

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
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

// LabelEndpoints sets every request's Label to its Endpoint, so that a run can
// report each endpoint on its own, using at most max different labels: the
// max-1 endpoints with the most requests, and OtherEndpoints for all the rest.
// With room for every endpoint there is no OtherEndpoints. Endpoints with equal
// counts are ranked by name, so the result does not depend on the order of the
// trace.
//
// Call it after KeepMethods, so that the ranking counts what will be sent.
func (t *Trace) LabelEndpoints(max int) error {
	if max < 2 {
		return fmt.Errorf("the endpoint breakdown needs room for at least 2 rows (the busiest endpoints, and the rest together), got %d", max)
	}
	// One string per endpoint, shared by its requests: a trace has millions of
	// requests and a few dozen endpoints.
	names := make([]string, len(t.Requests))
	count := map[string]int{}
	shared := map[string]string{}
	for i, r := range t.Requests {
		name := Endpoint(r.Method, r.Target)
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
