// Package journey compiles scenario files into runner.Journey values: the
// stateful session format of `gmeter journey`. See docs/journeys.md for the
// schema.
//
// A scenario describes one session: a sequence of steps the journey package
// compiles into a runner.JourneyStep each, with the rendering, checks and
// extraction of the step pre-compiled. Rendering uses a restricted template
// language (see template.go), and the journey's feeds are the rows of CSV
// files (see csv.go).
package journey

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/optimm/Gmeter/internal/runner"
)

// Scenario is the strict YAML schema of a scenario file.
type Scenario struct {
	Name  string            `yaml:"name"`
	Seed  uint64            `yaml:"seed,omitempty"`
	Feeds map[string]*Feed  `yaml:"feeds,omitempty"`
	Steps []*Step           `yaml:"steps"`
}

// Step is one step of a session.
type Step struct {
	Name      string            `yaml:"name"`
	Method    string            `yaml:"method,omitempty"`
	Path      string            `yaml:"path"`
	Think     string            `yaml:"think,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	Body      string            `yaml:"body,omitempty"`
	Check     *CheckSpec        `yaml:"check,omitempty"`
	Extract   map[string]*ExtractSpec `yaml:"extract,omitempty"`
	OnFailure string            `yaml:"on_failure,omitempty"` // "" or "continue"
}

// CheckSpec is the compiled checks of one step: the response must satisfy
// them, or the session fails at the step.
type CheckSpec struct {
	Status  any               `yaml:"status,omitempty"` // a status, a range, or a list of either
	Header  map[string]OpSpec `yaml:"header,omitempty"` // header name -> op
	Body    *OpSpec           `yaml:"body,omitempty"`   // the response body
	JSON    map[string]OpSpec `yaml:"json,omitempty"`   // JSON path -> op
}

// OpSpec is one check or check-op: exists, equals or matches.
type OpSpec struct {
	Exists  *bool  `yaml:"exists,omitempty"`
	Equals  any    `yaml:"equals,omitempty"`
	Matches string `yaml:"matches,omitempty"`
}

// ExtractSpec is one extracted value: session var name -> how to read it.
type ExtractSpec struct {
	From     string `yaml:"from"`              // "json", "header" or "regex" (default "json")
	Path     string `yaml:"path"`              // JSON path, header name or regex text
	Group    int    `yaml:"group,omitempty"`   // regex group (regex only)
	Header   string `yaml:"header,omitempty"`  // regex: the header to match instead of the body
	Optional bool   `yaml:"optional,omitempty"`
}

// Compile reads the scenario file at path and returns it as a runner.Journey.
// Anything wrong with the file — unknown keys, bad durations, invalid
// templates, bad checks or a missing CSV column — is an error here, before
// any request is sent.
func Compile(path string) (*runner.Journey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("journey: %w", err)
	}
	defer f.Close()
	sc, err := decodeScenario(f)
	if err != nil {
		return nil, fmt.Errorf("journey %s: %w", filepath.Base(path), err)
	}
	if sc.Name == "" {
		return nil, fmt.Errorf("journey %s: the scenario has no name: give it one with name:", filepath.Base(path))
	}
	if len(sc.Steps) == 0 {
		return nil, fmt.Errorf("journey %s: the scenario has no steps", sc.Name)
	}
	seen := map[string]bool{}
	for i, s := range sc.Steps {
		if s.Name == "" {
			return nil, fmt.Errorf("journey %s: step %d has no name: name every step", sc.Name, i+1)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("journey %s: two steps are named %q: step names must be unique", sc.Name, s.Name)
		}
		seen[s.Name] = true
	}
	anyRandom := false
	for _, fd := range sc.Feeds {
		if fd.How == "random" {
			anyRandom = true
		}
	}
	if sc.Seed != 0 && !anyRandom {
		return nil, fmt.Errorf("journey %s: the scenario seed is for select: random feeds", sc.Name)
	}
	feeds := make([]*Feed, 0, len(sc.Feeds))
	for name, fd := range sc.Feeds {
		if fd.Name == "" {
			fd.Name = name
		}
		fd.ScenarioPath, fd.Seed = path, sc.Seed
		if err := fd.load(name); err != nil {
			return nil, err
		}
		feeds = append(feeds, fd)
	}
	c := &compiler{feeds: feeds, feedByName: make(map[string]*Feed, len(feeds))}
	for _, f := range feeds {
		c.feedByName[f.Name] = f
	}
	j := &runner.Journey{Name: sc.Name}
	for _, s := range sc.Steps {
		st, err := c.step(s)
		if err != nil {
			return nil, fmt.Errorf("journey %s: step %s: %w", sc.Name, s.Name, err)
		}
		j.Steps = append(j.Steps, st)
	}
	return j, nil
}

// decodeScenario reads the YAML and refuses unknown fields, so a scenario that
// spells a field wrong fails here rather than being silently ignored.
func decodeScenario(f *os.File) (*Scenario, error) {
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var sc Scenario
	if err := dec.Decode(&sc); err != nil {
		return nil, err
	}
	return &sc, nil
}
