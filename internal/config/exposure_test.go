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
			// Root in a disposable container is the one way a Claude run starts
			// as root at all, so telling that owner to become an ordinary user
			// is advice they have already declined. What is left to say is that
			// the harnesses still have root.
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
			// config.toml is written by the owner's editor and holds no secret,
			// so a 0644 one is not reported. This is why the list is explicit.
			name: "a world-readable config.toml",
			euid: 501,
			setup: func(t *testing.T, p Paths) {
				write(t, p.ConfigFile(), 0o644)
			},
			absent: []string{"chmod", "config.toml"},
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
				"`yad hub admin-token list`, then revoke"},
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
func TestEveryPrivateFileIsCheckedOrExcused(t *testing.T) {
	excused := map[string]string{
		"internal/config/credentials.go": "writePrivate: runner-id, credentials/* and hub-admin-token — all in privateFiles",
		"internal/store/store.go":        "state.db and hub.db — both in privateFiles",
		"internal/control/server.go":     "yad.sock and yad.lock: gone when the daemon stops, and inside the data directory this checks as a whole",
		"internal/logfile/logfile.go":    "the daemon's log, inside the data directory this checks as a whole",
		"cmd/yad/cmd_lifecycle.go":       "stderr.log, inside the data directory this checks as a whole",
		"internal/runner/executor.go":    "a run's grant files, deleted when the run ends and inside the data directory",
		"internal/workdir/hook.go":       "a marker file in a workdir, which holds no secret",
		"internal/workdir/workdir.go":    "a marker file in a checkout, which holds no secret",
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds recorded fixtures, and bin holds what make build
			// left behind.
			if n := d.Name(); n == "testdata" || n == "bin" || n == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		// Test helpers write 0600 files for tests to read; none of them is a
		// profile file on anyone's machine.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		var found bool
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.INT && lit.Value == "0o600" {
				found = true
			}
			return !found
		})
		if !found {
			return nil
		}
		if _, ok := excused[rel]; !ok && !strings.Contains(rel, "/codextest/") {
			t.Errorf("%s creates a 0600 file that privateFiles has never heard of — add it to privateFiles, or add it to this test's excused list with the reason it needs no warning of its own", rel)
		}
		delete(excused, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A stale excuse is the same rot in the other direction: it would go on
	// excusing a file that no longer writes anything private.
	for rel, why := range excused {
		t.Errorf("%s no longer creates a 0600 file, so its excuse (%s) is stale — delete the line", rel, why)
	}
}

// A secret that has been readable by others must be assumed leaked, which a
// chmod does not undo — so the two files that hold a secret say how to retire
// it, and the two that do not are left with the chmod alone. Getting this
// wrong is worse than saying nothing: an owner does the chmod, feels finished,
// and goes on using a credential they were just told to distrust.
func TestALeakedSecretIsRotatedNotJustClosed(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 501 }
	t.Cleanup(func() { geteuid = old })
	p := paths(t)
	write(t, filepath.Join(p.Config, "credentials", "yashiki"), 0o644)
	write(t, p.HubAdminToken(), 0o644)
	write(t, p.HubDB(), 0o644)
	write(t, filepath.Join(p.Config, "runner-id"), 0o644)
	write(t, p.StateDB(), 0o644)

	for _, line := range Exposures(p) {
		// Match the path this line is about, not the prose after it: the
		// runner-id line mentions the word "credentials" while being about a
		// file that is not one.
		path, _, _ := strings.Cut(line, " is -rw")
		// hub.db holds a queued run's grants in plaintext; state.db does not,
		// because Loop.record strips them before writing.
		secret := strings.Contains(path, "credentials") || strings.HasSuffix(path, "hub-admin-token") ||
			strings.HasSuffix(path, "hub.db")
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
