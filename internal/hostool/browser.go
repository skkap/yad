package hostool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/shellword"
	"github.com/skkap/yad/internal/supervise"
)

// BrowserID is the browser's id in host_tools.
const BrowserID = "browser"

// The kinds a browser is reported as, most specific first: the order a
// machine with several is searched in, and the one reported is the first
// found (decision 0087).
const (
	BrowserChrome     = "chrome"
	BrowserChromium   = "chromium"
	BrowserPlaywright = "playwright"
)

// BrowserRootForTests, while set, is where every standard location is looked
// for, as though it were the root of the filesystem: a test machine's own
// Chrome under /Applications, or its Playwright cache under the home, must not
// decide what a test sees. PATH is the test's to set already. Nothing outside
// a test may set it: TestOnlyTestsReachTheBrowserRoot fails on any shipped
// file of the module that assigns it.
var BrowserRootForTests string

// browserPlace is one place a browser may be: a name looked up on PATH, or a
// file at a fixed location.
type browserPlace struct {
	kind string
	// onPATH is a name for exec.LookPath; path is a file. Exactly one is set.
	onPATH, path string
}

// browserPlaces is every place a browser is looked for on goos, in the order
// they are tried. Pure, so each OS's list is tested on any OS; getenv is the
// environment a run inherits (supervise.LookupEnv), so a LOCALAPPDATA or
// PLAYWRIGHT_BROWSERS_PATH declared in [env] is the one searched.
//
// Chrome and Chromium only, as the ticket that added them decided (YAD-201):
// they are what a headless screenshot and Playwright's own `chrome` channel
// drive. Edge, Brave and the rest are Chromium too, and none of them is looked
// for until a hub has a reason to want one.
func browserPlaces(goos, home string, getenv func(string) string) []browserPlace {
	var out []browserPlace
	add := func(kind string, names []string, paths ...string) {
		for _, n := range names {
			out = append(out, browserPlace{kind: kind, onPATH: n})
		}
		for _, p := range paths {
			if p != "" {
				out = append(out, browserPlace{kind: kind, path: p})
			}
		}
	}
	switch goos {
	case "darwin":
		app := func(dir, name string) string {
			if dir == "" {
				return ""
			}
			return filepath.Join(dir, name+".app", "Contents", "MacOS", name)
		}
		userApps := ""
		if home != "" {
			userApps = filepath.Join(home, "Applications")
		}
		add(BrowserChrome, []string{"google-chrome", "google-chrome-stable"},
			app("/Applications", "Google Chrome"), app(userApps, "Google Chrome"))
		add(BrowserChromium, []string{"chromium", "chromium-browser"},
			app("/Applications", "Chromium"), app(userApps, "Chromium"))
	case "windows":
		// Joined by hand: these are tested on every OS, where filepath would
		// join with the wrong separator.
		win := func(base string, rest ...string) string {
			if base == "" {
				return ""
			}
			return strings.Join(append([]string{strings.TrimRight(base, `\`)}, rest...), `\`)
		}
		pf, pf86, local := getenv("ProgramFiles"), getenv("ProgramFiles(x86)"), getenv("LOCALAPPDATA")
		add(BrowserChrome, []string{"chrome"},
			win(pf, "Google", "Chrome", "Application", "chrome.exe"),
			win(pf86, "Google", "Chrome", "Application", "chrome.exe"),
			win(local, "Google", "Chrome", "Application", "chrome.exe"))
		add(BrowserChromium, []string{"chromium"},
			win(local, "Chromium", "Application", "chrome.exe"),
			win(pf, "Chromium", "Application", "chrome.exe"))
	default:
		// Linux and the other unixes. The fixed paths are what a daemon a
		// service manager started with a bare PATH still finds.
		add(BrowserChrome, []string{"google-chrome", "google-chrome-stable"},
			"/opt/google/chrome/chrome", "/usr/bin/google-chrome", "/usr/bin/google-chrome-stable")
		add(BrowserChromium, []string{"chromium", "chromium-browser"},
			"/usr/bin/chromium", "/usr/bin/chromium-browser", "/snap/bin/chromium")
	}
	if dir := playwrightDir(goos, home, getenv); dir != "" {
		out = append(out, browserPlace{kind: BrowserPlaywright, path: dir})
	}
	return out
}

// playwrightDir is where Playwright keeps the browsers it installs:
// PLAYWRIGHT_BROWSERS_PATH, or its per-OS cache. "0" is Playwright's own word
// for "inside the project's node_modules", which is a workdir's business and
// not the machine's.
func playwrightDir(goos, home string, getenv func(string) string) string {
	if v := getenv("PLAYWRIGHT_BROWSERS_PATH"); v != "" {
		if v == "0" {
			return ""
		}
		return v
	}
	switch goos {
	case "darwin":
		if home != "" {
			return filepath.Join(home, "Library", "Caches", "ms-playwright")
		}
	case "windows":
		if local := getenv("LOCALAPPDATA"); local != "" {
			return strings.TrimRight(local, `\`) + `\ms-playwright`
		}
	default:
		if xdg := getenv("XDG_CACHE_HOME"); xdg != "" {
			return filepath.Join(xdg, "ms-playwright")
		}
		if home != "" {
			return filepath.Join(home, ".cache", "ms-playwright")
		}
	}
	return ""
}

// rooted is path as BrowserRootForTests sees it.
func rooted(path string) string {
	if BrowserRootForTests == "" {
		return path
	}
	return filepath.Join(BrowserRootForTests, path)
}

// detectBrowser reports the first browser found among browserPlaces.
//
// Most of it is stat: a version is read from the app bundle on macOS and from
// the install's version directory on Windows, and only a browser on Linux, or
// one found by name on PATH, is asked `--version` — one spawn at most, where
// each of git, gh and docker costs one or two. Chrome on macOS takes seconds
// to answer `--version`, which is most of the probe's five, and it reads the
// same number the bundle states.
//
// No display is probed: what a run does with a browser here is headless, and
// headless needs none.
func detectBrowser(ctx context.Context, goos string) Detected {
	d := Detected{ID: BrowserID}
	home, _ := os.UserHomeDir()
	getenv := func(name string) string { v, _ := supervise.LookupEnv(name); return v }
	for _, p := range browserPlaces(goos, home, getenv) {
		switch {
		case p.kind == BrowserPlaywright:
			if chromium, ok := playwrightBrowsers(rooted(p.path)); ok {
				d.Present, d.Kind, d.Path = true, p.kind, p.path
				if chromium != "" {
					d.Version, d.Error = playwrightVersion(ctx, chromium, goos)
				}
				return d
			}
		case p.onPATH != "":
			path, err := exec.LookPath(p.onPATH)
			if err != nil {
				continue
			}
			d.Present, d.Kind, d.Path = true, p.kind, path
			d.Version, d.Error = askVersion(ctx, path, shellword.Command(p.onPATH, "--version"))
			return d
		default:
			path := rooted(p.path)
			if !isFile(path) {
				continue
			}
			d.Present, d.Kind, d.Path = true, p.kind, p.path
			switch goos {
			case "darwin":
				d.Version = bundleVersion(path)
			case "windows":
				d.Version = windowsVersion(filepath.Dir(path))
			default:
				// Named in full: a fixed location is the system's, never
				// under anyone's home, and not on PATH to be named by.
				d.Version, d.Error = askVersion(ctx, path, shellword.Command(p.path, "--version"))
			}
			return d
		}
	}
	return d
}

// askVersion runs path --version. command is that, as its owner would paste
// it (shellword.Command): the name PATH found, or a fixed location in full.
// Never what the browser printed (internal/probe).
func askVersion(ctx context.Context, path, command string) (version, problem string) {
	out, err := run(ctx, path, []string{"--version"}, false, versionWait())
	switch {
	case err != nil:
		return "", "the browser found will not start — run `" + command + "` on this machine to see why"
	case out.TimedOut:
		return "", fmt.Sprintf("no answer to `%s` within %s — run it on this machine to see what it waits on", command, versionWait())
	case out.Err != nil:
		return "", "`" + command + "` exited with an error — run it on this machine to see why"
	}
	return harness.ParseVersion(string(out.Stdout)), ""
}

// isFile is a regular file: not a directory, and not a FIFO or a device,
// which a read would block on or never finish.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// plistVersion is the bundle's CFBundleShortVersionString. Chrome's and
// Chromium's Info.plist are XML, so a pattern over the text is enough, and
// what it matches is only ever digits and dots.
var plistVersion = regexp.MustCompile(`<key>CFBundleShortVersionString</key>\s*<string>(\d+(?:\.\d+){1,3})</string>`)

// bundleVersion reads the version of the app whose executable is exe:
// <name>.app/Contents/MacOS/<name>, beside Contents/Info.plist.
func bundleVersion(exe string) string {
	b, err := readCapped(filepath.Join(filepath.Dir(filepath.Dir(exe)), "Info.plist"))
	if err != nil {
		return ""
	}
	if m := plistVersion.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// readCapped reads at most outputCap bytes: an Info.plist is a few kilobytes,
// and a file that is not one must not grow the runner.
func readCapped(path string) ([]byte, error) {
	// Checked before the open, which would block on a FIFO with no writer
	// and hold every capability probe with it.
	if !isFile(path) {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, outputCap)
	n, err := f.Read(b)
	if n == 0 && err != nil {
		return nil, err
	}
	return b[:n], nil
}

var dottedVersion = regexp.MustCompile(`^\d+(?:\.\d+){1,3}$`)

// windowsVersion is the newest version-named directory beside chrome.exe,
// which is how Chrome's installer lays out Application: chrome.exe on
// Windows opens a window rather than answer --version.
func windowsVersion(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range entries {
		if e.IsDir() && dottedVersion.MatchString(e.Name()) && (best == "" || versionLess(best, e.Name())) {
			best = e.Name()
		}
	}
	return best
}

func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			return x < y
		}
	}
	return len(as) < len(bs)
}

// playwrightEntry is one of Playwright's browser directories: chromium-1243,
// chromium_headless_shell-1243, firefox-1466, webkit-2359. ffmpeg-N and the
// rest are not browsers.
var playwrightEntry = regexp.MustCompile(`^(chromium|chromium_headless_shell|firefox|webkit)-(\d+)$`)

// playwrightBrowsers says whether dir holds a browser Playwright finished
// installing — it writes INSTALLATION_COMPLETE last, so a download cut off
// half-way is not one — and the directory of its newest full Chromium, if it
// has one. A cache of only Firefox or WebKit is still Playwright's browsers,
// reported without a version.
func playwrightBrowsers(dir string) (chromium string, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	newest := -1
	for _, e := range entries {
		m := playwrightEntry.FindStringSubmatch(e.Name())
		if m == nil || !e.IsDir() || !isFile(filepath.Join(dir, e.Name(), "INSTALLATION_COMPLETE")) {
			continue
		}
		ok = true
		if rev, _ := strconv.Atoi(m[2]); m[1] == "chromium" && rev > newest {
			newest, chromium = rev, filepath.Join(dir, e.Name())
		}
	}
	return chromium, ok
}

// playwrightVersion is the version of the Chromium Playwright installed in
// dir, read as an installed Chrome's is: from the bundle on macOS, from the
// binary on Linux. On Windows it is not read: Playwright's build has no
// version directory, and chrome.exe answers --version with a window.
//
// A Linux Chromium that will not answer is a problem, not a missing version:
// Playwright's download without --with-deps lacks the system libraries it
// loads before main, so it cannot answer --version and cannot open a page
// either, and a hub told it is present with no error would send it the
// verifier run that then fails. The advice names Playwright's command rather
// than the binary, whose path is under the owner's home.
func playwrightVersion(ctx context.Context, dir, goos string) (version, problem string) {
	switch goos {
	case "darwin":
		// chrome-mac/ or chrome-mac-arm64/, holding Chromium.app or, since
		// Playwright moved to Chrome for Testing, "Google Chrome for Testing.app".
		plists, _ := filepath.Glob(filepath.Join(dir, "chrome-mac*", "*.app", "Contents", "Info.plist"))
		slices.Sort(plists)
		for _, p := range plists {
			if v := bundleVersion(filepath.Join(filepath.Dir(p), "MacOS", "x")); v != "" {
				return v, ""
			}
		}
	case "windows":
	default:
		bins, _ := filepath.Glob(filepath.Join(dir, "chrome-linux*", "chrome"))
		slices.Sort(bins)
		for _, b := range bins {
			if !isFile(b) {
				continue
			}
			out, err := run(ctx, b, []string{"--version"}, false, versionWait())
			if err != nil || out.TimedOut || out.Err != nil {
				return "", "Playwright's newest Chromium will not run on this machine — usually it lacks the system libraries it needs, which `sudo npx playwright install-deps chromium` installs"
			}
			return harness.ParseVersion(string(out.Stdout)), ""
		}
	}
	return "", ""
}
