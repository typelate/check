package asteval

import (
	"go/token"
	"go/types"
	"testing"
)

func TestDefaultFunctions(t *testing.T) {
	t.Run("fmt loaded with an incomplete scope", func(t *testing.T) {
		// A transitively imported package can be loaded shallowly by
		// go/packages, leaving its scope empty. DefaultFunctions must not
		// panic looking up fmt functions that are not there.
		app := types.NewPackage("example.com/app", "app")
		fmtPkg := types.NewPackage("fmt", "fmt")
		app.SetImports([]*types.Package{fmtPkg})

		fns := DefaultFunctions(app)
		if len(fns) != 0 {
			t.Errorf("DefaultFunctions(app) = %v, want no functions from an empty fmt scope", fns)
		}
	})

	t.Run("fmt not imported", func(t *testing.T) {
		fns := DefaultFunctions(types.NewPackage("example.com/app", "app"))
		if len(fns) != 0 {
			t.Errorf("DefaultFunctions(app) = %v, want no functions without fmt", fns)
		}
	})

	t.Run("fmt with a partial scope", func(t *testing.T) {
		// Only Sprintf is present; the other lookups must be skipped.
		app := types.NewPackage("example.com/app", "app")
		fmtPkg := types.NewPackage("fmt", "fmt")
		stringType := types.Universe.Lookup("string").Type()
		anySlice := types.NewSlice(types.NewInterfaceType(nil, nil))
		sig := types.NewSignatureType(nil, nil, nil,
			types.NewTuple(
				types.NewVar(token.NoPos, fmtPkg, "format", stringType),
				types.NewVar(token.NoPos, fmtPkg, "a", anySlice),
			),
			types.NewTuple(types.NewVar(token.NoPos, fmtPkg, "", stringType)),
			true)
		fmtPkg.Scope().Insert(types.NewFunc(token.NoPos, fmtPkg, "Sprintf", sig))
		app.SetImports([]*types.Package{fmtPkg})

		fns := DefaultFunctions(app)
		if _, ok := fns["printf"]; !ok {
			t.Errorf("DefaultFunctions(app) = %v, want printf from fmt.Sprintf", fns)
		}
		if _, ok := fns["print"]; ok {
			t.Errorf("DefaultFunctions(app) defines print, want it skipped when fmt.Sprint is missing")
		}
	})
}
