package replay

import (
	"strings"
	"testing"

	"github.com/optimm/Gmeter/internal/runner"
)

func mustGroup(t *testing.T, s string) Group {
	t.Helper()
	g, err := ParseGroup(s)
	if err != nil {
		t.Fatalf("ParseGroup(%q): %v", s, err)
	}
	return g
}

func TestAGroupNamesTheRequestsItMatches(t *testing.T) {
	for _, tc := range []struct {
		group, method, target string
		want                  string // "" if it does not match
	}{
		// A :name or * segment stands for exactly one segment.
		{"/products/:slug", "GET", "/products/blue-shoes", "GET /products/:slug"},
		{"/products/:slug", "POST", "/products/red-hat?ref=x", "POST /products/:slug"},
		{"/products/:slug", "GET", "/products", ""},
		{"/products/:slug", "GET", "/products/", ""}, // an empty segment is not a slug
		{"/products/:slug", "GET", "/products/a/b", ""},
		{"/products/*/reviews", "GET", "/products/blue-shoes/reviews", "GET /products/*/reviews"},
		{"/products/*/reviews", "GET", "/products/blue-shoes/questions", ""},
		{"/products/*/reviews", "GET", "/products/blue-shoes/reviews?page=2", "GET /products/*/reviews"}, // the query is not part of the path
		// ** at the end stands for whatever follows, nothing included.
		{"/static/**", "GET", "/static/css/app.3f9a.css", "GET /static/**"},
		{"/static/**", "GET", "/static", "GET /static/**"},
		{"/static/**", "GET", "/statics/x", ""},
		{"/static/css/**", "GET", "/static", ""},
		// A method limits the group to it.
		{"POST /orders/:id", "POST", "/orders/7", "POST /orders/:id"},
		{"POST /orders/:id", "GET", "/orders/7", ""},
		{"POST /orders/:id", "post", "/orders/7", "POST /orders/:id"}, // methods are compared in upper case
		{"post /orders/:id", "", "/orders/7", ""},                     // no method is sent as GET
		{"GET /orders/:id", "", "/orders/7", "GET /orders/:id"},
		// Literals are compared as they are.
		{"/Products/:slug", "GET", "/products/x", ""},
	} {
		g := mustGroup(t, tc.group)
		got, ok := g.Name(tc.method, tc.target)
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("%q on %s %s = %q, %v; want %q", tc.group, tc.method, tc.target, got, ok, tc.want)
		}
	}
}

func TestBadGroupsAreRefused(t *testing.T) {
	for _, s := range []string{"", "products/:slug", "GET", "GET products", "/a/**/b", "/a?b=1", "G3T /a", "GET /a extra"} {
		if _, err := ParseGroup(s); err == nil {
			t.Errorf("ParseGroup(%q) succeeded, want an error", s)
		}
	}
}

func TestLabelEndpointsUsesTheFirstGroupThatMatchesAndShapesTheRest(t *testing.T) {
	tr := &Trace{}
	for _, target := range []string{"/products/blue-shoes", "/products/red-hat", "/products/123", "/products/blue-shoes/reviews", "/orders/7", "/img/a.png"} {
		tr.Requests = append(tr.Requests, runner.Request{Target: target})
	}
	err := tr.LabelEndpoints(20, mustGroup(t, "/products/*/reviews"), mustGroup(t, "/products/:slug"), mustGroup(t, "/products/**"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range tr.Requests {
		got = append(got, r.Label)
	}
	want := []string{
		"GET /products/:slug", "GET /products/:slug", "GET /products/:slug", // an id is still one segment
		"GET /products/*/reviews", // the first group that matches, not the most specific
		"GET /orders/:id",         // no group: named by shape
		"GET /img/a.png",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %v\nwant     %v", got, want)
	}
}
