package pacer

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseStages(t *testing.T) {
	cases := []struct {
		in   string
		want []Stage
	}{
		{"30s@100", []Stage{{30 * time.Second, 100, 100}}},
		{"1m@100->500", []Stage{{time.Minute, 100, 500}}},
		{"30s@100,30s@200,30s@400", []Stage{{30 * time.Second, 100, 100}, {30 * time.Second, 200, 200}, {30 * time.Second, 400, 400}}},
		{" 10s@50 , 1m@50->500 , 30s@500 ", []Stage{{10 * time.Second, 50, 50}, {time.Minute, 50, 500}, {30 * time.Second, 500, 500}}},
		{"500ms@2.5", []Stage{{500 * time.Millisecond, 2.5, 2.5}}},
		{"10s@0", []Stage{{10 * time.Second, 0, 0}}}, // a pause
		{"1m30s@1000->0", []Stage{{90 * time.Second, 1000, 0}}},
	}
	for _, c := range cases {
		got, err := ParseStages(c.in)
		if err != nil {
			t.Errorf("ParseStages(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseStages(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseStagesRejectsMalformedInputAndNamesTheStage(t *testing.T) {
	cases := map[string]string{ // input -> what the error should mention
		"":                       "no stages",
		"100":                    "DURATION@RATE",
		"30s":                    "DURATION@RATE",
		"@100":                   "duration",
		"30s@":                   "rate",
		"soon@100":               "duration",
		"30s@fast":               "rate",
		"30s@100->":              "rate",
		"30s@->100":              "rate",
		"30s@1->2->3":            "rate",
		"30s@100,":               "stage 2",
		"30s@100,,30s@200":       "stage 2",
		"30s@100,30s@x":          "stage 2",
		"-5s@100":                "duration",
		"0s@100":                 "duration",
		"30s@-5":                 "rate",
		"30s@100->NaN":           "rate",
		"30s@Inf":                "rate",
		"30s@100;30s@200":        "rate",
		"30s@100, 30s@200 extra": "rate",
	}
	for in, mention := range cases {
		_, err := ParseStages(in)
		if err == nil {
			t.Errorf("ParseStages(%q) succeeded, want an error", in)
			continue
		}
		if !strings.Contains(err.Error(), mention) {
			t.Errorf("ParseStages(%q) error %q should mention %q", in, err, mention)
		}
	}
}

func TestStageStringIsWhatParseStagesReads(t *testing.T) {
	stages := []Stage{
		{30 * time.Second, 100, 100},
		{time.Minute, 100, 500},
		{time.Hour, 2.5, 2.5},
		{90 * time.Second, 1000, 0},
		{500 * time.Millisecond, 0, 0},
	}
	want := []string{"30s@100", "1m@100->500", "1h@2.5", "1m30s@1000->0", "500ms@0"}
	var parts []string
	for i, st := range stages {
		if got := st.String(); got != want[i] {
			t.Errorf("Stage %v prints as %q, want %q", st, got, want[i])
		}
		parts = append(parts, st.String())
	}
	back, err := ParseStages(strings.Join(parts, ","))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, stages) {
		t.Errorf("round trip gave %v, want %v", back, stages)
	}
}

func TestParseArrivals(t *testing.T) {
	for in, want := range map[string]Arrivals{"even": Even, "poisson": Poisson, "Poisson": Poisson, " EVEN ": Even} {
		got, err := ParseArrivals(in)
		if err != nil || got != want {
			t.Errorf("ParseArrivals(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	if _, err := ParseArrivals("random"); err == nil || !strings.Contains(err.Error(), "even") || !strings.Contains(err.Error(), "poisson") {
		t.Errorf("an unknown process should list the valid ones, got %v", err)
	}
}
