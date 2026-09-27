package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub answers the three URLs a release is read from, the way github.com
// answers them without a login, and records every request. What is worth
// proving is the paths: a wrong asset name or a dropped repository is a
// failure only a real release would otherwise show.
type fakeGitHub struct {
	repo   string
	tag    string // "" is a repository that has published nothing
	assets map[string]string
	status int // answers every asset download with this, when set

	mu   sync.Mutex
	seen []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	prefix := "/" + f.repo + "/releases/"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case rest == "latest" && f.tag == "":
		http.Redirect(w, r, "/"+f.repo+"/releases", http.StatusFound)
	case rest == "latest":
		http.Redirect(w, r, prefix+"tag/"+f.tag, http.StatusFound)
	case rest == "tag/"+f.tag && f.tag != "":
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(rest, "download/"+f.tag+"/") && f.tag != "":
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		body, ok := f.assets[strings.TrimPrefix(rest, "download/"+f.tag+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) requests() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.seen, "\n")
}

func serve(t *testing.T, f *fakeGitHub) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestGitHubLatest(t *testing.T) {
	f := &fakeGitHub{repo: DefaultRepo, tag: "v0.4.0"}
	base := serve(t, f)

	tag, err := GitHub{BaseURL: base}.Latest(context.Background())
	if err != nil || tag != "v0.4.0" {
		t.Fatalf("Latest = %q, %v, want v0.4.0", tag, err)
	}
	if !strings.Contains(f.requests(), "GET /"+DefaultRepo+"/releases/latest") {
		t.Errorf("requests were %q, want the default repository's latest release", f.requests())
	}
}

// GitHub redirects a repository with no releases to its releases page, which
// is a state to name rather than an error to pass through.
func TestGitHubLatestSaysWhenThereAreNoReleases(t *testing.T) {
	base := serve(t, &fakeGitHub{repo: DefaultRepo})

	_, err := GitHub{BaseURL: base}.Latest(context.Background())
	if err == nil {
		t.Fatal("Latest invented a release")
	}
	if !strings.Contains(err.Error(), "published no release yet") {
		t.Errorf("error %q, want it to say the repository has no releases", err)
	}
}

// A private fork answers 404 without a login, exactly as a mistyped one does,
// and the operator has to be told both are possible.
func TestGitHubLatestSaysWhenTheRepositoryIsNotThere(t *testing.T) {
	base := serve(t, &fakeGitHub{repo: DefaultRepo, tag: "v0.4.0"})

	_, err := GitHub{Repo: "someone/private-fork", BaseURL: base}.Latest(context.Background())
	if err == nil {
		t.Fatal("Latest reported success on a repository that is not there")
	}
	for _, want := range []string{"someone/private-fork", "private", RepoEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q, want it to carry %q", err, want)
		}
	}
}

func TestGitHubSaysWhenItCannotConnect(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	_, err := GitHub{BaseURL: srv.URL}.Latest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Errorf("error %v, want it to say the server could not be reached", err)
	}
}

func TestGitHubDownloadFetchesEachAsset(t *testing.T) {
	f := &fakeGitHub{repo: "someone/fork", tag: "v0.4.0", assets: map[string]string{"yad-linux-amd64": "a binary", ChecksumsName: "sums"}}
	base := serve(t, f)
	dir := t.TempDir()

	if err := (GitHub{Repo: "someone/fork", BaseURL: base}).Download(context.Background(), "v0.4.0", []string{"yad-linux-amd64", ChecksumsName}, dir); err != nil {
		t.Fatalf("Download: %v", err)
	}
	for name, body := range f.assets {
		if got := read(t, filepath.Join(dir, name)); got != body {
			t.Errorf("%s holds %q, want %q", name, got, body)
		}
	}
	for _, want := range []string{
		"GET /someone/fork/releases/download/v0.4.0/yad-linux-amd64",
		"GET /someone/fork/releases/download/v0.4.0/" + ChecksumsName,
	} {
		if !strings.Contains(f.requests(), want) {
			t.Errorf("requests were %q, want %q", f.requests(), want)
		}
	}
}

// Apply names an incomplete release by the asset it lacks, so the download
// leaves that asset out instead of failing on it.
func TestGitHubDownloadLeavesOutAMissingAsset(t *testing.T) {
	base := serve(t, &fakeGitHub{repo: DefaultRepo, tag: "v0.4.0", assets: map[string]string{"yad-linux-amd64": "a binary"}})
	dir := t.TempDir()

	if err := (GitHub{BaseURL: base}).Download(context.Background(), "v0.4.0", []string{"yad-linux-amd64", ChecksumsName}, dir); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ChecksumsName)); err == nil {
		t.Errorf("%s was written for a release that does not carry it", ChecksumsName)
	}
}

// Every asset of a tag that does not exist is missing too, and reported as an
// incomplete release that would send the operator after the wrong problem.
func TestGitHubDownloadSaysWhenTheTagIsMissing(t *testing.T) {
	f := &fakeGitHub{repo: DefaultRepo, tag: "v0.4.0"}
	base := serve(t, f)

	err := GitHub{BaseURL: base}.Download(context.Background(), "v9.9.9", []string{"yad-linux-amd64"}, t.TempDir())
	if err == nil {
		t.Fatal("Download reported success for a release that is not there")
	}
	if !strings.Contains(err.Error(), "has no release v9.9.9") {
		t.Errorf("error %q, want it to name the tag as the problem", err)
	}
	if strings.Contains(f.requests(), "/download/") {
		t.Errorf("requests were %q — it asked for assets of a release that does not exist", f.requests())
	}
}

func TestGitHubDownloadReportsAFailingServer(t *testing.T) {
	base := serve(t, &fakeGitHub{repo: DefaultRepo, tag: "v0.4.0", status: http.StatusBadGateway})

	err := GitHub{BaseURL: base}.Download(context.Background(), "v0.4.0", []string{"yad-linux-amd64"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("error %v, want the server's answer", err)
	}
}
