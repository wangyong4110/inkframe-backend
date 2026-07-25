package service

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/flosch/pongo2/v4"
)

//go:embed prompts
var promptTemplates embed.FS

// embedFSLoader implements pongo2.TemplateLoader so that {% include %} tags
// can resolve paths relative to the including template inside the embedded FS.
type embedFSLoader struct {
	fs embed.FS
}

func (l *embedFSLoader) Abs(base, name string) string {
	if base == "" {
		// name is already the full path supplied to FromFile()
		return name
	}
	return path.Join(path.Dir(base), name)
}

func (l *embedFSLoader) Get(p string) (io.Reader, error) {
	data, err := l.fs.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

// templateSet is the global pongo2 TemplateSet backed by the embedded FS.
// Using a TemplateSet (instead of pongo2.FromString) enables {% include %} resolution
// and provides built-in template caching per path.
var templateSet *pongo2.TemplateSet

func init() {
	templateSet = pongo2.NewSet("inkframe", &embedFSLoader{fs: promptTemplates})
}

// renderPrompt renders a Jinja2 prompt template by name (without extension).
// ctx is a map[string]interface{} of template variables.
func renderPrompt(name string, ctx map[string]interface{}) (string, error) {
	tpl, err := templateSet.FromFile("prompts/" + name + ".j2")
	if err != nil {
		return "", fmt.Errorf("load template %s: %w", name, err)
	}
	if ctx == nil {
		ctx = map[string]interface{}{}
	}
	out, err := tpl.Execute(pongo2.Context(ctx))
	if err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	return out, nil
}

// LoadRawPrompt reads a prompt file by name (without extension) from the embedded FS.
// It returns the raw text without any template rendering.
func LoadRawPrompt(name string) (string, error) {
	data, err := promptTemplates.ReadFile("prompts/" + name + ".j2")
	if err != nil {
		return "", fmt.Errorf("load prompt %s: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}
