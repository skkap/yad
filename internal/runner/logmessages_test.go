package runner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogMessagesReachingHealthAreLiterals is the second leg of the argument
// that recent_errors cannot leak a secret. The first is that healthErrors
// sends a record's Message and never its Attrs, which is where a wrapped
// error's text, a path on this machine and a child's output live. The second
// is this: every message that can reach the ring is a string constant in this
// repository, so the text leaving the machine is text a reviewer has read.
//
// A formatted message would break that — fmt.Sprintf is how the owner's
// username, a URL with a password in it or a harness's stderr would get into
// one — so a shipped file that formats a warning or an error fails here,
// naming itself. The whole module is walked: the ring wraps the daemon's
// handler, and every package the daemon hands a logger to writes through it.
//
// It matches on method name, so it is a backstop rather than a proof: a logger
// reached through an interface of another shape is beyond what reading the
// source can see. It is what catches the line someone would actually write.
func TestLogMessagesReachingHealthAreLiterals(t *testing.T) {
	// Where the message sits in each call's arguments. Warn(msg, attrs...),
	// WarnContext(ctx, msg, attrs...), Log(ctx, level, msg, attrs...).
	at := map[string]int{
		"Warn": 0, "Error": 0,
		"WarnContext": 1, "ErrorContext": 1,
		"Log": 2, "LogAttrs": 2,
	}
	const root = "../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			i, ok := at[sel.Sel.Name]
			// An err.Error() has no arguments and is not a logging call.
			if !ok || len(call.Args) <= i {
				return true
			}
			if !stringConstant(call.Args[i]) {
				t.Errorf("%s: %s's message is built at run time; a health report carries it to every hub, so it must be a literal — put the varying part in an attr, which stays on the machine",
					fset.Position(call.Args[i].Pos()), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// stringConstant reports whether e is a string literal, or literals joined
// with +. Anything else — a variable, a call, a format — is not one.
func stringConstant(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING
	case *ast.BinaryExpr:
		return v.Op == token.ADD && stringConstant(v.X) && stringConstant(v.Y)
	case *ast.ParenExpr:
		return stringConstant(v.X)
	}
	return false
}

// The judgement the walk above rests on, put to the cases that matter: the
// formatted message is exactly how a credential, a path or a harness's stderr
// would reach a hub, and it has to be refused whichever way it is written.
func TestOnlyALiteralCountsAsTheRunnersOwnWords(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want bool
	}{
		{name: "a sentence someone wrote", expr: `"could not read the account's home"`, want: true},
		{name: "two of them joined", expr: `"could not read " + "the account's home"`, want: true},
		{name: "a formatted one", expr: `fmt.Sprintf("could not read %s", home)`},
		{name: "a message concatenated with a value", expr: `"could not read " + home`},
		{name: "the error itself", expr: `err.Error()`},
		{name: "a variable", expr: `msg`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := parser.ParseExpr(tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			if got := stringConstant(e); got != tc.want {
				t.Errorf("stringConstant(%s) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}
