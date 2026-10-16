package hostool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/seamtest"
)

// browserMachine is a machine with nothing on PATH, no override, a home of
// its own, and its standard locations under a root the test fills.
func browserMachine(t *testing.T) (root, home string) {
	t.Helper()
	isolate(t, "YAD_GIT_PATH", "")
	root = BrowserRootForTests
	home = filepath.Join(t.TempDir(), "home", "alice")
	t.Setenv("HOME", home)
	for _, name := range []string{"PLAYWRIGHT_BROWSERS_PATH", "XDG_CACHE_HOME", "LOCALAPPDATA", "ProgramFiles", "ProgramFiles(x86)"} {
		t.Setenv(name, "")
	}
	return root, home
}

// put writes body at path under root, making its directories.
func put(t *testing.T, root, path, body string, mode os.FileMode) string {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return full
}

func plist(version string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
	<key>CFBundleName</key>
	<string>Google Chrome</string>
	<key>CFBundleShortVersionString</key>
	<string>` + version + `</string>
</dict></plist>
`
}

// macApp installs name.app under dir (relative to root) with version in its
// bundle, and an executable that would fail if anything ran it: on macOS the
// version is read, never asked.
func macApp(t *testing.T, root, dir, name, version string) {
	t.Helper()
	app := filepath.Join(dir, name+".app", "Contents")
	put(t, root, filepath.Join(app, "Info.plist"), plist(version), 0o644)
	put(t, root, filepath.Join(app, "MacOS", name), "#!/bin/sh\nexit 3\n", 0o755)
}

func answeringVersion(line string) string { return "#!/bin/sh\necho '" + line + "'\n" }

func TestBrowserIsAbsentWhereNoneIsInstalled(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			browserMachine(t)
			d := detectBrowser(context.Background(), goos)
			if d.ID != BrowserID || d.Present || d.Kind != "" || d.Version != "" || d.Error != "" {
				t.Errorf("got %+v, want browser absent and nothing else said", d)
			}
		})
	}
}

func TestBrowserOnMacIsReadFromItsBundle(t *testing.T) {
	for _, tc := range []struct {
		name, dir, app, kind string
	}{
		{"Chrome in /Applications", "/Applications", "Google Chrome", BrowserChrome},
		{"Chromium in /Applications", "/Applications", "Chromium", BrowserChromium},
		{"Chrome in the user's Applications", "~/Applications", "Google Chrome", BrowserChrome},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, home := browserMachine(t)
			macApp(t, root, strings.Replace(tc.dir, "~", home, 1), tc.app, "155.0.8059.82")
			d := detectBrowser(context.Background(), "darwin")
			if !d.Present || d.Kind != tc.kind || d.Version != "155.0.8059.82" || d.Error != "" {
				t.Errorf("got %+v, want %s 155.0.8059.82 with no error", d, tc.kind)
			}
		})
	}
}

func TestBrowserOnLinuxIsAskedItsVersion(t *testing.T) {
	for _, tc := range []struct{ path, line, kind, version string }{
		{"/opt/google/chrome/chrome", "Google Chrome 141.0.7390.65 ", BrowserChrome, "141.0.7390.65"},
		{"/usr/bin/chromium", "Chromium 140.0.7339.80 built on Debian GNU/Linux 13 (trixie)", BrowserChromium, "140.0.7339.80"},
		{"/snap/bin/chromium", "Chromium 141.0.7390.54 snap", BrowserChromium, "141.0.7390.54"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			root, _ := browserMachine(t)
			put(t, root, tc.path, answeringVersion(tc.line), 0o755)
			d := detectBrowser(context.Background(), "linux")
			if !d.Present || d.Kind != tc.kind || d.Version != tc.version || d.Error != "" || d.Path != tc.path {
				t.Errorf("got %+v, want %s %s at %s", d, tc.kind, tc.version, tc.path)
			}
		})
	}
}

// A browser PATH finds is found however the OS lays out its installs: a
// Chromium a package manager put somewhere of its own.
func TestBrowserOnPATH(t *testing.T) {
	browserMachine(t)
	dir := t.TempDir()
	put(t, dir, "chromium-browser", answeringVersion("Chromium 139.0.7258.5"), 0o755)
	t.Setenv("PATH", dir)
	for _, goos := range []string{"darwin", "linux"} {
		d := detectBrowser(context.Background(), goos)
		if !d.Present || d.Kind != BrowserChromium || d.Version != "139.0.7258.5" {
			t.Errorf("%s: got %+v, want chromium 139.0.7258.5", goos, d)
		}
	}
}

// One browser is reported, the first of chrome, chromium and playwright: a
// machine with Chrome and Playwright's cache says chrome.
func TestBrowserKindsAreTriedInOrder(t *testing.T) {
	root, home := browserMachine(t)
	playwright(t, root, filepath.Join(home, "Library", "Caches", "ms-playwright"), "chromium-1243", true)
	if d := detectBrowser(context.Background(), "darwin"); d.Kind != BrowserPlaywright {
		t.Fatalf("Playwright alone: %+v", d)
	}
	macApp(t, root, "/Applications", "Chromium", "150.0.1.2")
	if d := detectBrowser(context.Background(), "darwin"); d.Kind != BrowserChromium {
		t.Errorf("Chromium beside Playwright: %+v, want chromium", d)
	}
	macApp(t, root, "/Applications", "Google Chrome", "155.0.8059.82")
	if d := detectBrowser(context.Background(), "darwin"); d.Kind != BrowserChrome {
		t.Errorf("Chrome beside both: %+v, want chrome", d)
	}
}

// playwright makes one browser directory in a Playwright cache, finished or
// cut off before INSTALLATION_COMPLETE.
func playwright(t *testing.T, root, cache, name string, complete bool) string {
	t.Helper()
	dir := filepath.Join(cache, name)
	if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if complete {
		put(t, root, filepath.Join(dir, "INSTALLATION_COMPLETE"), "", 0o644)
	}
	return dir
}

func TestPlaywrightBrowsers(t *testing.T) {
	t.Run("the newest finished Chromium's version, on macOS", func(t *testing.T) {
		root, home := browserMachine(t)
		cache := filepath.Join(home, "Library", "Caches", "ms-playwright")
		old := playwright(t, root, cache, "chromium-1200", true)
		macApp(t, root, filepath.Join(old, "chrome-mac"), "Chromium", "130.0.1.1")
		newest := playwright(t, root, cache, "chromium-1243", true)
		macApp(t, root, filepath.Join(newest, "chrome-mac-arm64"), "Google Chrome for Testing", "145.0.7632.6")
		// Newer, and cut off: not a browser yet.
		half := playwright(t, root, cache, "chromium-1300", false)
		macApp(t, root, filepath.Join(half, "chrome-mac-arm64"), "Google Chrome for Testing", "999.0.0.1")
		playwright(t, root, cache, "ffmpeg-1011", true)
		d := detectBrowser(context.Background(), "darwin")
		if !d.Present || d.Kind != BrowserPlaywright || d.Version != "145.0.7632.6" || d.Error != "" {
			t.Errorf("got %+v, want playwright 145.0.7632.6", d)
		}
	})
	t.Run("on Linux, under XDG_CACHE_HOME, asked", func(t *testing.T) {
		root, _ := browserMachine(t)
		t.Setenv("XDG_CACHE_HOME", "/xdg")
		dir := playwright(t, root, "/xdg/ms-playwright", "chromium-1243", true)
		put(t, root, filepath.Join(dir, "chrome-linux", "chrome"), answeringVersion("Chromium 145.0.7632.6"), 0o755)
		d := detectBrowser(context.Background(), "linux")
		if !d.Present || d.Kind != BrowserPlaywright || d.Version != "145.0.7632.6" {
			t.Errorf("got %+v, want playwright 145.0.7632.6", d)
		}
	})
	t.Run("PLAYWRIGHT_BROWSERS_PATH, holding only Firefox", func(t *testing.T) {
		root, _ := browserMachine(t)
		t.Setenv("PLAYWRIGHT_BROWSERS_PATH", "/srv/pw")
		playwright(t, root, "/srv/pw", "firefox-1466", true)
		d := detectBrowser(context.Background(), "linux")
		if !d.Present || d.Kind != BrowserPlaywright || d.Version != "" || d.Error != "" {
			t.Errorf("got %+v, want playwright present with no version", d)
		}
	})
	t.Run("only a download cut off", func(t *testing.T) {
		root, home := browserMachine(t)
		playwright(t, root, filepath.Join(home, ".cache", "ms-playwright"), "chromium-1243", false)
		if d := detectBrowser(context.Background(), "linux"); d.Present {
			t.Errorf("got %+v, want absent", d)
		}
	})
	t.Run("PLAYWRIGHT_BROWSERS_PATH=0 is node_modules, not the machine", func(t *testing.T) {
		root, home := browserMachine(t)
		playwright(t, root, filepath.Join(home, ".cache", "ms-playwright"), "chromium-1243", true)
		t.Setenv("PLAYWRIGHT_BROWSERS_PATH", "0")
		if d := detectBrowser(context.Background(), "linux"); d.Present {
			t.Errorf("got %+v, want absent", d)
		}
	})
}

// A browser that will not answer is present and broken, in the runner's own
// words: never what it printed, never where it is.
func TestBrokenBrowserIsReportedWithoutItsOutputOrPath(t *testing.T) {
	root, _ := browserMachine(t)
	put(t, root, "/usr/bin/chromium", "#!/bin/sh\necho 'cannot open /home/alice/.config/chromium hunter2' >&2\nexit 1\n", 0o755)
	d := detectBrowser(context.Background(), "linux")
	if !d.Present || d.Kind != BrowserChromium || d.Error == "" || d.Version != "" {
		t.Fatalf("got %+v, want chromium present with an error", d)
	}
	for _, leak := range []string{"hunter2", "/home/alice", "/usr/bin", root} {
		if strings.Contains(d.Error, leak) {
			t.Errorf("error %q carries %q", d.Error, leak)
		}
	}
	if !strings.Contains(d.Error, "--version") {
		t.Errorf("error %q does not say what to run", d.Error)
	}
}

func TestBrowserPlacesByOS(t *testing.T) {
	env := map[string]string{
		"ProgramFiles":      `C:\Program Files`,
		"ProgramFiles(x86)": `C:\Program Files (x86)`,
		"LOCALAPPDATA":      `C:\Users\alice\AppData\Local`,
	}
	getenv := func(name string) string { return env[name] }
	render := func(goos, home string) string {
		var lines []string
		for _, p := range browserPlaces(goos, home, getenv) {
			where := p.path
			if p.onPATH != "" {
				where = "PATH:" + p.onPATH
			}
			lines = append(lines, p.kind+" "+where)
		}
		return strings.Join(lines, "\n")
	}
	want := map[string]string{
		"darwin": `chrome PATH:google-chrome
chrome PATH:google-chrome-stable
chrome /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
chrome /Users/alice/Applications/Google Chrome.app/Contents/MacOS/Google Chrome
chromium PATH:chromium
chromium PATH:chromium-browser
chromium /Applications/Chromium.app/Contents/MacOS/Chromium
chromium /Users/alice/Applications/Chromium.app/Contents/MacOS/Chromium
playwright /Users/alice/Library/Caches/ms-playwright`,
		"linux": `chrome PATH:google-chrome
chrome PATH:google-chrome-stable
chrome /opt/google/chrome/chrome
chrome /usr/bin/google-chrome
chrome /usr/bin/google-chrome-stable
chromium PATH:chromium
chromium PATH:chromium-browser
chromium /usr/bin/chromium
chromium /usr/bin/chromium-browser
chromium /snap/bin/chromium
playwright /Users/alice/.cache/ms-playwright`,
		"windows": `chrome PATH:chrome
chrome C:\Program Files\Google\Chrome\Application\chrome.exe
chrome C:\Program Files (x86)\Google\Chrome\Application\chrome.exe
chrome C:\Users\alice\AppData\Local\Google\Chrome\Application\chrome.exe
chromium PATH:chromium
chromium C:\Users\alice\AppData\Local\Chromium\Application\chrome.exe
chromium C:\Program Files\Chromium\Application\chrome.exe
playwright C:\Users\alice\AppData\Local\ms-playwright`,
	}
	for goos, w := range want {
		if got := render(goos, "/Users/alice"); got != w {
			t.Errorf("%s:\n%s\nwant:\n%s", goos, got, w)
		}
	}
	// No home and no variables: no place built from either.
	none := func(string) string { return "" }
	for _, goos := range []string{"darwin", "linux", "windows"} {
		for _, p := range browserPlaces(goos, "", none) {
			if p.path != "" && !strings.HasPrefix(p.path, "/") {
				t.Errorf("%s with no home: place %q is relative", goos, p.path)
			}
		}
	}
}

// Chrome's Windows installer keeps each version in a directory named for it,
// beside chrome.exe; the newest is the one chrome.exe is.
func TestWindowsVersionIsTheNewestVersionDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"140.0.7339.80", "141.0.7390.9", "141.0.7390.65", "SetupMetrics", "Dictionaries"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	put(t, dir, "999.0.0.0", "a file, not a version", 0o644)
	if got := windowsVersion(dir); got != "141.0.7390.65" {
		t.Errorf("windowsVersion = %q, want 141.0.7390.65", got)
	}
}

// A shipped yad that set the root would look for browsers somewhere they are
// not and report none on a machine that has one.
func TestOnlyTestsReachTheBrowserRoot(t *testing.T) {
	for _, at := range seamtest.Writes(t, "../..", "BrowserRootForTests") {
		t.Errorf("%s writes BrowserRootForTests; only a test may change it", at)
	}
}
