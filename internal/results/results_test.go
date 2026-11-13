package results

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

func TestWhatIsWrittenReadsBackAsTheSameRows(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	in := []runner.Sample{
		{Seq: 0, Stage: 0, Label: "GET /a", Due: 0, Sent: 10 * time.Microsecond, Finished: 1510 * time.Microsecond, Status: 200},
		{Seq: 1, Stage: 1, Label: `POST /a,"b"`, Due: 5 * time.Millisecond, Sent: 5*time.Millisecond + 2*time.Microsecond, Finished: 9 * time.Millisecond, Code: "NotFound"},
		{Seq: 2, Due: 10 * time.Millisecond, Sent: 10 * time.Millisecond, Finished: 2 * time.Second, Failure: "timeout"},
	}
	for _, s := range in {
		w.Add(s)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var got []runner.Sample
	err := Read(&buf, func(s runner.Sample) { got = append(got, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in) {
		t.Fatalf("read %d rows, wrote %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("row %d: read %+v, wrote %+v", i, got[i], in[i])
		}
	}
}

func TestTheFileSaysTheLatencyFromBothClocks(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Add(runner.Sample{Seq: 7, Due: time.Millisecond, Sent: 4 * time.Millisecond, Finished: 10 * time.Millisecond, Status: 503})
	w.Close()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{
		"seq,stage,label,due_us,sent_us,finished_us,corrected_us,service_us,status,error",
		"7,0,,1000,4000,10000,9000,6000,503,",
	}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestManyGoroutinesCanAddAtOnce(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				w.Add(runner.Sample{Seq: uint64(g*500 + i), Status: 200})
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	seen := map[uint64]bool{}
	if err := Read(&buf, func(s runner.Sample) { seen[s.Seq] = true }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 4000 {
		t.Errorf("%d distinct rows, want 4000", len(seen))
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestAFailingDiskIsReportedByCloseAndNeverBlocksTheCaller(t *testing.T) {
	w := NewWriter(failingWriter{})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200000; i++ { // far more than any buffer holds
			w.Add(runner.Sample{Seq: uint64(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Add blocked on a writer that fails")
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Close = %v, want the disk full error", err)
	}
}

func TestReadRefusesAFileThatIsNotAResultsFile(t *testing.T) {
	for name, in := range map[string]string{
		"empty":        "",
		"wrong header": "a,b,c\n1,2,3\n",
		"bad number":   "seq,stage,label,due_us,sent_us,finished_us,corrected_us,service_us,status,error\nx,0,,1,2,3,2,1,200,\n",
		"short row":    "seq,stage,label,due_us,sent_us,finished_us,corrected_us,service_us,status,error\n1,0\n",
	} {
		if err := Read(strings.NewReader(in), func(runner.Sample) {}); err == nil {
			t.Errorf("%s: Read accepted it", name)
		}
	}
}
