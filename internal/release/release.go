// Package release queries GitHub for the latest published viti-kubevirt release
// and compares it against the locally installed build. It backs the
// `viti nhn version --check` flag and the `viti nhn upgrade` command.
//
// vitictl has an equivalent package, but it talks to GitHub anonymously.
// That does not work here: vitistack/vitictl-kubevirt is private, and GitHub
// answers unauthenticated requests for private resources with 404 rather
// than 403, so an unauthenticated lookup is indistinguishable from "this
// repository has no releases". Requests are therefore authenticated
// whenever a token can be found.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub owner/name that hosts viti-kubevirt releases.
const Repo = "vitistack/vitictl-kubevirt"

// PluginName is how vitictl's plugin manager knows this plugin: the binary
// is viti-kubevirt, so viti exposes it as "nhn".
const PluginName = "kubevirt"

// DefaultTimeout bounds the GitHub API lookup so `viti nhn version --check`
// cannot hang a terminal on a slow network.
const DefaultTimeout = 5 * time.Second

// maxBody caps the response we are willing to read from GitHub.
const maxBody = 1 << 20

// githubAPIBase is a variable rather than a constant so tests can point it
// at a local server.
var githubAPIBase = "https://api.github.com"

// Latest describes a single GitHub release entry.
type Latest struct {
	Tag  string `json:"tag_name"`
	Name string `json:"name"`
	URL  string `json:"html_url"`
	Body string `json:"body"`
}

// FetchLatest returns the newest published release of repo, which is
// expected to be "owner/name". Errors are phrased for a human: a 404 in
// particular is ambiguous on GitHub, so the message depends on whether a
// token was in play.
func FetchLatest(ctx context.Context, repo string) (*Latest, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}

	endpoint := fmt.Sprintf("%s/repos/%s/releases/latest", githubAPIBase, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	tok := Token()
	if tok != "" && sameHostAsAPI(endpoint) {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, describeError(resp.StatusCode, resp.Status, repo, tok != "")
	}

	var out Latest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding GitHub response: %w", err)
	}
	if out.Tag == "" {
		return nil, errors.New("github API response missing tag_name")
	}
	return &out, nil
}

// Token returns a GitHub token for authenticating release lookups, or ""
// when none is configured.
//
// The lookup order matches the gh CLI's own: GH_TOKEN, then GITHUB_TOKEN,
// then whatever `gh auth token` reports — which is what most developers
// actually have set up. It matches vitictl's plugin installer too, so one
// working credential covers install, upgrade, and this check.
func Token() string {
	for _, env := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	// #nosec G204 -- gh is resolved from PATH and invoked with fixed arguments.
	out, err := exec.Command(gh, "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// sameHostAsAPI reports whether rawURL is the API host this package was
// configured with, so the user's token cannot leak to anywhere else. Today
// every request is built from githubAPIBase and so always qualifies; the
// check is here so that stays true if the base ever becomes configurable
// (a GitHub Enterprise host, say).
func sameHostAsAPI(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	base, err := url.Parse(githubAPIBase)
	if err != nil || base.Hostname() == "" {
		return false
	}
	return strings.EqualFold(u.Hostname(), base.Hostname())
}

// describeError turns a failed release lookup into something actionable.
// GitHub returns 404 both for a repository with no releases and for one the
// caller cannot see, so without a token the advice is to get one.
func describeError(code int, status, repo string, haveToken bool) error {
	switch {
	case code == http.StatusNotFound && !haveToken:
		return fmt.Errorf(
			"github API returned 404 for %s — the repository is private, or it has no releases yet. "+
				"Authenticate first: set GH_TOKEN (or GITHUB_TOKEN), or run 'gh auth login'",
			repo)
	case code == http.StatusNotFound:
		return fmt.Errorf(
			"github API returned 404 for %s — no releases found. "+
				"Check that your token can read the repository",
			repo)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf(
			"github API returned %s for %s — the token was rejected or lacks access; "+
				"re-authenticate with 'gh auth login' or refresh GH_TOKEN",
			status, repo)
	default:
		return fmt.Errorf("github API returned %s for %s", status, repo)
	}
}

// Status classifies the result of comparing a local version against the
// latest release tag.
type Status int

const (
	// StatusUpToDate means local and latest point at the same release tag.
	StatusUpToDate Status = iota
	// StatusOutdated means latest is newer than the local build.
	StatusOutdated
	// StatusDevelopment means the local build is a dev or pre-release build
	// (e.g. "dev", or a git-describe tag like "v1.2.3-5-gabc1234") and we
	// cannot meaningfully say it is "out of date".
	StatusDevelopment
	// StatusAhead means the local build's semver is newer than the latest
	// published release — typical for unreleased main builds.
	StatusAhead
)

// Compare classifies the relationship between the locally installed version
// string and a GitHub release tag.
func Compare(local, latestTag string) Status {
	local = strings.TrimSpace(local)
	latestTag = strings.TrimSpace(latestTag)

	if local == "" || local == "dev" || local == "(devel)" {
		return StatusDevelopment
	}
	if local == latestTag || strings.TrimPrefix(local, "v") == strings.TrimPrefix(latestTag, "v") {
		return StatusUpToDate
	}

	lv, lok := parseSemver(local)
	rv, rok := parseSemver(latestTag)
	if !lok || !rok {
		// Fall back: if they aren't equal and we can't parse, treat as
		// outdated so the user at least sees the release pointer.
		return StatusOutdated
	}

	switch cmp := compareSemver(lv, rv); {
	case cmp < 0:
		return StatusOutdated
	case cmp > 0:
		return StatusAhead
	default:
		// Same X.Y.Z but strings differ — typically a git-describe suffix
		// like "v1.2.3-5-gabc1234" on the local build.
		if local != latestTag {
			return StatusDevelopment
		}
		return StatusUpToDate
	}
}

type semver struct {
	major, minor, patch int
}

// parseSemver extracts the leading X.Y.Z numeric components from a tag like
// "v1.2.3", "1.2.3", or "v1.2.3-5-gabc1234". Anything after the third
// numeric segment is ignored on purpose — only the release portion is
// compared.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return semver{}, false
	}
	// Cut at the first non-numeric / non-dot character so suffixes like
	// "-5-gabc1234" or "-rc1" don't break parsing.
	end := len(s)
	for i, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			end = i
			break
		}
	}
	parts := strings.Split(s[:end], ".")
	if len(parts) < 3 {
		return semver{}, false
	}
	var v semver
	var err error
	if v.major, err = strconv.Atoi(parts[0]); err != nil {
		return semver{}, false
	}
	if v.minor, err = strconv.Atoi(parts[1]); err != nil {
		return semver{}, false
	}
	if v.patch, err = strconv.Atoi(parts[2]); err != nil {
		return semver{}, false
	}
	return v, true
}

func compareSemver(a, b semver) int {
	if a.major != b.major {
		return a.major - b.major
	}
	if a.minor != b.minor {
		return a.minor - b.minor
	}
	return a.patch - b.patch
}

// UpgradeHint returns the command a user runs to upgrade the plugin.
//
// viti-kubevirt deliberately ships no installer script of its own. It is
// distributed as a viti plugin, and `viti plugin upgrade` already resolves
// the release, verifies its SHA-256 checksum and Sigstore signature, and
// replaces the binary atomically — reimplementing that here would mean two
// copies of the same security-critical code.
func UpgradeHint() string {
	return "viti plugin upgrade " + PluginName
}

// ReleasesURL returns the human-readable releases page for Repo.
func ReleasesURL() string {
	return fmt.Sprintf("https://github.com/%s/releases", Repo)
}
