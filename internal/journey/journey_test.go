package journey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

func writeScenario(t *testing.T, dir, body string, files map[string]string) string {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompileStrict(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml  string
		err   string
		files map[string]string
	}{
		"no name": {
			yaml: "steps:\n  - name: a\n    path: /a\n",
			err:  "no name",
		},
		"no steps": {
			yaml: "name: j\n",
			err:  "no steps",
		},
		"unknown key": {
			yaml: "name: j\nbogus: 1\nsteps:\n  - name: a\n    path: /a\n",
			err:  "field bogus not found",
		},
		"duplicate steps": {
			yaml: "name: j\nsteps:\n  - {name: a, path: /a}\n  - {name: a, path: /b}\n",
			err:  "two steps are named",
		},
		"absolute path": {
			yaml: "name: j\nsteps:\n  - {name: a, path: http://x/y}\n",
			err:  "the path is always relative",
		},
		"bad think": {
			yaml: "name: j\nsteps:\n  - {name: a, path: /a, think: nope}\n",
			err:  "think",
		},
		"bad status range": {
			yaml: "name: j\nsteps:\n  - name: a\n    path: /a\n    check:\n      status: [200..100]\n",
			err:  "not a range",
		},
		"bad template func": {
			yaml: "name: j\nsteps:\n  - {name: a, path: /a, body: '{{nosuchfunc}}'}\n",
			err:  `function "nosuchfunc" not defined`,
		},
		"feed with no column": {
			yaml:  "name: j\nfeeds:\n  f:\n    file: f.csv\n    select: round-robin\nsteps:\n  - {name: a, path: '/{{feeds.f.nope}}'}\n",
			err:   "has no column",
			files: map[string]string{"f.csv": "a,b\n1,2\n"},
		},
		"unknown feed": {
			yaml: "name: j\nsteps:\n  - {name: a, path: '/{{feeds.nope}}'}\n",
			err:  "has no feed named",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeScenario(t, dir, tc.yaml, tc.files)
			_, err := Compile(path)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("got error %v, want one containing %q", err, tc.err)
			}
		})
	}
}

func TestCompileChecksAndExtracts(t *testing.T) {
	dir := t.TempDir()
	path := writeScenario(t, dir, `name: checkout
steps:
  - name: sign-in
    method: POST
    path: /login
    body: '{"email":"{{vars.email}}"}'
    check:
      status: [200, 201]
      json:
        token: {exists: true}
    extract:
      token: {from: json, path: $.token}
  - name: cart
    path: /cart/{{vars.token}}
    check:
      header:
        Location: {matches: '^/cart/[0-9]+$'}
`, nil)
	j, err := Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "checkout" || len(j.Steps) != 2 {
		t.Fatalf("journey: %+v", j)
	}
	if j.Steps[0].Label != "sign-in" || j.Steps[1].Label != "cart" {
		t.Fatalf("labels: %q, %q", j.Steps[0].Label, j.Steps[1].Label)
	}
	vars := map[string]any{"session": uint64(0), "email": "a@b.c"}
	method, target, header, body, err := j.Steps[0].Render(vars)
	if err != nil {
		t.Fatal(err)
	}
	if method != "POST" || target != "/login" {
		t.Errorf("render: %s %s", method, target)
	}
	if string(body) != `{"email":"a@b.c"}` {
		t.Errorf("body: %s", body)
	}
	if err := j.Steps[0].Check(vars, 200, nil, []byte(`{"token":"xyz"}`)); err != nil {
		t.Errorf("check: %v", err)
	}
	if err := j.Steps[0].Extract(vars, nil, []byte(`{"token":"xyz"}`)); err != nil {
		t.Fatal(err)
	}
	if vars["token"] != "xyz" {
		t.Errorf("extract: vars = %v", vars)
	}
	_, target, _, _, err = j.Steps[1].Render(vars)
	if err != nil {
		t.Fatal(err)
	}
	if target != "/cart/xyz" {
		t.Errorf("step 2 target: %q", target)
	}
	if header != nil {
		t.Errorf("step 1 header: %v", header)
	}
}

func TestCompileExtracts(t *testing.T) {
	dir := t.TempDir()
	path := writeScenario(t, dir, `name: j
steps:
  - name: a
    path: /a
    extract:
      id: {from: json, path: '$[0].id'}
      ua: {from: header, path: User-Agent, optional: true}
      loc: {from: regex, path: '^/cart/(\d+)$', group: 1, header: Location}
`, nil)
	j, err := Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"User-Agent": []string{"gmeter/1.0"}, "Location": []string{"/cart/7"}}
	body := []byte(`[{"id": 42}]`)
	vars := map[string]any{}
	if err := j.Steps[0].Extract(vars, header, body); err != nil {
		t.Fatal(err)
	}
	if vars["id"] != "42" {
		t.Errorf("id: %v", vars["id"])
	}
	if vars["ua"] != "gmeter/1.0" {
		t.Errorf("ua: %v", vars["ua"])
	}
	if vars["loc"] != "7" {
		t.Errorf("loc: %v", vars["loc"])
	}
	// An optional extract that finds nothing is fine; a required one fails.
	vars2 := map[string]any{}
	if err := j.Steps[0].Extract(vars2, http.Header{}, nil); err == nil {
		t.Error("expected the required extract id to fail with no body")
	}
}

func TestCompileFeeds(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, `name: j
feeds:
  shoppers:
    file: shoppers.csv
    select: round-robin
steps:
  - name: a
    path: /{{feeds.shoppers.email}}
    body: '{"sku":"{{feeds.shoppers.sku}}"}'
`, map[string]string{
		"shoppers.csv": "email,sku\nbob@x,apple\nalice@x,pear\n",
	})
	j, err := Compile(filepath.Join(dir, "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, target0, _, body0, err := j.Steps[0].Render(map[string]any{"session": uint64(0)})
	if err != nil {
		t.Fatal(err)
	}
	_, target1, _, body1, err := j.Steps[0].Render(map[string]any{"session": uint64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if target0 != "/bob@x" || target1 != "/alice@x" {
		t.Errorf("targets: %q, %q", target0, target1)
	}
	if string(body0) != `{"sku":"apple"}` || string(body1) != `{"sku":"pear"}` {
		t.Errorf("bodies: %q, %q", body0, body1)
	}
}

func TestJourneyEndToEnd(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"token":"abc","id":7}`))
		default:
			w.WriteHeader(201)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := writeScenario(t, dir, `name: e2e
steps:
  - name: login
    path: /login
    body: '{"u":1}'
    extract:
      token: {from: json, path: $.token}
    check:
      status: [200]
  - name: cart
    path: /cart/{{vars.token}}
    think: 1ms
    check:
      status: 201
`, nil)
	j, err := Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.Run(context.Background(), runner.Config{
		URL: srv.URL, Journey: j, Rate: 20, Duration: 200 * time.Millisecond, Workers: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Journey == nil || res.Journey.SessionsCompleted == 0 {
		t.Fatalf("journey result: %+v", res.Journey)
	}
	for _, l := range res.Labels {
		if l.Scheduled == 0 {
			t.Errorf("step %s has no scheduled count", l.Label)
		}
	}
	saw := map[string]bool{}
	for _, h := range hits {
		saw[h] = true
	}
	if !saw["/login"] || !saw["/cart/abc"] {
		t.Errorf("server saw %v", hits)
	}
}

func TestJSONPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		root any
		want any
		ok   bool
	}{
		{"$", map[string]any{"a": 1.0}, map[string]any{"a": 1.0}, true},
		{"$.a", map[string]any{"a": "x"}, "x", true},
		{"$.a", map[string]any{}, nil, false},
		{"$.a.b", map[string]any{"a": map[string]any{"b": 2.0}}, 2.0, true},
		{"$[0]", []any{"a", "b"}, "a", true},
		{"$.list[1]", map[string]any{"list": []any{1.0, 2.0}}, 2.0, true},
		{"$.list[5]", map[string]any{"list": []any{1.0, 2.0}}, nil, false},
		{"$[0].id", []any{map[string]any{"id": 42.0}}, 42.0, true},
		{"$.x.y.z", map[string]any{"x": map[string]any{}}, nil, false},
		{"$.a", "scalar", nil, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			segs, err := parsePath(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := evalPath(tc.root, segs)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v (%T), want %v", got, got, tc.want)
			}
		})
	}
	for _, bad := range []string{"a.b", ".a", "$.", "$[a]", "$.a$"} {
		if _, err := parsePath(bad); err == nil {
			t.Errorf("parsePath(%q) succeeded, want an error", bad)
		}
	}
}

func TestCompileBadPaths(t *testing.T) {
	dir := t.TempDir()
	for _, y := range []string{
		"name: j\nsteps:\n  - {name: a, path: /a, check: {json: {'$.a b': {exists: true}}}}\n",
		"name: j\nsteps:\n  - {name: a, path: /a, extract: {x: {from: json, path: 'a b'}}}\n",
		"name: j\nsteps:\n  - {name: a, path: /a, extract: {x: {from: bogus, path: $.a}}}\n",
		"name: j\nsteps:\n  - {name: a, path: /a, extract: {x: {from: json}}}\n",
		"name: j\nsteps:\n  - {name: a, path: /a, extract: {x: {from: json, path: '$.a b'}}}\n",
		"name: j\nsteps:\n  - {name: a, path: /a, extract: {x: {from: regex, path: '('}}}\n",
		"name: j\nsteps:\n  - {name: a, path: /a, on_failure: stop}\n",
	} {
		path := writeScenario(t, dir, y, nil)
		if _, err := Compile(path); err == nil {
			t.Errorf("Compile(%q) succeeded, want an error", y)
		}
	}
}

func TestCompileUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := writeScenario(t, dir, "name: j\nsteps:\n  - name: a\n    path: /a\n    method: GET\n    think: 1s\n", nil)
	if _, err := Compile(path); err != nil {
		t.Fatal(err)
	}
	path = writeScenario(t, dir, "name: j\nsteps:\n  - name: a\n    path: /a\n    weird: 1\n", nil)
	if _, err := Compile(path); err == nil || !strings.Contains(err.Error(), "field weird not found") {
		t.Errorf("unknown field in a step: got %v", err)
	}
}

func TestFeedSelection(t *testing.T) {
	f := &Feed{How: "random", Rows: []map[string]string{{"a": "1"}, {"a": "2"}, {"a": "3"}}}
	seen := map[string]int{}
	for i := uint64(0); i < 200; i++ {
		seen[f.selected(i)["a"].(string)]++
	}
	if len(seen) < 2 {
		t.Errorf("random selection saw only %v", seen)
	}
}

func TestJSONStringify(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(`42`), &v); err != nil {
		t.Fatal(err)
	}
	if s, err := stringifyJSON(v); err != nil || s != "42" {
		t.Errorf("stringifyJSON(42) = %q, %v; want \"42\"", s, err)
	}
	if s := stringifyYAML(42); s != "42" {
		t.Errorf("stringifyYAML(42) = %q", s)
	}
	if s := stringifyYAML(true); s != "true" {
		t.Errorf("stringifyYAML(true) = %q", s)
	}
}
