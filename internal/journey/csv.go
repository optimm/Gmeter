package journey

import (
	"encoding/csv"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// Feed is one feed: a CSV file of rows, one of which a session uses. Values
// are strings; a column the row does not have reads as "".
type Feed struct {
	Name         string // the feed's name in the scenario
	ScenarioPath string // the scenario file, so the CSV path resolves relative to it
	File         string // the CSV file, as the scenario writes it
	How          string `yaml:"select"` // "round-robin" or "random"
	Seed         uint64                  // the scenario's seed, for random selection
	Rows         []map[string]string
}

// load reads the feed's CSV file. The first row is the header; a file with no
// data rows is an error, because a session then has nothing to render from.
func (f *Feed) load(name string) error {
	path := f.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(f.ScenarioPath), path)
	}
	switch f.How {
	case "":
		f.How = "round-robin"
	case "round-robin", "random":
	default:
		return fmt.Errorf("feed %q: select must be round-robin or random, got %q", name, f.How)
	}
	fh, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("feed %q: %w", name, err)
	}
	defer fh.Close()
	recs, err := csv.NewReader(fh).ReadAll()
	if err != nil {
		return fmt.Errorf("feed %q: %w", name, err)
	}
	if len(recs) == 0 {
		return fmt.Errorf("feed %q: %s has no rows", name, f.File)
	}
	header := recs[0]
	for _, rec := range recs[1:] {
		row := map[string]string{}
		for j, col := range header {
			if col == "" {
				continue
			}
			if j < len(rec) {
				row[col] = rec[j]
			}
		}
		f.Rows = append(f.Rows, row)
	}
	if len(f.Rows) == 0 {
		return fmt.Errorf("feed %q: %s has no data rows", name, f.File)
	}
	return nil
}

// selected returns the row of the session with the given number: round-robin
// by session number, or random per session seeded by the scenario's seed and
// the session number.
func (f *Feed) selected(session uint64) map[string]any {
	if len(f.Rows) == 0 {
		return map[string]any{}
	}
	var row map[string]string
	if f.How == "random" {
		rng := rand.New(rand.NewPCG(f.Seed+session+1, f.Seed+session+0x9e3779b97f4a7c15))
		row = f.Rows[rng.IntN(len(f.Rows))]
	} else {
		row = f.Rows[int(session)%len(f.Rows)]
	}
	out := make(map[string]any, len(row))
	for k, v := range row {
		out[k] = v
	}
	return out
}

// hasColumn says whether any row has the named column.
func (f *Feed) hasColumn(col string) bool {
	for _, row := range f.Rows {
		if _, ok := row[col]; ok {
			return true
		}
	}
	return false
}
