package journey

import (
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// compiler turns a scenario into a runner.Journey.
type compiler struct {
	feeds    []*Feed
	feedByName map[string]*Feed
}

// validateFeedRefs checks that the templates of a step name only feed columns
// that exist, so a typo in the CSV is a usage error before any request is
// sent rather than a step that aborts every session.
var feedRefRe = regexp.MustCompile(`\{\{\s*\.feeds\.([A-Za-z0-9_-]+)\s*((?:\.[A-Za-z0-9_.-]*)?)\s*\}\}`)

func (c *compiler) validateFeedRefs(tmpl *template.Template, stepName string) error {
	var text string
	if tmpl.Tree != nil && tmpl.Tree.Root != nil {
		text = tmpl.Tree.Root.String()
	}
	for _, m := range feedRefRe.FindAllStringSubmatch(text, -1) {
		f, ok := c.feedByName[m[1]]
		if !ok {
			return fmt.Errorf("step %q: feeds has no feed named %q", stepName, m[1])
		}
		if col := strings.TrimPrefix(m[2], "."); col != "" && !f.hasColumn(strings.SplitN(col, ".", 2)[0]) {
			return fmt.Errorf("step %q: feed %q has no column %q", stepName, m[1], strings.SplitN(col, ".", 2)[0])
		}
	}
	return nil
}

// step compiles one scenario step into a runner.JourneyStep. The closures are
// the compiled step: Render builds the request of a session before anything
// is sent, Check judges its response, Extract reads session variables from
// it.
func (c *compiler) step(s *Step) (runner.JourneyStep, error) {
	method := strings.ToUpper(s.Method)
	if method == "" {
		method = http.MethodGet
	}
	if _, err := http.NewRequest(method, "http://journey.invalid", nil); err != nil {
		return runner.JourneyStep{}, fmt.Errorf("step %q: invalid method: %w", s.Name, err)
	}
	if strings.Contains(s.Path, "://") {
		return runner.JourneyStep{}, fmt.Errorf("step %q: the path is always relative to the base URL: %q is not", s.Name, s.Path)
	}
	pathTmpl, err := newTemplate(s.Path)
	if err != nil {
		return runner.JourneyStep{}, fmt.Errorf("step %q: path: %w", s.Name, err)
	}
	for name := range s.Headers {
		if strings.Contains(name, "{{") {
			return runner.JourneyStep{}, fmt.Errorf("step %q: header names cannot be templated: %q", s.Name, name)
		}
	}
	headerTmpls := make(map[string]*template.Template, len(s.Headers))
	for _, name := range slices.Sorted(maps.Keys(s.Headers)) {
		tmpl, err := newTemplate(s.Headers[name])
		if err != nil {
			return runner.JourneyStep{}, fmt.Errorf("step %q: header %s: %w", s.Name, name, err)
		}
		headerTmpls[name] = tmpl
	}
	for _, t := range []*template.Template{pathTmpl} {
		if err := c.validateFeedRefs(t, s.Name); err != nil {
			return runner.JourneyStep{}, err
		}
	}
	for t := range maps.Values(headerTmpls) {
		if err := c.validateFeedRefs(t, s.Name); err != nil {
			return runner.JourneyStep{}, err
		}
	}
	bodyTmpl, err := newTemplate(s.Body)
	if err != nil {
		return runner.JourneyStep{}, fmt.Errorf("step %q: body: %w", s.Name, err)
	}
	if err := c.validateFeedRefs(bodyTmpl, s.Name); err != nil {
		return runner.JourneyStep{}, err
	}
	think, err := parseThink(s.Think)
	if err != nil {
		return runner.JourneyStep{}, fmt.Errorf("step %q: %w", s.Name, err)
	}
	chk, err := newChecker(s.Check)
	if err != nil {
		return runner.JourneyStep{}, fmt.Errorf("step %q: %w", s.Name, err)
	}
	var exts []*extract
	for _, name := range slices.Sorted(maps.Keys(s.Extract)) {
		e, err := compileExtract(name, s.Extract[name])
		if err != nil {
			return runner.JourneyStep{}, fmt.Errorf("step %q: %w", s.Name, err)
		}
		exts = append(exts, e)
	}
	switch s.OnFailure {
	case "", "continue":
	default:
		return runner.JourneyStep{}, fmt.Errorf("step %q: on_failure must be empty or \"continue\", got %q", s.Name, s.OnFailure)
	}
	continueOnFailure := s.OnFailure == "continue"

	render := func(vars map[string]any) (string, string, http.Header, []byte, error) {
		data := templateData(vars, c.feeds)
		target, err := renderText(pathTmpl, data)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("rendering the path: %w", err)
		}
		if !strings.HasPrefix(target, "/") {
			return "", "", nil, nil, fmt.Errorf("the path must be relative to the base URL and start with \"/\", got %q", target)
		}
		if len(target) > StepRequestLimit {
			return "", "", nil, nil, fmt.Errorf("the path renders to %d bytes, over the %d limit", len(target), StepRequestLimit)
		}
		var header http.Header
		for _, name := range slices.Sorted(maps.Keys(headerTmpls)) {
			v, err := renderText(headerTmpls[name], data)
			if err != nil {
				return "", "", nil, nil, fmt.Errorf("rendering header %s: %w", name, err)
			}
			if len(v) > StepRequestLimit {
				return "", "", nil, nil, fmt.Errorf("header %s renders to %d bytes, over the %d limit", name, len(v), StepRequestLimit)
			}
			if header == nil {
				header = http.Header{}
			}
			header[name] = append(header[name], v)
		}
		var body []byte
		if s.Body != "" {
			v, err := renderText(bodyTmpl, data)
			if err != nil {
				return "", "", nil, nil, fmt.Errorf("rendering the body: %w", err)
			}
			if len(v) > StepRequestLimit {
				return "", "", nil, nil, fmt.Errorf("the body renders to %d bytes, over the %d limit", len(v), StepRequestLimit)
			}
			body = []byte(v)
		}
		return method, target, header, body, nil
	}

	var checkFn func(map[string]any, int, http.Header, []byte) error
	if chk != nil {
		checkFn = func(_ map[string]any, status int, header http.Header, body []byte) error {
			if err := chk.check(status, header, body); err != nil {
				return fmt.Errorf("check %q: %w", s.Name, err)
			}
			return nil
		}
	}
	var extractFn func(map[string]any, http.Header, []byte) error
	if len(exts) > 0 {
		extractFn = func(vars map[string]any, header http.Header, body []byte) error {
			for _, e := range exts {
				if err := e.apply(vars, header, body); err != nil {
					return fmt.Errorf("extract %q: %w", s.Name, err)
				}
			}
			return nil
		}
	}
	return runner.JourneyStep{
		Label:                  s.Name,
		Method:                 method,
		Think:                  think,
		ContinueOnCheckFailure: continueOnFailure,
		Render:                 render,
		Check:                  checkFn,
		Extract:                extractFn,
	}, nil
}

// parseThink reads a step's think time, where a session's step is due when the
// previous step finished plus this.
func parseThink(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d < 0 {
		return 0, fmt.Errorf("think must be a non-negative duration such as 200ms, got %q", s)
	}
	return d, nil
}
