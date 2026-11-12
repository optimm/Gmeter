package replay

import (
	"strings"
	"testing"
)

func TestParseCLFKeepsTheStatusEachLineRecorded(t *testing.T) {
	tr := mustCLF(t,
		`127.0.0.1 - - [10/Oct/2023:13:55:36 -0700] "GET /a HTTP/1.0" 200 2326`,
		`203.0.113.9 - bob [10/Oct/2023:13:55:37 -0700] "GET /b HTTP/1.1" 404 17 "https://example.com/" "Mozilla/5.0"`,
		`127.0.0.1 - - [10/Oct/2023:13:55:38 -0700] "GET /c HTTP/1.1" 503 -`,
	)

	var got []int
	for _, r := range tr.Requests {
		got = append(got, r.Logged)
	}
	if want := []int{200, 404, 503}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("logged statuses = %v, want %v", got, want)
	}
}

// A line whose status is missing or not a status is still a request to replay;
// it just has nothing to compare the response with.
func TestParseCLFStillReplaysALineWithoutAUsableStatus(t *testing.T) {
	tr := mustCLF(t,
		`127.0.0.1 - - [10/Oct/2023:13:55:36 -0700] "GET /dash HTTP/1.0" - -`,
		`127.0.0.1 - - [10/Oct/2023:13:55:37 -0700] "GET /nine HTTP/1.0" 999 5`,
		`127.0.0.1 - - [10/Oct/2023:13:55:38 -0700] "GET /low HTTP/1.0" 99 5`,
		`127.0.0.1 - - [10/Oct/2023:13:55:39 -0700] "GET /word HTTP/1.0" ok 5`,
		`127.0.0.1 - - [10/Oct/2023:13:55:40 -0700] "GET /none HTTP/1.0"`,
	)

	if len(tr.Requests) != 5 || tr.Unparsed != 0 {
		t.Fatalf("%d requests, %d unparsed, want all 5 kept", len(tr.Requests), tr.Unparsed)
	}
	for _, r := range tr.Requests {
		if r.Logged != 0 {
			t.Errorf("%s: Logged = %d, want 0 (nothing usable to compare with)", r.Target, r.Logged)
		}
	}
}

func TestParseJSONLKeepsAnOptionalStatus(t *testing.T) {
	tr, err := ParseJSONL(strings.NewReader(`{"path":"/a","status":200}
{"path":"/b","status":404}
{"path":"/c"}
`))
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, r := range tr.Requests {
		got = append(got, r.Logged)
	}
	if len(got) != 3 || got[0] != 200 || got[1] != 404 || got[2] != 0 {
		t.Errorf("logged statuses = %v, want [200 404 0]", got)
	}
}

func TestParseJSONLRefusesAStatusThatIsNotOne(t *testing.T) {
	for status, want := range map[string]string{
		"99":    `"status" must be an HTTP status code from 100 to 599, got 99`,
		"600":   `"status" must be an HTTP status code from 100 to 599, got 600`,
		"0":     `"status" must be an HTTP status code from 100 to 599, got 0`,
		"-1":    `"status" must be an HTTP status code from 100 to 599, got -1`,
		`"200"`: "cannot unmarshal string",
		"20.5":  "cannot unmarshal number 20.5",
	} {
		_, err := ParseJSONL(strings.NewReader(`{"path":"/a","status":` + status + "}\n"))
		if err == nil {
			t.Errorf("status %s accepted, want an error", status)
			continue
		}
		if !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), want) {
			t.Errorf("status %s: error %q, want it to name line 1 and say %q", status, err, want)
		}
	}
}
