package main

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPositional(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		want  int
		pos   []string
		flag  bool
		other string
		err   string
	}{
		{name: "flag before", args: []string{"--force", "home"}, want: 1, pos: []string{"home"}, flag: true},
		{name: "flag after", args: []string{"home", "--force"}, want: 1, pos: []string{"home"}, flag: true},
		{name: "a flag on each side", args: []string{"--other=x", "home", "--force"}, want: 1, pos: []string{"home"}, flag: true, other: "x"},
		{name: "no flag at all", args: []string{"home"}, want: 1, pos: []string{"home"}},
		{name: "two positionals, flag after", args: []string{"claude", "personal", "--force"}, want: 2, pos: []string{"claude", "personal"}, flag: true},
		{name: "two positionals, flag between", args: []string{"claude", "--force", "personal"}, want: 2, pos: []string{"claude", "personal"}, flag: true},
		{name: "a flag value that looks positional", args: []string{"--other", "home", "s1"}, want: 1, pos: []string{"s1"}, other: "home"},
		{name: "none given", args: nil, want: 1, err: "usage"},
		{name: "one too many", args: []string{"home", "--force", "extra"}, want: 1, err: `unexpected argument "extra"`},
		{name: "an unknown flag after the positional", args: []string{"home", "--nope"}, want: 1, err: "not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(&strings.Builder{}) // the flag package's own message is not the test's
			force := fs.Bool("force", false, "")
			other := fs.String("other", "", "")
			pos, err := positional(fs, tc.args, tc.want, "usage: test <name> [--force]")
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one mentioning %q", err, tc.err)
				}
				return
			case err != nil:
				t.Fatalf("err = %v", err)
			}
			if !slices.Equal(pos, tc.pos) {
				t.Errorf("positionals = %v, want %v", pos, tc.pos)
			}
			if *force != tc.flag {
				t.Errorf("--force = %v, want %v — a flag was lost", *force, tc.flag)
			}
			if *other != tc.other {
				t.Errorf("--other = %q, want %q", *other, tc.other)
			}
		})
	}
}

// Every command that takes a positional argument and a flag must accept the
// flag last, because that is how people type it and how this repo's own usage
// lines and error messages write it. `yad disconnect <name> --force` was
// refused by its own parser while three documents told the operator to run it.
//
// The forms below are the documented ones with the flag moved last. None of
// them can succeed in a bare profile — there is no hub, no daemon and no
// account — so what is asserted is the failure: it must be the command's own,
// never the parser complaining about an argument it was given on purpose.
func TestEveryCommandTakesItsFlagsAfterItsPositionals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string // the command's own refusal, which only a parsed flag reaches
	}{
		{
			name: "disconnect",
			args: []string{"disconnect", "home", "--force"},
			want: "no connection called",
		},
		{
			// --connection parsed means the id is not looked up in the store;
			// the daemon is asked for it, and there is none.
			name: "sessions close",
			args: []string{"sessions", "close", "s1", "--connection", "home"},
			want: "none is running for this profile",
		},
		{
			// A URL and no token: reached only once --name has been parsed,
			// since an unparsed one would be an unexpected argument instead.
			name: "connect",
			args: []string{"connect", "https://hub.example/v1", "--name", "home"},
			want: "no registration token",
		},
		{
			name: "hub admin-token revoke",
			args: []string{"hub", "admin-token", "revoke", "nosuch"},
			want: "no admin token",
		},
		{
			// The five commands in cmd_hub_run.go went through their own copy
			// of this helper and were never in this table.
			name: "hub cancel",
			args: []string{"hub", "cancel", "r1", "--hub", "https://hub.example"},
			want: "admin token",
		},
		{
			name: "hub watch",
			args: []string{"hub", "watch", "r1", "--hub", "https://hub.example"},
			want: "admin token",
		},
		{
			name: "hub close-session",
			args: []string{"hub", "close-session", "s1", "--hub", "https://hub.example"},
			want: "admin token",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProfile(t)
			code, out, errs := p.yad("", tc.args...)
			if code == 0 {
				t.Fatalf("exit 0 in a bare profile: %q", out)
			}
			if strings.Contains(errs, "unexpected argument") || strings.Contains(errs, "usage:") {
				t.Fatalf("the parser refused the documented form: %s", errs)
			}
			if !strings.Contains(errs, tc.want) {
				t.Errorf("error = %q, want the command's own refusal mentioning %q", errs, tc.want)
			}
		})
	}
}

// `yad account remove` succeeds on an account that was never there, so it
// cannot prove a parsed flag by its refusal like the commands above. What
// proves it is the absence of the question --yes exists to skip: unparsed, it
// would read the empty stdin and refuse instead.
func TestAccountRemoveTakesYesAfterItsPositionals(t *testing.T) {
	p := newProfile(t)
	code, out, errs := p.yad("", "account", "remove", "claude", "nosuch", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if strings.Contains(out, "?") || strings.Contains(out, "type") {
		t.Errorf("--yes was not parsed; it asked: %q", out)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("output = %q", out)
	}
}

// The same forms with a flag the command does not have: still the parser's to
// refuse, so a typo is not mistaken for a positional argument.
func TestAnUnknownFlagAfterAPositionalIsRefused(t *testing.T) {
	p := newProfile(t)
	code, _, errs := p.yad("", "disconnect", "home", "--forcee")
	if code == 0 || !strings.Contains(errs, "not defined") {
		t.Errorf("exit %d, err %q — want the flag package's own refusal", code, errs)
	}
}

// A guard for the next command. Collecting positional arguments off a flag
// set is what the hand-rolled shape does — parse, take the first argument,
// parse the rest — and it had been written three times before one of them got
// it wrong. Reading fs.Arg or fs.Args is the thing only the helper may do.
//
// The first version of this guard counted fs.Parse calls per function, and
// missed parseInterleaved, which parses once inside a loop: it enforced the
// shape of the bug it had just seen rather than the shape of the bug. It also
// counted per file, which flagged four innocent files — the same distinction
// the defect itself turns on.
func TestOnlyTheHelperCollectsPositionals(t *testing.T) {
	// run reads the subcommand and hands the rest on, so it is the one place
	// that must not consume every positional.
	allowed := map[string]bool{"parseInterleaved": true, "run": true}

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || allowed[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok || x.Name != "fs" {
					return true
				}
				if sel.Sel.Name == "Arg" || sel.Sel.Name == "Args" {
					t.Errorf("%s: %s reads fs.%s; take the positionals from positional() or parseInterleaved() so a flag may come last",
						filepath.Base(path), fn.Name.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("no command source was read, so this guard proved nothing")
	}
}
