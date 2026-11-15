package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseReleaseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"v0.2.2", [3]int{0, 2, 2}, true},
		{"1.10.0", [3]int{1, 10, 0}, true},
		{"dev", [3]int{}, false},
		{"v0.2.2-3-gabc1234", [3]int{}, false},
		{"v0.2.2-dirty", [3]int{}, false},
		{"v1.0.0-beta.1", [3]int{}, false},
		{"v1.2", [3]int{}, false},
		{"v1..2", [3]int{}, false},
		{"v+1.2.3", [3]int{}, false},
		{"", [3]int{}, false},
	}
	for _, tt := range tests {
		got, ok := parseReleaseVersion(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseReleaseVersion(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestVersionNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.3", "v0.2.2", true},
		{"v0.10.0", "v0.9.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.2.2", "v0.2.2", false},
		{"v0.2.1", "v0.2.2", false},
		{"0.2.3", "v0.2.2", true},
		{"v0.2.3", "dev", false},
		{"v0.2.3", "v0.2.2-1-gabc", false},
		{"", "v0.2.2", false},
		{"v1.0.0-rc1", "v0.2.2", false},
	}
	for _, tt := range tests {
		if got := versionNewer(tt.latest, tt.current); got != tt.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}

func TestChainlinkReleaseAsset(t *testing.T) {
	if got := chainlinkReleaseAsset("darwin", "arm64"); got != "chainlink-darwin-arm64" {
		t.Errorf("darwin/arm64 = %q", got)
	}
	if got := chainlinkReleaseAsset("windows", "amd64"); got != "chainlink-windows-amd64.exe" {
		t.Errorf("windows/amd64 = %q", got)
	}
}

func setChainlinkVersion(t *testing.T, v string) {
	t.Helper()
	old := chainlinkVersion
	chainlinkVersion = v
	t.Cleanup(func() { chainlinkVersion = old })
}

func TestShouldCheckForUpdates(t *testing.T) {
	oldTerm := updateStderrIsTerm
	t.Cleanup(func() { updateStderrIsTerm = oldTerm })
	isTerm := true
	updateStderrIsTerm = func() bool { return isTerm }
	t.Setenv("CI", "")
	t.Setenv("CHAINLINK_NO_UPDATE_CHECK", "")

	setChainlinkVersion(t, "v0.2.2")
	if !shouldCheckForUpdates("install") {
		t.Error("release build on a terminal should check")
	}
	for _, cmd := range []string{"update", "version", "--version", "self-install", "sourcemap"} {
		if shouldCheckForUpdates(cmd) {
			t.Errorf("%s should not check", cmd)
		}
	}

	isTerm = false
	if shouldCheckForUpdates("install") {
		t.Error("should not check when stderr is not a terminal")
	}
	isTerm = true

	t.Setenv("CI", "true")
	if shouldCheckForUpdates("install") {
		t.Error("should not check in CI")
	}
	t.Setenv("CI", "")

	t.Setenv("CHAINLINK_NO_UPDATE_CHECK", "1")
	if shouldCheckForUpdates("install") {
		t.Error("CHAINLINK_NO_UPDATE_CHECK should disable the check")
	}
	t.Setenv("CHAINLINK_NO_UPDATE_CHECK", "")

	setChainlinkVersion(t, "v0.2.2-4-g1234abc-dirty")
	if shouldCheckForUpdates("install") {
		t.Error("development builds should not check")
	}
}

// fakeChainlinkReleases serves /releases/latest for the ChainLink repo plus
// asset downloads.
type fakeChainlinkReleases struct {
	srv       *httptest.Server
	tag       string
	files     map[string][]byte
	fail      atomic.Bool
	apiCalls  atomic.Int64
	downloads atomic.Int64
}

func newFakeChainlinkReleases(t *testing.T, tag string) *fakeChainlinkReleases {
	t.Helper()
	f := &fakeChainlinkReleases{tag: tag, files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/"+chainlinkRepo+"/releases/latest":
			f.apiCalls.Add(1)
			if f.fail.Load() {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			rel := GitHubRelease{TagName: f.tag}
			for name := range f.files {
				rel.Assets = append(rel.Assets, GitHubAsset{Name: name, BrowserDownloadURL: f.srv.URL + "/dl/" + name})
			}
			json.NewEncoder(w).Encode(rel)
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			f.downloads.Add(1)
			body, ok := f.files[strings.TrimPrefix(r.URL.Path, "/dl/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	oldBase := githubAPIBase
	githubAPIBase = f.srv.URL
	t.Cleanup(func() { githubAPIBase = oldBase })
	return f
}

func TestUpdateCheckNotifiesAndCaches(t *testing.T) {
	home, _ := installTestHome(t)
	setChainlinkVersion(t, "v0.2.2")
	f := newFakeChainlinkReleases(t, "v0.3.0")

	var out bytes.Buffer
	startUpdateCheck(home).notify(&out, true)
	if !strings.Contains(out.String(), "v0.3.0") || !strings.Contains(out.String(), "chainlink update") {
		t.Errorf("notice = %q, want it to mention v0.3.0 and chainlink update", out.String())
	}
	if got := readUpdateCache(home).Latest; got != "v0.3.0" {
		t.Errorf("cached latest = %q, want v0.3.0", got)
	}

	// A fresh cache answers without asking GitHub again.
	out.Reset()
	startUpdateCheck(home).notify(&out, true)
	if f.apiCalls.Load() != 1 {
		t.Errorf("made %d API calls, want 1", f.apiCalls.Load())
	}
	if !strings.Contains(out.String(), "v0.3.0") {
		t.Errorf("cached notice = %q", out.String())
	}

	// Once updated, the notice goes away.
	setChainlinkVersion(t, "v0.3.0")
	out.Reset()
	startUpdateCheck(home).notify(&out, true)
	if out.Len() != 0 {
		t.Errorf("up-to-date notice = %q, want none", out.String())
	}
}

func TestUpdateCheckFailureKeepsCacheAndThrottles(t *testing.T) {
	home, _ := installTestHome(t)
	setChainlinkVersion(t, "v0.2.2")
	f := newFakeChainlinkReleases(t, "v0.3.0")
	f.fail.Store(true)

	old := updateCache{CheckedAt: time.Now().Add(-48 * time.Hour), Latest: "v0.2.5"}
	if err := writeUpdateCache(home, old); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	startUpdateCheck(home).notify(&out, true)
	if !strings.Contains(out.String(), "v0.2.5") {
		t.Errorf("notice = %q, want the previously known v0.2.5", out.String())
	}
	c := readUpdateCache(home)
	if c.Latest != "v0.2.5" || time.Since(c.CheckedAt) > time.Minute {
		t.Errorf("cache after failed check = %+v", c)
	}

	startUpdateCheck(home).notify(&out, true)
	if f.apiCalls.Load() != 1 {
		t.Errorf("made %d API calls after a failed check, want 1", f.apiCalls.Load())
	}
}

func TestNilUpdateNotifier(t *testing.T) {
	var n *updateNotifier
	var out bytes.Buffer
	n.notify(&out, true)
	if out.Len() != 0 {
		t.Errorf("nil notifier wrote %q", out.String())
	}
}

func updateTestSetup(t *testing.T, current, latest string) (home string, f *fakeChainlinkReleases, selfInstalls *[]string) {
	t.Helper()
	home, _ = installTestHome(t)
	setChainlinkVersion(t, current)
	f = newFakeChainlinkReleases(t, latest)
	f.files[chainlinkReleaseAsset(runtime.GOOS, runtime.GOARCH)] = []byte("#!/bin/sh\necho " + latest + "\n")
	f.files["chainlink-plan9-mips"] = []byte("wrong platform")

	var calls []string
	oldRun := updateRunSelfInstall
	updateRunSelfInstall = func(bin string) error {
		calls = append(calls, bin)
		return nil
	}
	t.Cleanup(func() { updateRunSelfInstall = oldRun })
	return home, f, &calls
}

func TestHandleUpdateInstallsLatest(t *testing.T) {
	home, f, selfInstalls := updateTestSetup(t, "v0.2.2", "v0.3.0")

	if err := handleUpdate(nil); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}
	dest := filepath.Join(home, ".chainlink", "bin", chainlinkBinaryName(runtime.GOOS))
	data, err := os.ReadFile(dest)
	if err != nil || !strings.Contains(string(data), "v0.3.0") {
		t.Fatalf("installed binary = %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(dest); info.Mode()&0111 == 0 {
			t.Errorf("installed binary mode = %v, want executable", info.Mode())
		}
	}
	if len(*selfInstalls) != 1 || (*selfInstalls)[0] != dest {
		t.Errorf("self-install runs = %v, want [%s]", *selfInstalls, dest)
	}
	if got := readUpdateCache(home).Latest; got != "v0.3.0" {
		t.Errorf("cached latest = %q", got)
	}
	if f.downloads.Load() != 1 {
		t.Errorf("downloads = %d, want 1", f.downloads.Load())
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".chainlink", ".staging")); len(entries) != 0 {
		t.Errorf("staging dir not cleaned up: %v", entries)
	}
}

func TestHandleUpdateAlreadyLatest(t *testing.T) {
	home, f, selfInstalls := updateTestSetup(t, "v0.3.0", "v0.3.0")
	if err := handleUpdate(nil); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}
	if f.downloads.Load() != 0 || len(*selfInstalls) != 0 {
		t.Errorf("downloads = %d, self-installs = %v; want none", f.downloads.Load(), *selfInstalls)
	}
	if _, err := os.Stat(filepath.Join(home, ".chainlink", "bin", chainlinkBinaryName(runtime.GOOS))); err == nil {
		t.Error("binary was written although already up to date")
	}

	// --force reinstalls anyway.
	if err := handleUpdate([]string{"--force"}); err != nil {
		t.Fatalf("handleUpdate --force: %v", err)
	}
	if f.downloads.Load() != 1 {
		t.Errorf("--force downloads = %d, want 1", f.downloads.Load())
	}
}

func TestHandleUpdateCheckOnly(t *testing.T) {
	_, f, selfInstalls := updateTestSetup(t, "v0.2.2", "v0.3.0")
	if err := handleUpdate([]string{"--check"}); err != nil {
		t.Fatalf("handleUpdate --check: %v", err)
	}
	if f.downloads.Load() != 0 || len(*selfInstalls) != 0 {
		t.Errorf("--check downloaded %d files and ran self-install %v times", f.downloads.Load(), *selfInstalls)
	}
}

func TestHandleUpdateRejectsBadDownloads(t *testing.T) {
	home, f, _ := updateTestSetup(t, "v0.2.2", "v0.3.0")
	dest := filepath.Join(home, ".chainlink", "bin", chainlinkBinaryName(runtime.GOOS))

	f.files[chainlinkReleaseAsset(runtime.GOOS, runtime.GOARCH)] = []byte("<html>not a binary</html>")
	if err := handleUpdate(nil); err == nil || !strings.Contains(err.Error(), "not an executable") {
		t.Errorf("non-binary download: err = %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("non-binary download was installed")
	}

	delete(f.files, chainlinkReleaseAsset(runtime.GOOS, runtime.GOARCH))
	if err := handleUpdate(nil); err == nil || !strings.Contains(err.Error(), "has no") {
		t.Errorf("missing platform asset: err = %v", err)
	}

	f.fail.Store(true)
	if err := handleUpdate(nil); err == nil {
		t.Error("API failure: want error")
	}
}

func TestHandleUpdateRejectsArgs(t *testing.T) {
	if err := handleUpdate([]string{"v1.0.0"}); err == nil {
		t.Error("want error for positional argument")
	}
}
