// Package results saves every request of a run to a CSV file, and reads such a
// file back. It is what `gmeter -results FILE` writes and what `gmeter plot`
// draws from, and the file opens in a spreadsheet, pandas or awk as well.
//
// One row is one request that was sent. The times are microseconds from the start
// of the run; corrected_us is the latency from when the request was due, and
// service_us from when it was sent, the two clocks the report shows.
package results

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

var header = []string{"seq", "stage", "label", "due_us", "sent_us", "finished_us", "corrected_us", "service_us", "status", "error"}

// buffered is how many samples may wait for the disk before Add blocks.
const buffered = 1 << 14

// Writer writes samples to a file from its own goroutine, so that the workers
// calling Add do not wait for the disk. Add is safe for concurrent use.
type Writer struct {
	in   chan runner.Sample
	done chan struct{}
	err  error // the first write error; set before done is closed
}

// NewWriter starts writing to w, header first. Close it to flush.
func NewWriter(w io.Writer) *Writer {
	rw := &Writer{in: make(chan runner.Sample, buffered), done: make(chan struct{})}
	go rw.run(w)
	return rw
}

// Add queues s. If the disk cannot keep up and the queue is full it waits, which
// delays the calling worker and shows up as latency; if writing has failed it
// discards s at once, and Close reports the error.
func (w *Writer) Add(s runner.Sample) { w.in <- s }

// Close writes what is queued and returns the first error writing had.
func (w *Writer) Close() error {
	close(w.in)
	<-w.done
	return w.err
}

func (w *Writer) run(out io.Writer) {
	defer close(w.done)
	buf := bufio.NewWriterSize(out, 1<<20)
	cw := csv.NewWriter(buf)
	failed := func(err error) {
		if err != nil && w.err == nil {
			w.err = err
		}
	}
	failed(cw.Write(header))
	for s := range w.in { // keeps draining after an error, so no caller blocks
		if w.err != nil {
			continue
		}
		status := ""
		switch {
		case s.Status != 0:
			status = strconv.Itoa(s.Status)
		case s.Code != "":
			status = s.Code
		}
		failed(cw.Write([]string{
			strconv.FormatUint(s.Seq, 10),
			strconv.Itoa(s.Stage),
			s.Label,
			us(s.Due), us(s.Sent), us(s.Finished),
			us(s.Finished - s.Due), us(s.Finished - s.Sent),
			status,
			s.Failure,
		}))
	}
	cw.Flush()
	failed(cw.Error())
	failed(buf.Flush())
}

func us(d time.Duration) string { return strconv.FormatInt(d.Microseconds(), 10) }

// Read calls fn with each row of a results file, in file order. Times come back
// in whole microseconds, as they were written.
func Read(r io.Reader, fn func(runner.Sample)) error {
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.FieldsPerRecord = -1 // until the header says this is a results file
	cr.ReuseRecord = true
	first, err := cr.Read()
	switch {
	case errors.Is(err, io.EOF):
		return errors.New("the file is empty: want a gmeter results file (written with -results)")
	case err != nil:
		return err
	}
	if strings.Join(first, ",") != strings.Join(header, ",") {
		return fmt.Errorf("not a gmeter results file: the first line should be %q", strings.Join(header, ","))
	}
	cr.FieldsPerRecord = len(header)
	for {
		row, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		line, _ := cr.FieldPos(0)
		s := runner.Sample{Label: row[2], Failure: row[9]}
		var errs [5]error
		s.Seq, errs[0] = strconv.ParseUint(row[0], 10, 64)
		var stage int64
		stage, errs[1] = strconv.ParseInt(row[1], 10, 32)
		s.Stage = int(stage)
		s.Due, errs[2] = micros(row[3])
		s.Sent, errs[3] = micros(row[4])
		s.Finished, errs[4] = micros(row[5])
		if err := errors.Join(errs[:]...); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		if n, err := strconv.Atoi(row[8]); err == nil {
			s.Status = n
		} else {
			s.Code = row[8]
		}
		fn(s)
	}
}

func micros(field string) (time.Duration, error) {
	n, err := strconv.ParseInt(field, 10, 64)
	return time.Duration(n) * time.Microsecond, err
}
