package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

type GitHubRelease struct {
	Assets []GitHubAsset `json:"assets"`
}

type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// These are package variables so tests can point installs at an httptest
// server and a fake chainlink binary.
var (
	githubAPIBase     = "https://api.github.com"
	installHTTPClient = newInstallHTTPClient()
	installExecutable = os.Executable
)

// maxExtractBytes caps the total uncompressed size of one archive, as a guard
// against decompression bombs.
var maxExtractBytes int64 = 2 << 30

// newInstallHTTPClient returns the client shared by tool and package installs.
// There is no overall Timeout because release archives can be large; stalled
// connections are caught by the dial, TLS and response-header timeouts.
func newInstallHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   4,
		},
	}
}

// SetupProjectBin is deprecated and should not be used.
func SetupProjectBin(tools map[string]string) error {
	return nil
}

// InstallToolList installs multiple tools from a map.
func InstallToolList(tools map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(tools)) {
		spec := tools[name]
		if err := InstallTool(name, spec); err != nil {
			return fmt.Errorf("failed to install tool %s (%s): %w", name, spec, err)
		}
	}
	return nil
}

// toolSpec is a parsed "owner/repo@tag".
type toolSpec struct {
	Repo string // "Kampfkarren/selene"
	Name string // "selene"
	Tag  string // "0.32.0"
}

// validPathComponent reports whether s is safe to use as a single directory
// name under ~/.chainlink (no separators, no "..").
func validPathComponent(s string) bool {
	return s != "" && s != "." && !strings.Contains(s, "..") && !strings.ContainsAny(s, `/\`)
}

// parseToolSpec parses "owner/repo@tag".
func parseToolSpec(spec string) (toolSpec, error) {
	parts := strings.Split(spec, "@")
	if len(parts) != 2 {
		return toolSpec{}, fmt.Errorf("invalid tool format: %s. Expected format 'owner/repo@tag'", spec)
	}
	repoParts := strings.Split(parts[0], "/")
	if len(repoParts) != 2 || !validPathComponent(repoParts[0]) || !validPathComponent(repoParts[1]) {
		return toolSpec{}, fmt.Errorf("invalid tool repo: %s. Expected format 'owner/repo@tag'", spec)
	}
	if !validPathComponent(parts[1]) {
		return toolSpec{}, fmt.Errorf("invalid tool tag in %s", spec)
	}
	return toolSpec{Repo: parts[0], Name: repoParts[1], Tag: parts[1]}, nil
}

// toolInstallDir returns the directory for a tool version, keyed by the repo
// short name (the same key runToolBySpec uses). If the tag was installed with
// a 'v' prefix that directory is returned instead. installed reports whether
// the returned directory exists.
func toolInstallDir(home, key, tag string) (dir string, installed bool) {
	base := filepath.Join(home, ".chainlink", "tools", key)
	dir = filepath.Join(base, tag)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir, true
	}
	if !strings.HasPrefix(tag, "v") {
		vDir := filepath.Join(base, "v"+tag)
		if info, err := os.Stat(vDir); err == nil && info.IsDir() {
			return vDir, true
		}
	}
	return dir, false
}

// installedToolDir is toolInstallDir, but also finds installs from older
// versions of ChainLink, which named the directory after the chain.toml alias
// instead of the repo.
func installedToolDir(home string, s toolSpec, alias string) (dir string, installed bool) {
	dir, installed = toolInstallDir(home, s.Name, s.Tag)
	if installed || alias == "" || alias == s.Name {
		return dir, installed
	}
	if legacy, ok := toolInstallDir(home, alias, s.Tag); ok {
		return legacy, true
	}
	return dir, false
}

// InstallTool downloads a tool release from GitHub by tag.
// Format: "owner/repo@tag" (e.g., "Kampfkarren/selene@0.32.0")
func InstallTool(toolName, spec string) error {
	_, err := InstallToolDir(toolName, spec)
	return err
}

// InstallToolDir is InstallTool, but also returns the directory the tool is
// installed in.
//
// The files go in ~/.chainlink/tools/<repo>/<tag>, where <tag> is the tag as
// written in the spec even when the release was found under 'v'+tag, so the
// tool proxy finds it right after an auto-install. toolName only names the
// ~/.chainlink/bin symlink.
func InstallToolDir(toolName, spec string) (string, error) {
	s, err := parseToolSpec(spec)
	if err != nil {
		return "", err
	}
	if !validPathComponent(toolName) {
		return "", fmt.Errorf("invalid tool name: %q", toolName)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	// Check if the tool is already installed (as-is or with 'v' prefix)
	if dir, ok := installedToolDir(home, s, toolName); ok {
		status("Cached", sgrBlue, "%s", spec)
		if err := linkTool(home, toolName); err != nil {
			warnf("%v", err)
		}
		return dir, nil
	}

	release, tag, err := fetchRelease(s.Repo, s.Tag)
	if err != nil {
		return "", err
	}
	if len(release.Assets) == 0 {
		return "", fmt.Errorf("no assets found for release %s", tag)
	}

	asset, err := selectAsset(release.Assets, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", fmt.Errorf("%s/%s: %w", s.Repo, tag, err)
	}

	// Download and extract into a staging dir, and move it into place only on
	// success, so a failed install is retried rather than skipped next time.
	staging, err := newStagingDir(home)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)

	destPath, err := safeJoin(staging, asset.Name)
	if err != nil || destPath == staging {
		return "", fmt.Errorf("invalid asset name %q", asset.Name)
	}
	status("Downloading", sgrCyan, "%s %s", spec, dim(asset.Name))
	if err := downloadFile(asset.BrowserDownloadURL, destPath); err != nil {
		return "", fmt.Errorf("failed to download asset: %w", err)
	}

	contents := filepath.Join(staging, "contents")
	if err := os.MkdirAll(contents, 0755); err != nil {
		return "", err
	}
	extracted, err := extractArchive(destPath, contents)
	if err != nil {
		return "", fmt.Errorf("failed to extract %s: %w", asset.Name, err)
	}
	if !extracted {
		// A raw binary: name it after the tool so it is easy to find.
		binName := s.Name
		if runtime.GOOS == "windows" {
			binName += ".exe"
		}
		binPath := filepath.Join(contents, binName)
		if err := os.Rename(destPath, binPath); err != nil {
			return "", err
		}
		if err := os.Chmod(binPath, 0755); err != nil {
			return "", err
		}
	}

	if err := makeBinariesExecutable(contents); err != nil {
		return "", fmt.Errorf("failed to make binaries executable: %w", err)
	}

	binDir := filepath.Join(home, ".chainlink", "tools", s.Name, s.Tag)
	if err := moveIntoPlace(contents, binDir); err != nil {
		return "", err
	}
	status("Installed", sgrGreen, "%s %s", spec, dim(binDir))

	return binDir, linkTool(home, toolName)
}

// fetchRelease looks up a release by tag. GitHub tags often start with 'v',
// but users might omit it, so a 404 is retried with 'v'+tag. It returns the
// tag that resolved.
func fetchRelease(repo, tag string) (*GitHubRelease, string, error) {
	release, status, err := getRelease(repo, tag)
	if status == http.StatusNotFound && !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
		release, _, err = getRelease(repo, tag)
	}
	return release, tag, err
}

func getRelease(repo, tag string) (*GitHubRelease, int, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", githubAPIBase, repo, url.PathEscape(tag))
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := installHTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to fetch release info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, resp.StatusCode, fmt.Errorf("github API returned status: %s (rate limited? set GITHUB_TOKEN)", resp.Status)
		}
		return nil, resp.StatusCode, fmt.Errorf("github API returned status: %s", resp.Status)
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to decode release JSON: %w", err)
	}
	return &release, resp.StatusCode, nil
}

// Common spellings of each OS and arch in release asset names.
var (
	assetOSAliases = map[string][]string{
		"darwin": {"darwin", "macos", "apple", "osx"},
		"linux":  {"linux"},
		// Not bare "win64": "darwin64" contains it.
		"windows": {"windows", "-win64", "_win64", "-win32", "_win32"},
	}
	assetArchAliases = map[string][]string{
		"amd64": {"amd64", "x86_64", "x86-64", "x64"},
		"arm64": {"arm64", "aarch64"},
		"386":   {"i386", "i686"},
		"arm":   {"armv7", "armhf"},
	}
	// Assets that sit next to the real archive and are never the tool.
	assetSkipSuffixes = []string{
		".sha256", ".sha512", ".sha1", ".md5", ".sig", ".asc", ".pem",
		".sbom", ".txt", ".json", ".intoto.jsonl", ".minisig",
	}
)

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func isArchiveName(name string) bool {
	for _, ext := range []string{".zip", ".tar.gz", ".tgz", ".tar.xz"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// selectAsset picks the release asset for goos/goarch. An OS+arch match wins;
// otherwise an asset naming only the OS (e.g. a universal macOS binary) is
// used, as long as it does not name a different arch. As a last resort, arm64
// macOS and Windows accept an amd64 build, which they run under emulation.
// Checksums and signatures are ignored, and archives are preferred over other
// files.
func selectAsset(assets []GitHubAsset, goos, goarch string) (*GitHubAsset, error) {
	osNames := assetOSAliases[goos]
	if len(osNames) == 0 {
		osNames = []string{goos}
	}
	archNames := assetArchAliases[goarch]
	if len(archNames) == 0 {
		archNames = []string{goarch}
	}

	var emulated []string
	if goarch == "arm64" && (goos == "darwin" || goos == "windows") {
		emulated = assetArchAliases["amd64"]
	}

	exact, osOnly, cross := -1, -1, -1
	exactArchive, osOnlyArchive, crossArchive := false, false, false
	for i, asset := range assets {
		name := strings.ToLower(asset.Name)
		if !containsAny(name, osNames) || hasAnySuffix(name, assetSkipSuffixes) {
			continue
		}
		archive := isArchiveName(name)
		if containsAny(name, archNames) {
			if exact < 0 || (archive && !exactArchive) {
				exact, exactArchive = i, archive
			}
			continue
		}
		if namesOtherArch(name, goarch) {
			if containsAny(name, emulated) && (cross < 0 || (archive && !crossArchive)) {
				cross, crossArchive = i, archive
			}
			continue
		}
		if osOnly < 0 || (archive && !osOnlyArchive) {
			osOnly, osOnlyArchive = i, archive
		}
	}

	switch {
	case exact >= 0:
		return &assets[exact], nil
	case osOnly >= 0:
		return &assets[osOnly], nil
	case cross >= 0:
		return &assets[cross], nil
	}
	return nil, fmt.Errorf("no suitable asset found for %s/%s", goos, goarch)
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// namesOtherArch reports whether name mentions an arch other than goarch.
func namesOtherArch(name, goarch string) bool {
	for arch, aliases := range assetArchAliases {
		if arch != goarch && containsAny(name, aliases) {
			return true
		}
	}
	return false
}

// newStagingDir creates a temp dir under ~/.chainlink/.staging. It sits on the
// same filesystem as the install dirs so finished installs can be renamed in.
func newStagingDir(home string) (string, error) {
	root := filepath.Join(home, ".chainlink", ".staging")
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", fmt.Errorf("failed to create staging directory: %w", err)
	}
	dir, err := os.MkdirTemp(root, "install-*")
	if err != nil {
		return "", fmt.Errorf("failed to create staging directory: %w", err)
	}
	return dir, nil
}

// moveIntoPlace renames a fully prepared src dir to dest. If dest appeared in
// the meantime (a concurrent install), the existing copy is kept.
func moveIntoPlace(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("failed to create install directory: %w", err)
	}
	if err := os.Rename(src, dest); err != nil {
		if info, statErr := os.Stat(dest); statErr == nil && info.IsDir() {
			return nil
		}
		return fmt.Errorf("failed to move install into %s: %w", dest, err)
	}
	return nil
}

// chainlinkBinaryName is the file name `chainlink self-install` installs as.
func chainlinkBinaryName(goos string) string {
	if goos == "windows" {
		return "chainlink.exe"
	}
	return "chainlink"
}

// linkTool points ~/.chainlink/bin/<toolName> at the chainlink binary, which
// proxies the call to the installed tool.
func linkTool(home, toolName string) error {
	globalBinDir := filepath.Join(home, ".chainlink", "bin")
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(toolName), ".exe") {
		toolName += ".exe"
	}
	if err := os.MkdirAll(globalBinDir, 0755); err != nil {
		return fmt.Errorf("failed to create global bin directory: %w", err)
	}
	target, err := toolSymlinkTarget(home)
	if err != nil {
		return err
	}
	return ensureSymlink(target, filepath.Join(globalBinDir, toolName))
}

// toolSymlinkTarget prefers the stable ~/.chainlink/bin/chainlink over the
// running binary, which may be a download that is later deleted. The running
// binary's path is resolved, because when invoked through a tool symlink
// os.Executable can return the symlink itself.
func toolSymlinkTarget(home string) (string, error) {
	stable := filepath.Join(home, ".chainlink", "bin", chainlinkBinaryName(runtime.GOOS))
	if info, err := os.Lstat(stable); err == nil && info.Mode().IsRegular() {
		return stable, nil
	}
	exe, err := installExecutable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// ensureSymlink makes link point at target, leaving a correct link untouched.
func ensureSymlink(target, link string) error {
	cleanTarget, cleanLink := filepath.Clean(target), filepath.Clean(link)
	samePath := cleanTarget == cleanLink
	if runtime.GOOS == "windows" && strings.EqualFold(cleanTarget, cleanLink) {
		samePath = true
	}
	if !samePath {
		targetInfo, targetErr := os.Stat(target)
		linkInfo, linkErr := os.Stat(link)
		if targetErr == nil && linkErr == nil && os.SameFile(targetInfo, linkInfo) {
			samePath = true
		}
	}
	if samePath {
		return fmt.Errorf("refusing to replace %s with a link to itself", link)
	}
	if current, err := os.Readlink(link); err == nil && current == target {
		return nil
	}
	if _, err := os.Lstat(link); err == nil {
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("failed to replace %s: %w", link, err)
		}
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("failed to create global symlink %s: %w", link, err)
	}
	return nil
}

// extractArchive extracts src into dest based on its extension. It returns
// false (and does nothing) when src is not a known archive type.
func extractArchive(src, dest string) (bool, error) {
	name := strings.ToLower(src)
	switch {
	case strings.HasSuffix(name, ".zip"):
		return true, extractZip(src, dest)
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return true, extractTarGz(src, dest)
	case strings.HasSuffix(name, ".tar.xz"):
		return true, extractTarXz(src, dest)
	}
	return false, nil
}

// safeJoin joins an archive entry name onto dest, rejecting names that are
// absolute, carry a drive letter, or climb out of dest with "..".
func safeJoin(dest, name string) (string, error) {
	// Archive names use '/', but zips made on Windows may use '\'.
	n := strings.ReplaceAll(name, `\`, "/")
	if path.IsAbs(n) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" ||
		(len(n) >= 2 && n[1] == ':') {
		return "", fmt.Errorf("illegal absolute path in archive: %q", name)
	}
	n = path.Clean(n)
	if n == "." {
		return filepath.Clean(dest), nil
	}
	local := filepath.FromSlash(n)
	if n == ".." || strings.HasPrefix(n, "../") || !filepath.IsLocal(local) {
		return "", fmt.Errorf("illegal file path in archive: %q", name)
	}
	return filepath.Join(dest, local), nil
}

// writeExtractedFile writes r to target, charging the bytes to *budget.
func writeExtractedFile(target string, r io.Reader, mode os.FileMode, budget *int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	n, err := io.CopyN(f, r, *budget+1)
	*budget -= n
	if err == io.EOF {
		err = nil
	} else if err == nil {
		err = errors.New("archive exceeds the extraction size limit")
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func extractZip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	budget := maxExtractBytes
	for _, f := range r.File {
		target, err := safeJoin(dest, f.Name)
		if err != nil {
			return err
		}

		mode := f.Mode()
		if mode.IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() {
			continue // symlinks and other special entries are skipped
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeExtractedFile(target, rc, mode, &budget)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(src, dest string) error {
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()

	gzr, err := gzip.NewReader(bufio.NewReader(file))
	if err != nil {
		return err
	}
	defer gzr.Close()
	return extractTar(gzr, dest)
}

func extractTarXz(src, dest string) error {
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()

	xzr, err := xz.NewReader(bufio.NewReader(file))
	if err != nil {
		return err
	}
	return extractTar(xzr, dest)
}

// extractTar extracts regular files and directories. Links and other special
// entries are skipped, so nothing can point outside dest.
func extractTar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	budget := maxExtractBytes
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			target, err := safeJoin(dest, header.Name)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			target, err := safeJoin(dest, header.Name)
			if err != nil {
				return err
			}
			if err := writeExtractedFile(target, tr, os.FileMode(header.Mode), &budget); err != nil {
				return err
			}
		}
	}
}

// Leading bytes of ELF and Mach-O (thin and universal) executables.
var binaryMagics = [][]byte{
	{0x7f, 'E', 'L', 'F'},
	{0xfe, 0xed, 0xfa, 0xce}, {0xce, 0xfa, 0xed, 0xfe},
	{0xfe, 0xed, 0xfa, 0xcf}, {0xcf, 0xfa, 0xed, 0xfe},
	{0xca, 0xfe, 0xba, 0xbe},
}

func hasBinaryMagic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [4]byte
	n, _ := io.ReadFull(f, head[:])
	// Shebang scripts are tools too (the old extension rule made them executable).
	if n >= 2 && head[0] == '#' && head[1] == '!' {
		return true
	}
	if n < len(head) {
		return false
	}
	for _, m := range binaryMagics {
		if bytes.Equal(head[:], m) {
			return true
		}
	}
	return false
}

// makeBinariesExecutable sets the exec bit on extracted ELF/Mach-O files.
// Other files (LICENSE, README, ...) keep the mode from the archive.
func makeBinariesExecutable(binDir string) error {
	return filepath.WalkDir(binDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0111 == 0111 || !hasBinaryMagic(path) {
			return nil
		}
		return os.Chmod(path, info.Mode().Perm()|0755)
	})
}

// isToolBinary reports whether a file in a tool dir looks like a program.
func isToolBinary(path string, info os.FileInfo, goos string) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	if goos == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	return info.Mode().Perm()&0111 != 0 || hasBinaryMagic(path)
}

// findToolBinary finds the executable in an installed tool dir, preferring
// one named after the tool (ignoring case and a .exe suffix).
func findToolBinary(dir, toolName, goos string) (string, error) {
	var first string
	var match string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !isToolBinary(p, info, goos) {
			return nil
		}
		base := d.Name()
		if goos == "windows" {
			base = strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".EXE")
		}
		if strings.EqualFold(base, toolName) {
			match = p
			return filepath.SkipAll
		}
		if first == "" && !isDocFile(base) {
			first = p
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if match != "" {
		return match, nil
	}
	if first != "" {
		return first, nil
	}
	return "", fmt.Errorf("%w: no executable for %s in %s", errToolBinaryNotFound, toolName, dir)
}

var errToolBinaryNotFound = errors.New("tool binary not found")

// isDocFile reports names that release archives ship next to the binary, which
// may carry an exec bit from the archive but are never the tool.
func isDocFile(name string) bool {
	lower := strings.ToLower(name)
	for _, doc := range []string{"license", "licence", "readme", "changelog", "copying", "notice", "authors", "contributors"} {
		if strings.HasPrefix(lower, doc) {
			return true
		}
	}
	return false
}

// downloadFile saves url to dest, removing dest if anything fails.
func downloadFile(url, dest string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	return downloadRequest(req, dest)
}

func downloadRequest(req *http.Request, dest string) (err error) {
	resp, err := installHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(dest)
		}
	}()

	_, err = io.Copy(out, resp.Body)
	return err
}
