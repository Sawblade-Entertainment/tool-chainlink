package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// chainlinkVersion is set at build time with
// -ldflags "-X main.chainlinkVersion=v1.2.3". Local builds get `git describe`
// output, which is not a release version, so they never nag about updates.
var chainlinkVersion = "dev"

// chainlinkRepo is where ChainLink releases are published.
const chainlinkRepo = "Sawblade-Entertainment/tool-chainlink"

const (
	// updateCheckInterval is how often the background check asks GitHub.
	updateCheckInterval = 24 * time.Hour
	// updateCheckTimeout bounds the background check so an offline machine
	// never waits long for a command to finish.
	updateCheckTimeout = 2 * time.Second
)

// Package variables so tests can control them.
var (
	updateNow            = time.Now
	updateCheckClient    = &http.Client{Timeout: updateCheckTimeout}
	updateStderrIsTerm   = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }
	updateRunSelfInstall = func(bin string) error {
		cmd := exec.Command(bin, "self-install")
		cmd.Stdout, cmd.Stderr = uiOut, os.Stderr
		return cmd.Run()
	}
)

// parseReleaseVersion parses "v1.2.3" or "1.2.3". Prereleases, `git describe`
// output such as "v0.2.2-3-gabc1234" and "dev" are not releases.
func parseReleaseVersion(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	for i, p := range parts {
		if p == "" || strings.TrimLeft(p, "0123456789") != "" {
			return [3]int{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

// versionNewer reports whether latest is a release newer than current, which
// must itself be a release.
func versionNewer(latest, current string) bool {
	l, ok1 := parseReleaseVersion(latest)
	c, ok2 := parseReleaseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// chainlinkReleaseAsset is the release asset built for goos/goarch by the
// Makefile, e.g. chainlink-darwin-arm64 or chainlink-windows-amd64.exe.
func chainlinkReleaseAsset(goos, goarch string) string {
	name := "chainlink-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// fetchLatestChainlinkRelease asks GitHub for the newest published (non-draft,
// non-prerelease) ChainLink release.
func fetchLatestChainlinkRelease(client *http.Client) (*GitHubRelease, error) {
	rel, _, err := getReleaseFrom(client, fmt.Sprintf("%s/repos/%s/releases/latest", githubAPIBase, chainlinkRepo))
	if err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, errors.New("latest release has no tag")
	}
	return rel, nil
}

// updateCache remembers the last check so GitHub is asked at most once per
// updateCheckInterval.
type updateCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest,omitempty"`
}

func updateCachePath(home string) string {
	return filepath.Join(home, ".chainlink", "update-check.json")
}

func readUpdateCache(home string) updateCache {
	var c updateCache
	if data, err := os.ReadFile(updateCachePath(home)); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

func writeUpdateCache(home string, c updateCache) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	path := updateCachePath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return replaceFileFrom(path, bytes.NewReader(data), 0644)
}

// shouldCheckForUpdates decides whether running cmd should look for a newer
// release. Development builds, CI, scripts (stderr not a terminal) and
// commands that already deal with versions are left alone.
func shouldCheckForUpdates(cmd string) bool {
	switch cmd {
	case "update", "version", "--version", "self-install", "sourcemap":
		return false
	}
	if _, ok := parseReleaseVersion(chainlinkVersion); !ok {
		return false
	}
	if os.Getenv("CHAINLINK_NO_UPDATE_CHECK") != "" || os.Getenv("CI") != "" {
		return false
	}
	return updateStderrIsTerm()
}

// updateNotifier holds the latest known release and, when the cache was stale,
// a background refresh started alongside the command.
type updateNotifier struct {
	latest string
	done   chan struct{} // closed when the refresh finishes; nil if none
	fresh  string        // written by the refresh before done is closed
}

// startUpdateCheck reads the cached result and, if it is older than
// updateCheckInterval, refreshes it in the background.
func startUpdateCheck(home string) *updateNotifier {
	cache := readUpdateCache(home)
	n := &updateNotifier{latest: cache.Latest}
	age := updateNow().Sub(cache.CheckedAt)
	if age >= 0 && age < updateCheckInterval {
		return n
	}
	n.done = make(chan struct{})
	go func() {
		defer close(n.done)
		next := updateCache{CheckedAt: updateNow(), Latest: cache.Latest}
		// A failed check still counts as a check, so an offline machine
		// doesn't retry on every command.
		if rel, err := fetchLatestChainlinkRelease(updateCheckClient); err == nil {
			next.Latest = rel.TagName
			n.fresh = rel.TagName
		}
		_ = writeUpdateCache(home, next)
	}()
	return n
}

// notify prints a notice to w when a newer release is known. With wait set it
// first lets an in-flight check finish (bounded by updateCheckTimeout);
// otherwise it uses whatever is known now and the check keeps running.
func (n *updateNotifier) notify(w io.Writer, wait bool) {
	if n == nil {
		return
	}
	latest := n.latest
	if n.done != nil {
		if wait {
			<-n.done
		}
		select {
		case <-n.done:
			if n.fresh != "" {
				latest = n.fresh
			}
		default:
		}
	}
	if !versionNewer(latest, chainlinkVersion) {
		return
	}
	fmt.Fprintf(w, "\n%s chainlink %s is available %s\n", statusVerb(colorStderr, "Update", sgrYellow),
		paint(colorStderr, latest, sgrBold), paint(colorStderr, "(you have "+chainlinkVersion+")", sgrDim))
	fmt.Fprintf(w, "%*s Run %s to install it.\n", statusVerbWidth, "", paint(colorStderr, "chainlink update", sgrBold, sgrCyan))
}

// beginUpdateCheck starts the background check for cmd, or returns nil.
func beginUpdateCheck(cmd string) *updateNotifier {
	if !shouldCheckForUpdates(cmd) {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return startUpdateCheck(home)
}

func handleVersion() {
	fmt.Fprintf(uiOut, "chainlink %s\n", chainlinkVersion)
}

// handleUpdate replaces ~/.chainlink/bin/chainlink with the latest release
// binary for this platform, then lets the new binary refresh the Studio plugin.
func handleUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only report whether an update is available")
	force := fs.Bool("force", false, "reinstall even if already on the latest version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	status("Checking", sgrCyan, "for updates %s", dim("(current: "+chainlinkVersion+")"))
	rel, err := fetchLatestChainlinkRelease(installHTTPClient)
	if err != nil {
		return err
	}
	latest := rel.TagName
	_ = writeUpdateCache(home, updateCache{CheckedAt: updateNow(), Latest: latest})

	_, isRelease := parseReleaseVersion(chainlinkVersion)
	newer := !isRelease || versionNewer(latest, chainlinkVersion)
	if !newer && !*force {
		status("Up to date", sgrGreen, "chainlink %s", chainlinkVersion)
		return nil
	}
	if *check {
		if newer {
			status("Available", sgrYellow, "chainlink %s", latest)
			hintf("Run `chainlink update` to install it.")
		} else {
			status("Up to date", sgrGreen, "chainlink %s", chainlinkVersion)
		}
		return nil
	}

	assetName := chainlinkReleaseAsset(runtime.GOOS, runtime.GOARCH)
	var asset *GitHubAsset
	for i := range rel.Assets {
		if rel.Assets[i].Name == assetName {
			asset = &rel.Assets[i]
			break
		}
	}
	if asset == nil {
		return fmt.Errorf("release %s has no %s binary", latest, assetName)
	}

	staging, err := newStagingDir(home)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	status("Downloading", sgrCyan, "chainlink %s", latest)
	downloaded := filepath.Join(staging, assetName)
	if err := downloadFile(asset.BrowserDownloadURL, downloaded); err != nil {
		return fmt.Errorf("failed to download %s: %w", assetName, err)
	}
	if !hasBinaryMagic(downloaded) {
		return fmt.Errorf("downloaded %s is not an executable", assetName)
	}

	dest := filepath.Join(home, ".chainlink", "bin", chainlinkBinaryName(runtime.GOOS))
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("failed to create bin directory: %w", err)
	}
	if err := installBinary(downloaded, dest); err != nil {
		return fmt.Errorf("failed to write binary to %s: %w", dest, err)
	}
	status("Updated", sgrGreen, "chainlink %s → %s %s", chainlinkVersion, latest, dim(dest))

	// The plugin is embedded in the binary, so the new binary installs its own.
	if err := updateRunSelfInstall(dest); err != nil {
		warnf("failed to refresh the Studio plugin and PATH: %v (run `chainlink self-install`)", err)
	}

	if exe, err := installExecutable(); err == nil && !sameExecutable(exe, dest) {
		warnf("this chainlink is running from %s, not %s; make sure %s comes first on your PATH",
			exe, dest, filepath.Dir(dest))
	}
	return nil
}

// sameExecutable reports whether a and b are the same file, following symlinks.
func sameExecutable(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}
