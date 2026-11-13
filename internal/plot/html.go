package plot

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"io"
)

//go:embed page.html
var page string

var pageTemplate = template.Must(template.New("page").Parse(page))

// WriteHTML writes the page for d. The data goes in as JSON, which escapes
// what could close the script early, and the title is escaped by the template.
func WriteHTML(w io.Writer, d *Data) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return pageTemplate.Execute(w, struct {
		Title string
		Data  template.JS
	}{d.Title, template.JS(raw)})
}
