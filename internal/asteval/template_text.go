package asteval

import (
	"text/template"
	"text/template/parse"
)

// textTemplate wraps *text/template.Template.
type textTemplate struct {
	t *template.Template
}

func (s *textTemplate) New(name string) Template {
	return &textTemplate{t: s.t.New(name)}
}

func (s *textTemplate) Parse(text string) (Template, error) {
	t, err := s.t.Parse(text)
	if err != nil {
		return nil, err
	}
	return &textTemplate{t: t}, nil
}

func (s *textTemplate) Funcs(funcMap map[string]any) Template {
	return &textTemplate{t: s.t.Funcs(funcMap)}
}

func (s *textTemplate) Option(opt ...string) Template {
	return &textTemplate{t: s.t.Option(opt...)}
}

func (s *textTemplate) Delims(left, right string) Template {
	return &textTemplate{t: s.t.Delims(left, right)}
}

func (s *textTemplate) Lookup(name string) Template {
	t := s.t.Lookup(name)
	if t == nil {
		return nil
	}
	return &textTemplate{t: t}
}

func (s *textTemplate) Name() string {
	return s.t.Name()
}

func (s *textTemplate) AddParseTree(name string, tree *parse.Tree) (Template, error) {
	t, err := s.t.AddParseTree(name, tree)
	if err != nil {
		return nil, err
	}
	return &textTemplate{t: t}, nil
}

func (s *textTemplate) Tree() *parse.Tree {
	return s.t.Tree
}

func (s *textTemplate) FindTree(name string) (*parse.Tree, bool) {
	t := s.t.Lookup(name)
	if t == nil {
		return nil, false
	}
	return t.Tree, true
}
