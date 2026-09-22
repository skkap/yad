package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The error codes are written down in three places a hub author reads — the
// constants, the description of Error.code in openapi.yaml, and HUB.md — and a
// code named in one and not the others is how `internal` went undocumented
// while yad hub sent it. The constants are the source; the other two must
// name every one of them, and say how many there are.
func TestEveryErrorCodeIsDocumented(t *testing.T) {
	codes := errorCodes(t)
	if len(codes) == 0 {
		t.Fatal("no Code constants found in error.go, so this test checks nothing")
	}
	words := map[int]string{9: "nine", 10: "ten", 11: "eleven", 12: "twelve", 13: "thirteen"}
	count, ok := words[len(codes)]
	if !ok {
		t.Fatalf("%d codes: add the word for it to this test", len(codes))
	}
	field, _ := reflect.TypeFor[Error]().FieldByName("Code")
	hub, err := os.ReadFile("../../HUB.md")
	if err != nil {
		t.Fatal(err)
	}
	flatHub := strings.Join(strings.Fields(string(hub)), " ")
	for _, doc := range []struct{ where, text, quote string }{
		{"the doc tag on Error.Code", field.Tag.Get("doc"), ""},
		{"HUB.md", flatHub, "`"},
	} {
		if want := "names " + count + " codes"; !strings.Contains(doc.text, want) {
			t.Errorf("%s does not say %q", doc.where, want)
		}
		for _, c := range codes {
			if !strings.Contains(doc.text, doc.quote+c+doc.quote) {
				t.Errorf("%s does not name %q", doc.where, c)
			}
		}
	}
}

// errorCodes is the value of every Code constant in error.go.
func errorCodes(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "error.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !strings.HasPrefix(name.Name, "Code") || !ok {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				codes = append(codes, v)
			}
		}
	}
	return codes
}

// HUB.md and the document's description of Grant.name are what a hub author
// validates grant names from, and neither is this file. Each must name every
// name and prefix grant.go refuses, or a hub checking from them offers a run
// every runner refuses whole.
func TestEveryRefusedGrantNameIsDocumented(t *testing.T) {
	var names []string
	for n := range deniedGrantNames {
		names = append(names, n)
	}
	for _, p := range deniedGrantPrefixes {
		names = append(names, p.prefix)
	}
	for n := range accountGrantNames {
		names = append(names, n)
	}
	for _, p := range accountGrantPrefixes {
		names = append(names, p.prefix)
	}
	hub, err := os.ReadFile("../../HUB.md")
	if err != nil {
		t.Fatal(err)
	}
	field, _ := reflect.TypeFor[Grant]().FieldByName("Name")
	for _, doc := range []struct{ where, text, quote string }{
		{"HUB.md", string(hub), "`"},
		{"the doc tag on Grant.Name", field.Tag.Get("doc"), ""},
	} {
		for _, n := range names {
			if !strings.Contains(doc.text, doc.quote+n+doc.quote) {
				t.Errorf("%s does not name %q, which grant.go refuses", doc.where, n)
			}
		}
	}
}
