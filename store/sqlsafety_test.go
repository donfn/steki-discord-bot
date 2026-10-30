package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var sqlWord = regexp.MustCompile(`(?i)\b(select|insert|update|delete|where|values|from)\b`)

// TestSQLIsNeverBuiltFromRuntimeValues is a tripwire for SQL injection. Every
// value a member can influence reaches the database as a ?N parameter; the
// text of a query is made only of literals, package constants and the
// placeholders helper. The test fails when SQL is formatted with fmt.Sprintf or
// concatenated with anything else, wherever the result ends up.
//
// It cannot follow a string through every variable, so it is a guard against
// the usual slip, not a proof. Two places are out of its reach on purpose:
// migrate runs the embedded migration files, and pgimport.go copies a fixed
// list of tables out of Postgres and goes away once the import is done.
func TestSQLIsNeverBuiltFromRuntimeValues(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	consts := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "pgimport.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		for _, decl := range f.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.CONST {
				for _, spec := range gd.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						consts[n.Name] = true
					}
				}
			}
		}
	}

	hasSQL := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		s, err := strconv.Unquote(lit.Value)
		return err == nil && sqlWord.MatchString(s)
	}
	// parts flattens a chain of + into its operands.
	var parts func(e ast.Expr) []ast.Expr
	parts = func(e ast.Expr) []ast.Expr {
		if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			return append(parts(b.X), parts(b.Y)...)
		}
		if p, ok := e.(*ast.ParenExpr); ok {
			return parts(p.X)
		}
		return []ast.Expr{e}
	}
	allowed := func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.BasicLit:
			return e.Kind == token.STRING
		case *ast.Ident:
			return consts[e.Name]
		case *ast.CallExpr:
			id, ok := e.Fun.(*ast.Ident)
			return ok && id.Name == "placeholders"
		}
		return false
	}

	checked := 0
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "migrate" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BinaryExpr:
					ops := parts(n)
					if n.Op != token.ADD || len(ops) < 2 || !slicesAny(ops, hasSQL) {
						return true
					}
					checked++
					for _, op := range ops {
						if !allowed(op) {
							t.Errorf("%s: %s builds SQL from a runtime value; pass it as a ?N parameter",
								fset.Position(op.Pos()), fn.Name.Name)
						}
					}
					return false // the operands were judged together
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" && strings.HasPrefix(sel.Sel.Name, "Sprint") &&
						len(n.Args) > 0 && hasSQL(n.Args[0]) {
						t.Errorf("%s: %s formats SQL with fmt; pass values as ?N parameters",
							fset.Position(n.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	// A parser that finds nothing would pass for ever.
	if checked == 0 {
		t.Error("found no concatenated query to check; the test no longer looks at the code")
	}
}

func slicesAny(exprs []ast.Expr, f func(ast.Expr) bool) bool {
	for _, e := range exprs {
		if f(e) {
			return true
		}
	}
	return false
}
