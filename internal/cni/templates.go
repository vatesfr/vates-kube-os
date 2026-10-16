package cni

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

// The pod network manifests are embedded rather than built with fmt.Sprintf in
// Go string literals.
//
// Two reasons. They stay readable: a manifest can be reviewed, diffed and
// compared against upstream's own example as YAML, which is how the Cilium
// images and the flannel pod network were checked in the first place. And they
// stay correct: a template that references a field the data does not have is a
// parse error at startup, not a document with a blank in it.
//
//go:embed templates/*.tmpl
var templateFS embed.FS

// templates is parsed once, at package initialisation. The files are embedded,
// so a malformed template is a defect in the build rather than a condition that
// can arise at runtime -- which is why template.Must is appropriate here and
// would not be for anything read from disk.
var templates = template.Must(
	template.New("cni").ParseFS(templateFS, "templates/*.tmpl"))

// render executes an embedded template.
func render(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
