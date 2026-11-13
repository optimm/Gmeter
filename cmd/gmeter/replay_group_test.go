package main

import (
	"strings"
	"testing"
)

const slugTrace = `{"at":0,"path":"/products/blue-shoes"}
{"at":0.02,"path":"/products/red-hat"}
{"at":0.04,"path":"/products/green-scarf?ref=home"}
{"at":0.06,"path":"/img/a.png"}
{"at":0.08,"path":"/img/b.png"}
{"at":0.10,"path":"/health"}
`

func TestReplayGroupsTheEndpointsTheFlagsName(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", slugTrace)

	_, _, without := savedEndpoints(t, srv.URL, "-file", trace)
	if len(without) != 6 {
		t.Fatalf("without -group: %v, want each slug and file its own endpoint", without)
	}

	_, _, scheduled := savedEndpoints(t, srv.URL, "-file", trace, "-group", "/products/:slug", "-group", "GET /img/**")
	want := map[string]uint64{"GET /products/:slug": 3, "GET /img/**": 2, "GET /health": 1}
	if len(scheduled) != len(want) {
		t.Errorf("endpoints = %v, want %v", scheduled, want)
	}
	for name, n := range want {
		if scheduled[name] != n {
			t.Errorf("%s: %d requests, want %d (all: %v)", name, scheduled[name], n, scheduled)
		}
	}
}

func TestBadGroupFlagsAreUsageErrors(t *testing.T) {
	srv, _ := traceServer(t)
	trace := writeTrace(t, "trace.jsonl", slugTrace)
	for _, args := range [][]string{
		{"-group", "products/:slug"},                     // not a path
		{"-group", "/a/**/b"},                            // ** in the middle
		{"-group", "/products/:slug", "-endpoints", "0"}, // no breakdown to group
	} {
		code, _, stderr := execReplay(t, append(append([]string{"-file", trace, "-quiet"}, args...), srv.URL)...)
		if code != 2 || !strings.Contains(stderr, "-group") {
			t.Errorf("%v: exit %d, stderr %q, want a usage error naming -group", args, code, stderr)
		}
	}
}
