package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// paths gives a profile whose two directories exist and are private, which is
// the machine Exposures should have nothing to say about.
func paths(t *testing.T) Paths {
	t.Helper()
	p := Paths{Profile: DefaultProfile, Config: t.TempDir(), Data: t.TempDir()}
	for _, d := range []string{p.Config, p.Data} {
		if err := os.Chmod(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func write(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Separately from the write, because the umask takes bits off a create.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// The three things the run-it-safely guide tells an owner to care about, each
// on its own: root, a directory another user can reach, and a profile file
// another user can read.
func TestExposures(t *testing.T) {
	for _, tc := range []struct {
		name string
		euid int
		// sandbox is the runner's own IS_SANDBOX.
		sandbox string
		setup   func(t *testing.T, p Paths)
		want    []string
		absent  []string
	}{
		{
			name:   "a private profile owned by an ordinary user",
			euid:   501,
			setup:  func(*testing.T, Paths) {},
			absent: []string{"warning", "root", "chmod"},
		},
		{
			name:  "root",
			euid:  0,
			setup: func(*testing.T, Paths) {},
			want:  []string{"running as root", "run yad as an ordinary user", "refuses the default permission mode"},
			// The owner has not declared a sandbox, so nothing may say they have.
			absent: []string{"IS_SANDBOX=1 —"},
		},
		{
			// Not the only way a Claude run starts as root — the adapter
			// refuses root only for bypassPermissions — but it is the one this
			// owner has declared, so telling them to become an ordinary user
			// is advice they have already considered. What is left to say is
			// that the harnesses still have root.
			name:    "root with the sandbox declared",
			euid:    0,
			sandbox: "1",
			setup:   func(*testing.T, Paths) {},
			want:    []string{"running as root with IS_SANDBOX=1", "every harness this runner runs has root"},
			absent:  []string{"run yad as an ordinary user"},
		},
		{
			name: "a data directory other users can reach",
			euid: 501,
			setup: func(t *testing.T, p Paths) {
				if err := os.Chmod(p.Data, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want:   []string{"the data directory", "is -rwxr-xr-x", "chmod 700 "},
			absent: []string{"the config directory"},
		},
		{
			// The ticket named the data directory; the credentials are in the
			// config directory, so the same check runs over both.
			name: "a config directory other users can reach",
			euid: 501,
			setup: func(t *testing.T, p Paths) {
				if err := os.Chmod(p.Config, 0o750); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"the config directory", "is -rwxr-x---", "chmod 700 "},
		},
		{
			name: "a credential another user can read",
			euid: 501,
			setup: func(t *testing.T, p Paths) {
				write(t, filepath.Join(p.Config, "credentials", "yashiki"), 0o644)
			},
			want: []string{
				filepath.Join("credentials", "yashiki") + " is -rw-r--r--",
				`connection "yashiki"`,
				"chmod 600 ",
				// A chmod stops the next reader, not the one who already read
				// it, so the action a leaked secret needs is the rotation.
				"not the one who already read it",
				"revoke this credential at the hub and run `yad connect` again",
			},
		},
		{
			// A credential the owner tightened past 0600 is not an exposure,
			// and warning about it would teach an owner to stop reading these.
			name: "a credential nobody else can read",
			euid: 501,
			setup: func(t *testing.T, p Paths) {
				write(t, filepath.Join(p.Config, "credentials", "yashiki"), 0o400)
				write(t, filepath.Join(p.Config, "runner-id"), 0o600)
				write(t, p.StateDB(), 0o600)
			},
			absent: []string{"chmod"},
		},
		{
			// config.Save writes config.toml 0600 on purpose — it names the
			// hubs and the accounts — so a world-readable one is reported. It
			// holds no secret, so the mode is the whole of its fix and it must
			// not ask for a rotation.
			name: "a world-readable config.toml",
			euid: 501,
			setup: func(t *testing.T, p Paths) {
				write(t, p.ConfigFile(), 0o644)
			},
			want:   []string{"config.toml is -rw-r--r--", "names the hubs", "chmod 600 "},
			absent: []string{"not the one who already read it"},
		},
		{
			name: "every profile file at once, each named",
			euid: 501,
			setup: func(t *testing.T, p Paths) {
				write(t, filepath.Join(p.Config, "runner-id"), 0o644)
				write(t, p.HubAdminToken(), 0o640)
				write(t, p.StateDB(), 0o604)
				write(t, p.HubDB(), 0o644)
				write(t, filepath.Join(p.Config, "credentials", "b"), 0o644)
				write(t, filepath.Join(p.Config, "credentials", "a"), 0o644)
			},
			want: []string{"runner-id", "hub-admin-token", "state.db", "hub.db",
				`connection "a"`, `connection "b"`,
				// The same file is the default token for a remote hub too, so
				// the advice must send the owner to the hub that issued it
				// rather than straight at this machine's hub.db.
				"revoke it at the hub that issued it",
				"`yad hub admin-token list` then revoke"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := geteuid
			geteuid = func() int { return tc.euid }
			t.Cleanup(func() { geteuid = old })
			t.Setenv("IS_SANDBOX", tc.sandbox)
			p := paths(t)
			tc.setup(t, p)
			got := strings.Join(Exposures(p), "\n")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("no %q in:\n%s", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Errorf("unwanted %q in:\n%s", absent, got)
				}
			}
		})
	}
}

// A profile whose directories were never created reports nothing rather than
// failing: `yad doctor` on a machine where nothing has been set up yet is the
// first thing anyone runs, and absence is not an exposure.
func TestExposuresOnAProfileThatDoesNotExist(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 501 }
	t.Cleanup(func() { geteuid = old })
	dir := t.TempDir()
	p := Paths{Profile: DefaultProfile, Config: filepath.Join(dir, "none"), Data: filepath.Join(dir, "nor-this")}
	if got := Exposures(p); got != nil {
		t.Errorf("Exposures = %q, want nothing", got)
	}
	if _, err := os.Stat(p.Config); !os.IsNotExist(err) {
		t.Errorf("Exposures created %s", p.Config)
	}
}

// The credentials directory is read to find the per-connection files. One that
// is itself unreadable is not a reason to report nothing at all: the rest of
// the profile is still checked.
func TestExposuresSurvivesAnUnreadableCredentialsDirectory(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 501 }
	t.Cleanup(func() { geteuid = old })
	p := paths(t)
	write(t, filepath.Join(p.Config, "credentials", "yashiki"), 0o644)
	if err := os.Chmod(filepath.Join(p.Config, "credentials"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(p.Config, "credentials"), 0o700) })
	write(t, filepath.Join(p.Config, "runner-id"), 0o644)
	got := strings.Join(Exposures(p), "\n")
	if !strings.Contains(got, "runner-id") {
		t.Errorf("runner-id not checked:\n%s", got)
	}
}

// privateFiles is a hand-written list, and nothing in the compiler connects a
// new 0600 file to it: the day someone writes one somewhere else, `yad doctor`
// silently stops covering it and docs/run-it-safely.md silently becomes wrong.
// So this walks the repository's own source for the places that create a file
// 0600 and fails on one this list has never heard of. The fix is one line in
// privateFiles, or one line here saying why that file needs no warning of its
// own.
//
// It matches by file rather than by line so that moving code inside a file does
// not fail it, and it reads the syntax tree rather than grepping so that 0o600
// in a comment or a test's expectation is not a site.
//
// A literal is not the only way to make one. config.WriteSecret and
// writePrivate are the sanctioned ways to create a profile secret and neither
// puts 0o600 at the call site, so calls to them count as sites too — which is
// how config.toml came to be on the list at all.
var privateWriters = map[string]bool{"WriteSecret": true, "writePrivate": true}

func TestEveryPrivateFileIsCheckedOrExcused(t *testing.T) {
	excused := map[string]string{
		"internal/config/credentials.go": "writePrivate: runner-id, credentials/* and hub-admin-token — all in privateFiles",
		"internal/config/config.go":      "Save writes config.toml 0600 — in privateFiles, as the mode without the rotation",
		"internal/config/identity.go":    "runner-id, in privateFiles",
		"cmd/yad/cmd_hub.go":             "WriteSecret for hub-admin-token, which is in privateFiles",
		"internal/store/store.go":        "state.db and hub.db — both in privateFiles",
		"internal/control/server.go":     "yad.sock and yad.lock: gone when the daemon stops, and inside the data directory this checks as a whole",
		"internal/logfile/logfile.go":    "the daemon's log, inside the data directory this checks as a whole",
		"cmd/yad/cmd_lifecycle.go":       "stderr.log, inside the data directory this checks as a whole",
		"internal/runner/executor.go":    "a run's grant files, deleted when the run ends and inside the data directory",
		"internal/workdir/hook.go":       "a marker file in a workdir, which holds no secret",
		"internal/workdir/workdir.go":    "a marker file in a checkout, which holds no secret",
	}
	root := filepath.Join("..", "..")
	sources, err := moduleSources(root)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, rel := range sources {
		// Test helpers write 0600 files for tests to read; none of them is a
		// profile file on anyone's machine.
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.INT && n.Value == "0o600" {
					found = true
				}
			case *ast.CallExpr:
				// WriteSecret / config.WriteSecret / writePrivate, by the name
				// called rather than by resolving the package, which would
				// need type information this walk deliberately does not load.
				switch fn := n.Fun.(type) {
				case *ast.Ident:
					found = found || privateWriters[fn.Name]
				case *ast.SelectorExpr:
					found = found || privateWriters[fn.Sel.Name]
				}
			}
			return !found
		})
		if !found {
			continue
		}
		if _, ok := excused[rel]; !ok && !strings.Contains(rel, "/codextest/") {
			t.Errorf("%s creates a 0600 file that privateFiles has never heard of — add it to privateFiles, or add it to this test's excused list with the reason it needs no warning of its own", rel)
		}
		delete(excused, rel)
	}
	// A stale excuse is the same rot in the other direction: it would go on
	// excusing a file that no longer writes anything private.
	for rel, why := range excused {
		t.Errorf("%s no longer creates a 0600 file, so its excuse (%s) is stale — delete the line", rel, why)
	}
}

// moduleSources is every .go file of this module under root, slash-separated
// and relative to it: the files `go build ./...` reads, whatever else sits in
// the directory. Go's own rule decides, so the walk and the compiler agree on
// what the source is — a directory starting with . or _ is not part of the
// module, testdata is not, and a directory with a go.mod of its own is another
// module. An agent's worktree nested under .claude/worktrees is all of those
// at once, and its copy of cmd/yad would otherwise be reported here as a
// second, unlisted writer of every private file.
//
// Not `git ls-files`: CI has git, but a test that needs a checkout to pass
// fails in a module cache or an exported tarball, where the source is the
// same and the answer should be too.
func moduleSources(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			// bin holds what make build left behind.
			if n := d.Name(); strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") || n == "testdata" || n == "bin" {
				return fs.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

// The walk above is only as good as what it leaves out. Each case is a copy of
// a real source file somewhere Go would not build it from, beside the one it
// would: an agent's worktree under .claude/worktrees — the case that failed
// `make check` on the main checkout while any agent was at work — and a nested
// module with no dot in its path.
func TestModuleSourcesIsTheModuleAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
	}{
		{"an agent worktree", []string{".claude/worktrees/agent-1/go.mod", ".claude/worktrees/agent-1/cmd/yad/cmd_hub.go"}},
		{"a worktree without its go.mod", []string{".claude/worktrees/agent-2/cmd/yad/cmd_hub.go"}},
		{"a nested module", []string{"worktrees/agent-3/go.mod", "worktrees/agent-3/cmd/yad/cmd_hub.go"}},
		{"an underscore directory", []string{"_old/cmd/yad/cmd_hub.go"}},
		{"testdata", []string{"internal/x/testdata/cmd_hub.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range append([]string{"go.mod", "cmd/yad/cmd_hub.go"}, tc.files...) {
				path := filepath.Join(root, filepath.FromSlash(f))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := moduleSources(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] != "cmd/yad/cmd_hub.go" {
				t.Errorf("moduleSources = %q, want the module's own cmd/yad/cmd_hub.go alone", got)
			}
		})
	}
}

// A secret that has been readable by others must be assumed leaked, which a
// chmod does not undo — so an entry that holds one says how to retire it, and
// an entry that does not is left with the chmod alone. Getting this wrong is
// worse than saying nothing: an owner does the chmod, feels finished, and goes
// on using a credential they were just told to distrust.
//
// The fixture is built from privateFiles itself, so every entry the code has
// is judged. An earlier version wrote a hand-picked few, which meant a new
// entry was covered only if whoever added it also remembered this test — and
// hub.db-wal went a whole round with the wrong advice because it did not.
// secretBase names the entries whose warning must carry a rotation, by base
// name. Spelled out rather than derived, because the correspondence is what is
// under test: deriving the expectation the way the code does would make the
// test pass for any rule at all. A -shm indexes a -wal and holds no run data,
// so it is not here even when its database is.
var secretBase = map[string]bool{
	"hub-admin-token": true,
	"hub.db":          true, "hub.db-wal": true,
	// The runner's store keeps no grant, but an event body is whatever the
	// harness printed and nothing deletes one.
	"state.db": true, "state.db-wal": true,
}

func TestALeakedSecretIsRotatedNotJustClosed(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 501 }
	t.Cleanup(func() { geteuid = old })
	p := paths(t)
	// Every entry the code actually has, not a hand-picked few: Exposures
	// prints a line only for a file that exists, so an entry with no file here
	// is an entry this test never judges. Adding one to privateFiles without
	// adding it to secretBase now fails rather than passing unseen.
	write(t, filepath.Join(p.Config, "credentials", "yashiki"), 0o644)
	for _, f := range privateFiles(p) {
		write(t, f.path, 0o644)
	}
	if n := len(privateFiles(p)); n < 9 {
		t.Fatalf("privateFiles returned %d entries; the fixture below is meant to cover the whole list", n)
	}

	for _, line := range Exposures(p) {
		// Match the path this line is about, not the prose after it: the
		// runner-id line mentions the word "credentials" while being about a
		// file that is not one.
		path, _, _ := strings.Cut(line, " is -rw")
		secret := strings.Contains(path, "credentials"+string(os.PathSeparator)) ||
			secretBase[filepath.Base(path)]
		rotates := strings.Contains(line, "not the one who already read it")
		if secret != rotates {
			t.Errorf("secret=%v but rotation advice=%v:\n%s", secret, rotates, line)
		}
		if !strings.Contains(line, "chmod 600 ") {
			t.Errorf("no chmod in:\n%s", line)
		}
	}
	// `yad disconnect` is not built yet (it answers "arrives in epic E7"), so
	// no warning may tell an owner to run it. The credential path is the one
	// config.Credential already gives when ReadSecret refuses the same file.
	got := strings.Join(Exposures(p), "\n")
	if strings.Contains(got, "yad disconnect") {
		t.Errorf("a warning names a command this yad does not have:\n%s", got)
	}
	// `yad hub admin-token create` refuses while the file is still there —
	// revoke only touches hub.db — so an advice line that skips the delete
	// sends the owner into that refusal.
	if !strings.Contains(got, "delete "+p.HubAdminToken()) {
		t.Errorf("the admin-token advice skips deleting the file, which is what makes create refuse:\n%s", got)
	}
	// The runner id is not a secret, and saying it is would have an owner
	// rotating an identity that rotating retires.
	for _, line := range Exposures(p) {
		if strings.Contains(line, "runner-id") && strings.Contains(line, "take over") {
			t.Errorf("the runner id is reported as a takeover that reading it cannot cause:\n%s", line)
		}
	}
}

// The next action is a command an owner pastes, so a path with a space in it
// has to survive the trip. A profile directory comes from an environment
// variable and is not constrained to shell-safe characters.
func TestNextActionsSurviveAPathWithASpace(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 501 }
	t.Cleanup(func() { geteuid = old })
	base := t.TempDir()
	dir := filepath.Join(base, "My Disk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := Paths{Profile: DefaultProfile, Config: dir, Data: dir}
	write(t, filepath.Join(dir, "credentials", "yashiki"), 0o644)
	got := strings.Join(Exposures(p), "\n")
	if !strings.Contains(got, "chmod 700 '"+dir+"'") {
		t.Errorf("the directory command is not pasteable:\n%s", got)
	}
	if !strings.Contains(got, "chmod 600 '"+filepath.Join(dir, "credentials", "yashiki")+"'") {
		t.Errorf("the credential command is not pasteable:\n%s", got)
	}
	// A path needing no quoting keeps none, so the ordinary message stays
	// readable — which is the whole reason shellArg checks before quoting.
	// Checked on the command alone: these messages are prose and carry
	// apostrophes of their own ("every run's events").
	plain := paths(t)
	write(t, plain.StateDB(), 0o644)
	for _, line := range Exposures(plain) {
		_, cmd, ok := strings.Cut(line, "chmod 600 ")
		if ok && strings.HasPrefix(cmd, "'") {
			t.Errorf("an ordinary path was quoted:\n%s", line)
		}
	}
}

// A directory nobody else can reach by mode is still not yours when somebody
// else owns it, and control.checkDir already refuses to bind a socket in one.
// Reporting only the mode would call such a profile clean.
func TestExposuresReportsADirectoryOwnedBySomeoneElse(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 501 }
	t.Cleanup(func() { geteuid = old })
	p := paths(t)
	// Not chown — a test cannot give a directory away without root. What the
	// check compares is the directory's uid against the process's, so a
	// process claiming to be somebody else is the same comparison.
	got := strings.Join(Exposures(p), "\n")
	if got != "" {
		t.Fatalf("a private profile owned by this process reported:\n%s", got)
	}
	if u := os.Getuid(); u == 0 {
		t.Skip("running as root owns everything")
	}
	root := Paths{Profile: DefaultProfile, Config: "/", Data: "/"}
	got = strings.Join(Exposures(root), "\n")
	if !strings.Contains(got, "belongs to uid 0") || !strings.Contains(got, "YAD_CONFIG_DIR") {
		t.Errorf("a directory owned by uid 0 was not reported as another user's:\n%s", got)
	}
}

// Exposures answers one question about the machine, and its wording may not
// borrow authority from questions it never asked. With IS_SANDBOX declared it
// knows root no longer refuses a Claude run; it has not looked at whether a
// harness is installed, whether its version probe answers, or whether the
// configured permission mode is one the adapter accepts — all of which the
// harness table above these lines reports. Saying "Claude Code will start"
// contradicted doctor's own output in a bare container, where the same run
// prints "No drivable harness found".
func TestTheSandboxWarningClaimsOnlyWhatItChecked(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = old })
	t.Setenv("IS_SANDBOX", "1")
	got := strings.Join(Exposures(paths(t)), "\n")
	for _, overclaim := range []string{"will start", "will run", "is installed", "is ready"} {
		if strings.Contains(got, overclaim) {
			t.Errorf("the warning claims %q, which Exposures never checked:\n%s", overclaim, got)
		}
	}
	if !strings.Contains(got, "every harness this runner runs has root") {
		t.Errorf("the warning dropped the fact it does establish:\n%s", got)
	}
}

// SQLite gives -wal and -shm the database's own mode
// (store.TestSidecarsTakeTheDatabaseMode), and an uncleaned exit leaves them
// behind — so an exposed hub.db has two more files beside it, of which the
// -wal gives up the same grants and the -shm gives up nothing but its mode.
// That distinction is the point: saying both leak would send an owner rotating
// over a wal-index.
func TestSidecarsAreReportedBesideTheirDatabase(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 501 }
	t.Cleanup(func() { geteuid = old })
	p := paths(t)
	stores := []string{p.HubDB(), p.HubDB() + "-wal", p.HubDB() + "-shm",
		p.StateDB(), p.StateDB() + "-wal", p.StateDB() + "-shm"}
	for _, f := range stores {
		write(t, f, 0o644)
	}
	got := Exposures(p)
	for _, want := range stores {
		var found bool
		for _, line := range got {
			if strings.HasPrefix(line, want+" is ") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was not reported:\n%s", want, strings.Join(got, "\n"))
		}
	}
	for _, line := range got {
		switch {
		// The -wal carries the database's pages, so it gives up what the
		// database gives up. The -shm is the wal-index: saying it holds
		// uncheckpointed run data would send an owner rotating over a file
		// that has none.
		case strings.HasPrefix(line, p.HubDB()+"-wal is "):
			if !strings.Contains(line, "rotate every secret") || !strings.Contains(line, "not yet checkpointed") {
				t.Errorf("the hub -wal does not carry the database's own remedy:\n%s", line)
			}
		case strings.HasPrefix(line, p.HubDB()+"-shm is "):
			if strings.Contains(line, "rotate") || strings.Contains(line, "not yet checkpointed") {
				t.Errorf("the -shm claims to hold run data:\n%s", line)
			}
		case strings.HasPrefix(line, p.StateDB()+"-wal is "):
			// The runner's store keeps no grant, so this must not promise a
			// rotation of grants — but an event body is whatever the harness
			// printed, so it does carry the conditional warning.
			if strings.Contains(line, "any run's grants") {
				t.Errorf("the runner store's -wal claims grants it never holds:\n%s", line)
			}
			if !strings.Contains(line, "if a run ever printed a credential") {
				t.Errorf("the runner store's -wal drops what it can hold:\n%s", line)
			}
		case strings.HasPrefix(line, p.StateDB()+"-shm is "):
			if strings.Contains(line, "rotate") || strings.Contains(line, "printed a credential") {
				t.Errorf("the runner store's -shm claims to hold run data:\n%s", line)
			}
		}
		// Whichever file it is about, fixing it alone is not enough: the next
		// open copies the database's mode back onto the sidecar.
		if strings.Contains(line, "-wal is ") || strings.Contains(line, "-shm is ") {
			if !strings.Contains(line, "hands the mode straight back") {
				t.Errorf("a sidecar's fix does not send the owner to the database:\n%s", line)
			}
		}
	}
}
