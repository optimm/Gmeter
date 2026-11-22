package journey

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"text/template"
)

// The restricted template language of a scenario: text/template with only the
// two safe encoding helpers journey documents, and missing values an error, so
// a template that names a variable the session does not have fails the step
// rather than printing "<no value>".
var journeyFuncs = template.FuncMap{
	"urlquery": url.QueryEscape,
	"json": func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("<error: %v>", err)
		}
		return string(b)
	},
}

// designDot turns {{vars.x}} into {{.vars.x}}: text/template reads a bare
// identifier as a function name, and the scenario language is data-driven.
var designDot = regexp.MustCompile(`\{\{\s*(vars|feeds|session)`)

func dotify(text string) string { return designDot.ReplaceAllString(text, "{{.$1") }

// newTemplate parses one template of a step at compile time, so an unknown
// function or a bad action fails the scenario before any request is sent.
func newTemplate(text string) (*template.Template, error) {
	t, err := template.New("step").Funcs(journeyFuncs).Parse(dotify(text))
	if err != nil {
		return nil, err
	}
	return t.Option("missingkey=error"), nil
}

// renderText runs t with data, the map of variables, feeds and the session
// number.
func renderText(t *template.Template, data map[string]any) (string, error) {
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// templateData is what step templates render from: the session's variables
// (the extracts of earlier steps), every feed's selected row, and the session
// number.
func templateData(vars map[string]any, feeds []*Feed) map[string]any {
	session := sessionOf(vars)
	out := map[string]any{"vars": vars, "session": session}
	if len(feeds) > 0 {
		rows := make(map[string]any, len(feeds))
		for _, f := range feeds {
			rows[f.Name] = f.selected(session)
		}
		out["feeds"] = rows
	}
	return out
}

func sessionOf(vars map[string]any) uint64 {
	if s, ok := vars["session"].(uint64); ok {
		return s
	}
	if s, ok := vars["session"].(int); ok {
		return uint64(s)
	}
	return 0
}
