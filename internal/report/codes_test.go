package report

import (
	"bytes"
	"strings"
	"testing"
)

func TestTheReportCountsCallsByTheirCode(t *testing.T) {
	res := sampleResult()
	res.Statuses = map[int]uint64{}
	res.Codes = map[string]uint64{"OK": 90, "NOT_FOUND": 6, "INTERNAL": 4}
	s := Summarize(res, 10)
	if s.Responses != 100 || s.OK != 90 || s.BadStatus != 10 || s.Codes["NOT_FOUND"] != 6 {
		t.Errorf("responses %d, ok %d, bad %d, codes %v; want 100, 90 OK, the 10 others bad", s.Responses, s.OK, s.BadStatus, s.Codes)
	}
	var text strings.Builder
	if err := s.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "codes      OK: 90   NOT_FOUND: 6   INTERNAL: 4\n") || strings.Contains(text.String(), "status     ") {
		t.Errorf("want a codes line, busiest first, and no HTTP status line:\n%s", text.String())
	}
	var js bytes.Buffer
	if err := s.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	back, err := ReadSummary(&js)
	if err != nil {
		t.Fatal(err)
	}
	if back.Codes["INTERNAL"] != 4 {
		t.Errorf("codes after a round trip: %v", back.Codes)
	}

	// An HTTP run saves no codes.
	js.Reset()
	if err := Summarize(sampleResult(), 10).WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(js.String(), `"codes"`) {
		t.Errorf("an HTTP run saves codes:\n%s", js.String())
	}
}
