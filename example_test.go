package gmeter_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/optimm/Gmeter"
)

// Fail a test when a service misses its latency target at a given rate.
func ExampleResult_Meets() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	res, err := gmeter.Run(context.Background(), gmeter.Config{URL: srv.URL, Rate: 200, Duration: time.Second})
	if err != nil {
		fmt.Println(err)
		return
	}
	target := gmeter.SLO{Quantile: 0.99, Latency: 100 * time.Millisecond}
	if err := res.Meets(target); err != nil {
		fmt.Println("missed:", err)
		return
	}
	fmt.Println("p99 is within 100ms at 200 requests per second")
	// Output: p99 is within 100ms at 200 requests per second
}

// Print the same report the gmeter command prints, or save its JSON for
// `gmeter diff`.
func ExampleResult_WriteText() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	res, err := gmeter.Run(context.Background(), gmeter.Config{URL: srv.URL, Rate: 50, Duration: time.Second})
	if err != nil {
		fmt.Println(err)
		return
	}
	res.WriteText(os.Stderr)
}

// Ask how much load a server can take while it keeps its target.
func ExampleFindCapacity() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	got, err := gmeter.FindCapacity(context.Background(),
		gmeter.Config{URL: srv.URL},
		gmeter.SLO{Quantile: 0.99, Latency: 50 * time.Millisecond},
		gmeter.SearchOptions{Start: 50, Max: 200, Trial: 2 * time.Second})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("holds at least %.0f requests per second: %v\n", got.MaxRPS, got.LowerBound)
}
