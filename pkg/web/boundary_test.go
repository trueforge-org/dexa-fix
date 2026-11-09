package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// The Frontend and the Backend are two types so that the compiler keeps the page code away from the
// database, the keys and the other state of the backend, and the other way round: the only bridge
// is the internal API (Frontend.api). This test documents the rule and guards against someone
// embedding one type in the other, or sharing a field, to get around it.

var backendOnlyFields = []string{
	"store", "importers", "cookieKey", "limiter", "nonces", "bcryptSlots", "slSlots", "sl", "auditNoise",
	"estCache", "httpClient", "oidcCache", "oidcMu", "authChain", "serviceSecrets", "internalBudget",
	"publicSlots", "onSettings", "ForcePasswordLogin",
}

var frontendOnlyFields = []string{
	"tmpl", "assetVer", "api", "apiUpstream", "apiHandler", "configETag", "warnedVersion", "cop",
	"lmu", "handler", "https", "h3", "h3port", "listenAddr", "bindErr", "unlimited",
}

func TestFrontendAndBackendDoNotReachIntoEachOther(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg := pkgs["web"]
	if pkg == nil {
		t.Fatal("package web not found")
	}
	checked := map[string]int{}
	for name, f := range pkg.Files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				recv, forbidden := receiver(d)
				if recv == "" {
					continue
				}
				checked[recv]++
				ast.Inspect(d.Body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == d.Recv.List[0].Names[0].Name && slices.Contains(forbidden, sel.Sel.Name) {
						t.Errorf("%s: method %s of %s uses the other half's field %s", name, d.Name.Name, recv, sel.Sel.Name)
					}
					return true
				})
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					ts, ok := sp.(*ast.TypeSpec)
					st, isStruct := typeStruct(ts, ok)
					if !isStruct || (ts.Name.Name != "Frontend" && ts.Name.Name != "Backend") {
						continue
					}
					other := map[string]string{"Frontend": "Backend", "Backend": "Frontend"}[ts.Name.Name]
					for _, fld := range st.Fields.List {
						if strings.Contains(types.ExprString(fld.Type), other) {
							t.Errorf("%s: %s has a field of type %s", name, ts.Name.Name, other)
						}
					}
				}
			}
		}
	}
	if checked["Frontend"] == 0 || checked["Backend"] == 0 {
		t.Fatalf("no methods found: %v", checked)
	}
}

// receiver returns the receiver type of d (Frontend or Backend) and the fields its body must not use.
func receiver(d *ast.FuncDecl) (string, []string) {
	if d.Recv == nil || len(d.Recv.List) != 1 || len(d.Recv.List[0].Names) != 1 || d.Body == nil {
		return "", nil
	}
	switch types.ExprString(d.Recv.List[0].Type) {
	case "*Frontend":
		return "Frontend", backendOnlyFields
	case "*Backend":
		return "Backend", frontendOnlyFields
	}
	return "", nil
}

func typeStruct(ts *ast.TypeSpec, ok bool) (*ast.StructType, bool) {
	if !ok {
		return nil, false
	}
	st, isStruct := ts.Type.(*ast.StructType)
	return st, isStruct
}
