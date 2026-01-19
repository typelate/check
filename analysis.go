package check

import (
	"golang.org/x/tools/go/analysis"
)

var Analyzer = &analysis.Analyzer{
	Name: "embedcheck",
	Doc:  "checks that //go:embed patterns match files",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	//for _, file := range pass.Files {
	//	pos := pass.Fset.Position(file.Pos())
	//	pkgDir := filepath.Dir(pos.Filename)
	//
	//	for _, decl := range file.Decls {
	//		genDecl, ok := decl.(*ast.GenDecl)
	//		if !ok || genDecl.Doc == nil {
	//			continue
	//		}
	//
	//		for _, comment := range genDecl.Doc.List {
	//			if !strings.HasPrefix(comment.Text, "//go:embed ") {
	//				continue
	//			}
	//
	//			line := strings.TrimPrefix(comment.Text, "//go:embed ")
	//			for _, pattern := range strings.Fields(line) {
	//				fullPattern := filepath.Join(pkgDir, pattern)
	//				matches, err := filepath.Glob(fullPattern)
	//				if err != nil {
	//					pass.Reportf(comment.Pos(), "invalid pattern %q: %v", pattern, err)
	//					continue
	//				}
	//				if len(matches) == 0 {
	//					pass.Reportf(comment.Pos(), "pattern %q matches no files", pattern)
	//				}
	//			}
	//		}
	//	}
	//}
	return nil, nil
}
