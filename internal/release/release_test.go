package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withAPI points the package at a local server and blanks every credential
// source, so a developer's real gh login can never leak into a test.
func withAPI(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })

	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	// Token() falls back to `gh auth token`. An empty PATH makes that lookup
	// fail, so a test that means "unauthenticated" really is unauthenticated
	// even on a machine where gh is logged in.
	t.Setenv("PATH", t.TempDir())
}

func serveJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const latestBody = `{"tag_name":"v1.2.3","name":"v1.2.3","html_url":"https://example.test/releases/v1.2.3"}`

func TestFetchLatestReturnsTheRelease(t *testing.T) {
	withAPI(t, serveJSON(http.StatusOK, latestBody))

	got, err := FetchLatest(context.Background(), Repo)
	if err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if got.Tag != "v1.2.3" {
		t.Errorf("Tag = %q, want v1.2.3", got.Tag)
	}
	if got.URL != "https://example.test/releases/v1.2.3" {
		t.Errorf("URL = %q, want the release page", got.URL)
	}
}

func TestFetchLatestRequestsTheLatestReleaseOfTheRepo(t *testing.T) {
	var path string
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(latestBody))
	})

	if _, err := FetchLatest(context.Background(), Repo); err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if want := "/repos/" + Repo + "/releases/latest"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// The private repository is the whole reason this package exists: without a
// token every lookup 404s, so the token must actually be sent.
func TestFetchLatestAuthenticatesWhenATokenIsSet(t *testing.T) {
	var auth string
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(latestBody))
	})
	t.Setenv("GH_TOKEN", "s3cret")

	if _, err := FetchLatest(context.Background(), Repo); err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if auth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer s3cret")
	}
}

func TestFetchLatestPrefersGHTokenOverGitHubToken(t *testing.T) {
	var auth string
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(latestBody))
	})
	t.Setenv("GITHUB_TOKEN", "second")
	t.Setenv("GH_TOKEN", "first")

	if _, err := FetchLatest(context.Background(), Repo); err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if auth != "Bearer first" {
		t.Errorf("Authorization = %q, want GH_TOKEN to win", auth)
	}
}

func TestFetchLatestSendsNoCredentialWhenNoneIsConfigured(t *testing.T) {
	var auth string
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(latestBody))
	})

	if _, err := FetchLatest(context.Background(), Repo); err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if auth != "" {
		t.Errorf("Authorization = %q, want no header", auth)
	}
}

// GitHub returns 404 for a private repository the caller cannot see, so the
// unauthenticated case must say how to authenticate rather than claim there
// are no releases.
func TestFetchLatest404WithoutTokenExplainsHowToAuthenticate(t *testing.T) {
	withAPI(t, serveJSON(http.StatusNotFound, `{"message":"Not Found"}`))

	_, err := FetchLatest(context.Background(), Repo)
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	for _, want := range []string{"GH_TOKEN", "gh auth login", Repo} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestFetchLatest404WithTokenPointsAtAccess(t *testing.T) {
	withAPI(t, serveJSON(http.StatusNotFound, `{"message":"Not Found"}`))
	t.Setenv("GH_TOKEN", "s3cret")

	_, err := FetchLatest(context.Background(), Repo)
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	// With a token in play, telling the user to get a token is useless advice.
	if strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("error %q should not tell an authenticated user to log in", err)
	}
	if !strings.Contains(err.Error(), "read the repository") {
		t.Errorf("error %q should point at repository access", err)
	}
}

func TestFetchLatestRejectedTokenSaysSo(t *testing.T) {
	withAPI(t, serveJSON(http.StatusUnauthorized, `{"message":"Bad credentials"}`))
	t.Setenv("GH_TOKEN", "stale")

	_, err := FetchLatest(context.Background(), Repo)
	if err == nil {
		t.Fatal("expected an error for a 401")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error %q should say the token was rejected", err)
	}
}

func TestFetchLatestMissingTagIsAnError(t *testing.T) {
	withAPI(t, serveJSON(http.StatusOK, `{"name":"no tag here"}`))

	if _, err := FetchLatest(context.Background(), Repo); err == nil {
		t.Fatal("expected an error when the response has no tag_name")
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name   string
		local  string
		latest string
		want   Status
	}{
		{"identical tags", "v1.2.3", "v1.2.3", StatusUpToDate},
		{"v prefix only on one side", "1.2.3", "v1.2.3", StatusUpToDate},
		{"older patch", "v1.2.2", "v1.2.3", StatusOutdated},
		{"older minor", "v1.1.9", "v1.2.0", StatusOutdated},
		{"older major", "v0.9.9", "v1.0.0", StatusOutdated},
		{"newer than published", "v1.3.0", "v1.2.3", StatusAhead},
		{"dev build", "dev", "v1.2.3", StatusDevelopment},
		{"go install pseudo version", "(devel)", "v1.2.3", StatusDevelopment},
		{"empty local", "", "v1.2.3", StatusDevelopment},
		{"git describe on the release commit", "v1.2.3-5-gabc1234", "v1.2.3", StatusDevelopment},
		{"unparseable local", "banana", "v1.2.3", StatusOutdated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compare(tt.local, tt.latest); got != tt.want {
				t.Errorf("Compare(%q, %q) = %v, want %v", tt.local, tt.latest, got, tt.want)
			}
		})
	}
}

// The hint is what the user is told to type, and it has to be a command viti
// actually has. It is not a curl|bash line like vitictl's, because this repo
// ships no installer script.
func TestUpgradeHintNamesThePluginCommand(t *testing.T) {
	if got := UpgradeHint(); got != "viti plugin upgrade kubevirt" {
		t.Errorf("UpgradeHint() = %q, want %q", got, "viti plugin upgrade kubevirt")
	}
}

func TestReleasesURLPointsAtTheRepo(t *testing.T) {
	if want := "https://github.com/" + Repo + "/releases"; ReleasesURL() != want {
		t.Errorf("ReleasesURL() = %q, want %q", ReleasesURL(), want)
	}
}
