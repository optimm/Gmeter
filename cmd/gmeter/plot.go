package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/optimm/Gmeter/internal/plot"
)

// runPlot implements `gmeter plot [flags] RESULTS.csv`: draw a file written with
// -results as one self-contained HTML page. Exit code: 0 written, 2 bad usage, a
// file it cannot read, or a page it cannot write.
func runPlot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gmeter plot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		out     = fs.String("o", "", "where to write the page ('-' for stdout); default the results file with .html in place of its extension")
		title   = fs.String("title", "", "the page's title (default the results file's name)")
		buckets = fs.Int("buckets", 100, "slices to cut the run into; each is one point on the charts")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: gmeter plot [flags] RESULTS.csv\n\n"+
			"Draws a file written with -results as one HTML page, with no outside dependencies: latency\n"+
			"over time by both clocks, load over time, and the latency distribution. Flags must come\n"+
			"before the file.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "gmeter plot: "+format+"\n", a...)
		return 2
	}
	switch {
	case fs.NArg() == 0:
		fmt.Fprintln(stderr, "gmeter plot: missing the results file, written with -results")
		fs.Usage()
		return 2
	case fs.NArg() > 1:
		return usage("expected exactly one results file but got %d arguments (flags must come before the file)", fs.NArg())
	case *buckets < 1:
		return usage("-buckets must be at least 1, got %d", *buckets)
	}
	path := fs.Arg(0)
	f, err := os.Open(path)
	if err != nil {
		return usage("%v", err)
	}
	defer f.Close()
	data, err := plot.Summarize(f, *buckets)
	if err != nil {
		return usage("%s: %v", path, err)
	}
	data.Title = *title
	if data.Title == "" {
		data.Title = filepath.Base(path)
	}

	if *out == "-" {
		if err := plot.WriteHTML(stdout, data); err != nil {
			fmt.Fprintf(stderr, "gmeter plot: %v\n", err)
			return 1
		}
		return 0
	}
	if *out == "" {
		*out = strings.TrimSuffix(path, filepath.Ext(path)) + ".html"
	}
	page, err := os.Create(*out)
	if err != nil {
		return usage("%v", err)
	}
	if err := errors.Join(plot.WriteHTML(page, data), page.Close()); err != nil {
		fmt.Fprintf(stderr, "gmeter plot: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "gmeter plot: wrote %s\n", *out)
	return 0
}
