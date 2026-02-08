package asteval

import (
	"html/template"
	"text/template/parse"
)

// htmlTemplate wraps *html/template.Template.
type htmlTemplate struct {
	t *template.Template
}

func (h *htmlTemplate) New(name string) Template {
	return &htmlTemplate{t: h.t.New(name)}
}

func (h *htmlTemplate) Parse(text string) (Template, error) {
	t, err := h.t.Parse(text)
	if err != nil {
		return nil, err
	}
	return &htmlTemplate{t: t}, nil
}

func (h *htmlTemplate) Funcs(funcMap map[string]any) Template {
	return &htmlTemplate{t: h.t.Funcs(funcMap)}
}

func (h *htmlTemplate) Option(opt ...string) Template {
	return &htmlTemplate{t: h.t.Option(opt...)}
}

func (h *htmlTemplate) Delims(left, right string) Template {
	return &htmlTemplate{t: h.t.Delims(left, right)}
}

func (h *htmlTemplate) Lookup(name string) Template {
	t := h.t.Lookup(name)
	if t == nil {
		return nil
	}
	return &htmlTemplate{t: t}
}

func (h *htmlTemplate) Name() string {
	return h.t.Name()
}

func (h *htmlTemplate) AddParseTree(name string, tree *parse.Tree) (Template, error) {
	t, err := h.t.AddParseTree(name, tree)
	if err != nil {
		return nil, err
	}
	return &htmlTemplate{t: t}, nil
}

func (h *htmlTemplate) Tree() *parse.Tree {
	return h.t.Tree
}

func (h *htmlTemplate) FindTree(name string) (*parse.Tree, bool) {
	t := h.t.Lookup(name)
	if t == nil {
		return nil, false
	}
	return t.Tree, true
}
