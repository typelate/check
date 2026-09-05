package check

import (
	"path/filepath"

	"golang.org/x/tools/go/packages"
)

// FindPackage returns the package from list whose Go files live in the
// directory dir names; a path ending in .go names its directory. Use it
// to pick the package to check from a packages.Load result.
func FindPackage(list []*packages.Package, dir string) (*packages.Package, bool) {
	d := dir
	if filepath.Ext(d) == ".go" {
		d = filepath.Dir(dir)
	}
	for _, pkg := range list {
		if len(pkg.GoFiles) > 0 && filepath.Dir(pkg.GoFiles[0]) == d {
			return pkg, true
		}
	}
	return nil, false
}

// FindPackageByPath returns the package from list with the import path.
func FindPackageByPath(list []*packages.Package, path string) (*packages.Package, bool) {
	for _, pkg := range list {
		if pkg.PkgPath == path {
			return pkg, true
		}
	}
	return nil, false
}
