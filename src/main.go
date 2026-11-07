package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
	"golang.org/x/term"
)

type PackageInfo struct {
	Name        string
	Version     string
	License     string
	Description string
	Website     string
}

type ChainLinkConfig struct {
	Package  PackageInfo       `toml:"Package"`
	Tools    map[string]string `toml:"Tools"`
	Packages map[string]string `toml:"Packages"`
}

// initTemplate holds the files `chainlink init` creates, laid out as they
// appear in the new project. "all:" keeps the dot-directory .vscode.
//
//go:embed all:templates/init
var initTemplate embed.FS

const initTemplateRoot = "templates/init"

// registryClient is shared by registry config fetches, the GitHub device flow
// and publish uploads so none of them can hang forever on a stalled connection.
var registryClient = &http.Client{Timeout: 60 * time.Second}

// maxErrorBody caps how much of an error response body is read into memory.
const maxErrorBody = 64 << 10

const defaultRegistry = "https://github.com/UpliftGames/wally-index"

type invocationMode int

const (
	invokeCLI invocationMode = iota
	invokeGlobalInstall
	invokeToolProxy
)

// classifyInvocation decides what to do based on the binary name (os.Args[0]).
// "chainlink" (any case, optional .exe) is the CLI, "chainlink-<os>-<arch>" is a
// release binary being run for the first time, anything else is a tool symlink.
// The returned name has any .exe suffix removed.
func classifyInvocation(arg0 string) (invocationMode, string) {
	name := filepath.Base(arg0)
	if strings.EqualFold(filepath.Ext(name), ".exe") {
		name = name[:len(name)-len(".exe")]
	}
	lower := strings.ToLower(name)
	switch {
	case lower == "chainlink":
		return invokeCLI, name
	case strings.HasPrefix(lower, "chainlink-"):
		return invokeGlobalInstall, name
	}
	return invokeToolProxy, name
}

func main() {
	// If we are called as a symlink for a tool, the first argument (os.Args[0])
	// will be the tool name.
	mode, invokedAs := classifyInvocation(os.Args[0])
	switch mode {
	case invokeGlobalInstall:
		// A release binary being run for the first time: default to global installation.
		if err := handleGlobal(); err != nil {
			fail("installing globally", err)
		}
		return
	case invokeToolProxy:
		// The tool owns stdout, so ChainLink's own messages go to stderr.
		uiOut, colorOut = os.Stderr, colorStderr
		// If the tool ran but returned an exit code, runToolBySpec already exited with it.
		if err := proxyToTool(invokedAs, os.Args[1:]); err != nil {
			fail("", err)
		}
		return
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	cmd := os.Args[1]
	switch cmd {
	case "help", "-h", "--help":
		printUsage()
	case "init":
		if err := handleInit(); err != nil {
			fail("initializing", err)
		}
	case "add":
		if len(os.Args) < 3 {
			usageFail("chainlink add <spec> [--tool | --package]")
		}
		if err := handleAdd(os.Args[2:]); err != nil {
			fail("adding", err)
		}
	case "remove":
		if len(os.Args) < 3 {
			usageFail("chainlink remove <name>")
		}
		if err := handleRemove(os.Args[2]); err != nil {
			fail("removing", err)
		}
	case "install":
		assumeYes, err := parseInstallArgs(os.Args[2:])
		if err != nil {
			fail("install", err)
		}
		if err := installAll(assumeYes); err != nil {
			fail("installing", err)
		}
	case "login":
		if err := handleLogin(os.Args[2:]); err != nil {
			fail("logging in", err)
		}
	case "publish":
		if err := handlePublish(os.Args[2:]); err != nil {
			fail("publishing", err)
		}
	case "serve":
		if err := serve(); err != nil {
			fail("serving", err)
		}
	case "build":
		if len(os.Args) < 3 {
			usageFail("chainlink build <output.rbxl|.rbxlx|.rbxm|.rbxmx>")
		}
		if err := handleBuild(os.Args[2]); err != nil {
			fail("building", err)
		}
	case "sourcemap":
		if err := handleSourcemap(sourcemapOutput(os.Args[2:])); err != nil {
			fail("generating sourcemap", err)
		}
	case "self-install":
		if err := handleGlobal(); err != nil {
			fail("installing globally", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "%s unknown command %q\n\n", paint(colorStderr, "error:", sgrBold, sgrRed), cmd)
		printUsage()
		os.Exit(2)
	}
}

// sourcemapOutput returns the -o/--output path. luau-lsp's rojoPath integration
// passes "--output <path>" along with flags and a project file we don't need.
func sourcemapOutput(args []string) string {
	for i, a := range args {
		switch {
		case (a == "-o" || a == "--output") && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "--output="):
			return strings.TrimPrefix(a, "--output=")
		}
	}
	return ""
}

// usageCommands is the command list shown by "chainlink help".
var usageCommands = [][2]string{
	{"init", "Create chain.toml and a starter project (client/, server/, shared/)"},
	{"install [-y]", "Install the tools and packages in chain.toml (-y: don't ask)"},
	{"add <spec> [--tool|--package]", "Add a tool (owner/repo@tag) or package (scope/name@version)"},
	{"remove <name>", "Remove a tool or package from chain.toml"},
	{"serve", "Start the live-sync server for the Studio plugin"},
	{"build <output>", "Build a place (.rbxl/.rbxlx) or src/ as a model (.rbxm/.rbxmx)"},
	{"login", "Authenticate with a package registry"},
	{"publish", "Publish this project as a package"},
	{"self-install", "Install chainlink to ~/.chainlink/bin, add it to PATH, and install the Studio plugin"},
	{"help", "Show this help"},
}

func printUsage() {
	fmt.Printf("%s %s\n\n", bold("ChainLink"), dim("— tools and packages for Roblox projects"))
	fmt.Printf("%s chainlink <command> [args]\n\n", bold("Usage:"))
	fmt.Println(bold("Commands:"))
	width := 0
	for _, c := range usageCommands {
		width = max(width, len(c[0]))
	}
	for _, c := range usageCommands {
		fmt.Printf("  %s  %s\n", cyan(fmt.Sprintf("%-*s", width, c[0])), c[1])
	}
}

func createPlaceholderFile(path string, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

// writeInitTemplate copies the embedded init template into dir.
func writeInitTemplate(dir string) error {
	return fs.WalkDir(initTemplate, initTemplateRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := initTemplate.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, initTemplateRoot+"/")
		return createPlaceholderFile(filepath.Join(dir, filepath.FromSlash(rel)), string(data))
	})
}

func handleInit() error {
	// 1. Git initialization
	if _, err := os.Stat(".git"); os.IsNotExist(err) {
		status("Initializing", sgrCyan, "git repository")
		if err := exec.Command("git", "init").Run(); err != nil {
			return fmt.Errorf("failed to run git init: %w", err)
		}
	} else {
		status("Skipping", sgrBlue, "git init %s", dim("(repository already exists)"))
	}

	// 2. Create chain.toml
	if _, err := os.Stat("chain.toml"); err == nil {
		return fmt.Errorf("chain.toml already exists")
	}

	status("Creating", sgrCyan, "chain.toml")
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}
	folderName := filepath.Base(cwd)

	config := ChainLinkConfig{
		Package: PackageInfo{
			Name:    folderName,
			Version: "0.1.0",
		},
		Tools:    make(map[string]string),
		Packages: make(map[string]string),
	}

	if err := writeConfig(&config); err != nil {
		return err
	}

	// 3. Create project structure
	status("Creating", sgrCyan, "project structure")

	return writeInitTemplate(".")
}

// loadRawConfig decodes chain.toml into a generic map so add/remove can rewrite
// it without dropping keys ChainLinkConfig does not model (DisableBootstrap,
// [package] registry, future sections).
func loadRawConfig(path string) (map[string]any, error) {
	raw := map[string]any{}
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// configTable returns the table stored under name (matched case-insensitively,
// like toml.Decode does for ChainLinkConfig). When create is set a missing
// table is added; otherwise nil is returned for it.
func configTable(raw map[string]any, name string, create bool) (map[string]any, error) {
	key := ""
	if _, ok := raw[name]; ok {
		key = name
	} else {
		for k := range raw {
			if strings.EqualFold(k, name) {
				key = k
				break
			}
		}
	}
	if key == "" {
		if !create {
			return nil, nil
		}
		t := map[string]any{}
		raw[name] = t
		return t, nil
	}
	t, ok := raw[key].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s in chain.toml must be a table", key)
	}
	return t, nil
}

func handleRemove(name string) error {
	raw, err := loadRawConfig("chain.toml")
	if err != nil {
		return fmt.Errorf("could not find chain.toml: %w", err)
	}

	// Check tools first, then packages (case-insensitive)
	for _, section := range []string{"Tools", "Packages"} {
		table, err := configTable(raw, section, false)
		if err != nil {
			return err
		}
		for k := range table {
			if strings.EqualFold(k, name) {
				delete(table, k)
				return writeConfig(raw)
			}
		}
	}

	return fmt.Errorf("item %s not found in tools or packages", name)
}

func handleAdd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("no specification provided")
	}

	spec := args[0]
	isTool := false
	isPackage := false
	for _, arg := range args[1:] {
		if arg == "--tool" {
			isTool = true
		} else if arg == "--package" {
			isPackage = true
		}
	}

	// Heuristic: assume tool if it contains capital letters, otherwise package
	if !isTool && !isPackage {
		isTool = containsUppercase(spec)
	}

	section := "Packages"
	parsed, err := parseToolSpec(spec)
	name := parsed.Name
	if isTool {
		section = "Tools"
		if err != nil {
			return fmt.Errorf("invalid tool spec. Expected format 'owner/repo@tag'")
		}
	} else if err != nil {
		return fmt.Errorf("invalid package spec. Expected format 'scope/name@version'")
	}

	raw, err := loadRawConfig("chain.toml")
	if errors.Is(err, fs.ErrNotExist) {
		// Create new config if not exists
		raw = map[string]any{"Tools": map[string]any{}, "Packages": map[string]any{}}
	} else if err != nil {
		return fmt.Errorf("failed to parse chain.toml: %w", err)
	}

	table, err := configTable(raw, section, true)
	if err != nil {
		return err
	}
	table[name] = spec

	return writeConfig(raw)
}

func containsUppercase(s string) bool {
	for _, r := range s {
		// Specs are almost always ASCII, so skip the unicode table lookup for them.
		if r < 0x80 {
			if r >= 'A' && r <= 'Z' {
				return true
			}
		} else if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// pendingDownload is a package or tool that isn't in the global cache yet.
type pendingDownload struct {
	Kind   string // "package" or "tool"
	Spec   string
	Source string
}

// planDownloads lists what installing config would download, in install order.
// Invalid specs are reported here so nothing is fetched before they're fixed.
func planDownloads(config ChainLinkConfig, home string) ([]pendingDownload, error) {
	var pending []pendingDownload
	for _, alias := range slices.Sorted(maps.Keys(config.Packages)) {
		spec := config.Packages[alias]
		scope, name, version, err := parsePackageSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("package %s: %w", alias, err)
		}
		if !dirExists(packageInstallDir(home, scope, name, version)) {
			pending = append(pending, pendingDownload{"package", spec, "wally.run"})
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(config.Tools)) {
		spec := config.Tools[alias]
		s, err := parseToolSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", alias, err)
		}
		if _, installed := installedToolDir(home, s, alias); !installed {
			pending = append(pending, pendingDownload{"tool", spec, "github.com/" + s.Repo})
		}
	}
	return pending, nil
}

// confirmDownloads lists pending downloads and asks before fetching anything.
// assumeYes (chainlink install -y) skips the prompt.
func confirmDownloads(pending []pendingDownload, assumeYes bool) error {
	if len(pending) == 0 || assumeYes {
		return nil
	}
	fmt.Fprintln(uiOut, bold("The following will be downloaded for the first time:"))
	width := 0
	for _, p := range pending {
		width = max(width, len(p.Spec))
	}
	for _, p := range pending {
		fmt.Fprintf(uiOut, "  %s %-*s  %s\n", cyan(fmt.Sprintf("%-7s", p.Kind)), width, p.Spec, dim("from "+p.Source))
	}
	ok, err := confirm(fmt.Sprintf("Download %d item(s)?", len(pending)))
	if errors.Is(err, errNotInteractive) {
		return fmt.Errorf("%w; re-run with 'chainlink install -y' to download without asking", err)
	} else if err != nil {
		return err
	}
	if !ok {
		return errors.New("cancelled, nothing was downloaded")
	}
	return nil
}

func installAll(assumeYes bool) error {
	var config ChainLinkConfig
	if _, err := toml.DecodeFile("chain.toml", &config); err != nil {
		return fmt.Errorf("could not find chain.toml: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	pending, err := planDownloads(config, home)
	if err != nil {
		return err
	}
	if err := confirmDownloads(pending, assumeYes); err != nil {
		return err
	}

	if err := InstallPackageList(config.Packages); err != nil {
		return fmt.Errorf("package installation failed: %w", err)
	}
	if err := InstallToolList(config.Tools); err != nil {
		return fmt.Errorf("tool installation failed: %w", err)
	}

	status("Finished", sgrGreen, "%d package(s) and %d tool(s) ready", len(config.Packages), len(config.Tools))
	return nil
}

// parseInstallArgs accepts -y/--yes and rejects anything else.
func parseInstallArgs(args []string) (assumeYes bool, err error) {
	for _, a := range args {
		switch a {
		case "-y", "--yes":
			assumeYes = true
		default:
			return false, fmt.Errorf("unknown argument %q", a)
		}
	}
	return assumeYes, nil
}

// writeConfig encodes config (a ChainLinkConfig or a raw map) to chain.toml,
// keeping the existing file's permissions.
func writeConfig(config any) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(config); err != nil {
		return fmt.Errorf("failed to encode chain.toml: %w", err)
	}
	perm := os.FileMode(0644)
	if info, err := os.Stat("chain.toml"); err == nil {
		perm = info.Mode().Perm()
	}
	return replaceFileAtomic("chain.toml", buf.Bytes(), perm)
}

// replaceFileAtomic writes data to a temp file next to path and renames it into
// place, so a failed write never leaves path truncated.
func replaceFileAtomic(path string, data []byte, perm os.FileMode) error {
	return replaceFileFrom(path, bytes.NewReader(data), perm)
}

func replaceFileFrom(path string, r io.Reader, perm os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, r); err != nil {
		return err
	}
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		oldPath := path + ".old"
		if err = os.Remove(oldPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, statErr := os.Lstat(path); statErr == nil {
			if err = os.Rename(path, oldPath); err != nil {
				return err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if err = os.Rename(tmp.Name(), path); err != nil {
			if restoreErr := os.Rename(oldPath, path); restoreErr != nil && !errors.Is(restoreErr, os.ErrNotExist) {
				return fmt.Errorf("failed to install replacement: %v; failed to restore previous file: %w", err, restoreErr)
			}
			return err
		}
		if err = os.Remove(oldPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("installed replacement but failed to remove backup %s: %w", oldPath, err)
		}
		return nil
	}
	return os.Rename(tmp.Name(), path)
}

func handleGlobal() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	// We want to put it in ~/.chainlink/bin/chainlink
	binDir := filepath.Join(home, ".chainlink", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("failed to create bin directory: %w", err)
	}

	destPath := filepath.Join(binDir, chainlinkBinaryName(runtime.GOOS))

	// Copy binary to global bin
	if err := installBinary(exePath, destPath); err != nil {
		return fmt.Errorf("failed to write binary to %s: %w", destPath, err)
	}

	status("Installed", sgrGreen, "chainlink %s", dim(destPath))

	if err := InstallChainlink(); err != nil {
		warnf("failed to update PATH: %v", err)
	}

	if err := installPlugin(); err != nil {
		warnf("failed to install Studio plugin: %v", err)
	}

	return nil
}

// installBinary streams src into a temp file beside dst and renames it over dst.
// Replacing the file instead of rewriting it avoids ETXTBSY while a copy of
// chainlink is running and keeps macOS code signatures valid. Copying a file
// onto itself is a no-op.
func installBinary(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if dstInfo, err := os.Stat(dst); err == nil && os.SameFile(srcInfo, dstInfo) {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return replaceFileFrom(dst, in, 0755)
}

type AuthStore struct {
	Tokens map[string]string `toml:"tokens"`
}

func authPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".chainlink", "auth.toml"), nil
}

func loadAuthStore() (*AuthStore, error) {
	path, err := authPath()
	if err != nil {
		return nil, err
	}
	store := &AuthStore{Tokens: map[string]string{}}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, store); err != nil {
		return nil, fmt.Errorf("malformed auth config at %s: %w", path, err)
	}
	if store.Tokens == nil {
		store.Tokens = map[string]string{}
	}
	return store, nil
}

func (s *AuthStore) save() error {
	path, err := authPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("failed to create auth directory: %w", err)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(s); err != nil {
		return fmt.Errorf("failed to encode auth store: %w", err)
	}
	return replaceFileAtomic(path, buf.Bytes(), 0600)
}

func setToken(api, token string) error {
	key, err := normalizeURL(api)
	if err != nil {
		return err
	}
	store, err := loadAuthStore()
	if err != nil {
		return err
	}
	if token == "" {
		delete(store.Tokens, key)
	} else {
		store.Tokens[key] = token
	}
	return store.save()
}

func getToken(api string) (string, error) {
	key, err := normalizeURL(api)
	if err != nil {
		return "", err
	}
	store, err := loadAuthStore()
	if err != nil {
		return "", err
	}
	if t, ok := store.Tokens[key]; ok {
		return t, nil
	}
	if t, ok := store.Tokens[strings.TrimSuffix(key, "/")]; ok {
		return t, nil
	}
	return "", fmt.Errorf("no token found for %s", key)
}

func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

type IndexConfig struct {
	API           string   `json:"api"`
	GithubOAuthID string   `json:"github_oauth_id,omitempty"`
	FallbackRegs  []string `json:"fallback_registries,omitempty"`
}

// indexConfigURL maps a GitHub-hosted registry index repo to the raw URL of its config.json.
func indexConfigURL(registry string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(registry), ".git"))
	if err != nil {
		return "", fmt.Errorf("invalid registry URL %q: %w", registry, err)
	}
	repoPath := strings.Trim(u.Path, "/")
	if u.Host != "github.com" || repoPath == "" {
		return "", fmt.Errorf("only GitHub-hosted registries are supported (got %s); use --api", registry)
	}
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/HEAD/config.json", repoPath), nil
}

func fetchIndexConfig(registry string) (*IndexConfig, error) {
	rawURL, err := indexConfigURL(registry)
	if err != nil {
		return nil, err
	}
	return fetchIndexConfigFrom(registryClient, rawURL)
}

func fetchIndexConfigFrom(client *http.Client, rawURL string) (*IndexConfig, error) {
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch registry config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch registry config from %s: %s", rawURL, resp.Status)
	}

	var cfg IndexConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("malformed registry config: %w", err)
	}
	if cfg.API == "" {
		return nil, fmt.Errorf("registry config has no 'api' field")
	}
	return &cfg, nil
}

func projectRegistry(projectPath string) string {
	for _, name := range []string{"chain.toml", "wally.toml"} {
		var m struct {
			Package struct {
				Registry string `toml:"registry"`
			} `toml:"package"`
		}
		if _, err := toml.DecodeFile(filepath.Join(projectPath, name), &m); err == nil && m.Package.Registry != "" {
			return m.Package.Registry
		}
	}
	return defaultRegistry
}

func handleLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	projectPath := fs.String("project-path", ".", "path to the project")
	api := fs.String("api", "", "API URL to store the token for")
	token := fs.String("token", "", "token to store (skips interactive login)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *token != "" && *api != "" {
		if err := setToken(*api, *token); err != nil {
			return err
		}
		status("Saved", sgrGreen, "token for %s", *api)
		return nil
	}

	var cfg *IndexConfig
	if *api != "" {
		cfg = &IndexConfig{API: *api}
	} else {
		var err error
		cfg, err = fetchIndexConfig(projectRegistry(*projectPath))
		if err != nil {
			return err
		}
	}

	if *token != "" {
		if err := setToken(cfg.API, *token); err != nil {
			return err
		}
		status("Saved", sgrGreen, "token for %s", cfg.API)
		return nil
	}

	if cfg.GithubOAuthID == "" {
		return promptAPIKey(cfg.API)
	}
	return promptGithubAuth(cfg.API, cfg.GithubOAuthID)
}

func promptAPIKey(api string) error {
	fmt.Printf("The package registry requires an API key to publish to %s.\n", bold(api))
	fmt.Printf("%s %s ", paint(colorOut, "?", sgrBold, sgrCyan), bold("Enter an API key:"))

	var key string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return fmt.Errorf("failed to read API key: %w", err)
		}
		key = string(b)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return fmt.Errorf("failed to read API key: %w", err)
		}
		key = line
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("API key cannot be empty")
	}
	if err := setToken(api, key); err != nil {
		return err
	}
	status("Saved", sgrGreen, "API key for %s", api)
	return nil
}

type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type accessTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval"`
}

func postJSON(client *http.Client, endpoint string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("%s: %s", resp.Status, string(data))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// githubDeviceFlow holds the endpoints and clock used by the GitHub OAuth device
// flow so tests can point it at a local server and skip the polling delay.
type githubDeviceFlow struct {
	client   *http.Client
	codeURL  string
	tokenURL string
	sleep    func(time.Duration)
}

func promptGithubAuth(api, clientID string) error {
	return githubDeviceFlow{
		client:   registryClient,
		codeURL:  "https://github.com/login/device/code",
		tokenURL: "https://github.com/login/oauth/access_token",
		sleep:    time.Sleep,
	}.run(api, clientID)
}

func (g githubDeviceFlow) run(api, clientID string) error {
	var dc deviceCodeResponse
	err := postJSON(g.client, g.codeURL, map[string]string{
		"client_id": clientID,
		"scope":     "read:user read:org",
	}, &dc)
	if err != nil {
		return fmt.Errorf("failed to start GitHub device flow: %w", err)
	}

	fmt.Println()
	fmt.Printf("  Open %s\n", paint(colorOut, dc.VerificationURI, sgrBold, sgrCyan))
	fmt.Printf("  and enter the code %s\n", paint(colorOut, dc.UserCode, sgrBold, sgrYellow))
	fmt.Println()
	status("Waiting", sgrCyan, "for GitHub authorization...")

	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)

	for {
		g.sleep(interval)
		if dc.ExpiresIn > 0 && time.Now().After(deadline) {
			return errors.New("device code expired, please try again")
		}

		var tr accessTokenResponse
		err := postJSON(g.client, g.tokenURL, map[string]string{
			"client_id":   clientID,
			"device_code": dc.DeviceCode,
			"grant_type":  "urn:ietf:params:oauth:grant-type:device-code",
		}, &tr)
		if err != nil {
			return fmt.Errorf("failed to poll for access token: %w", err)
		}

		switch tr.Error {
		case "":
			if tr.AccessToken == "" {
				return errors.New("GitHub returned an empty access token")
			}
			if err := setToken(api, tr.AccessToken); err != nil {
				return err
			}
			status("Authorized", sgrGreen, "with GitHub")
			return nil
		case "authorization_pending":
			continue
		case "slow_down":
			if tr.Interval > 0 {
				interval = time.Duration(tr.Interval) * time.Second
			} else {
				interval += 5 * time.Second
			}
		case "expired_token":
			return errors.New("device code expired, please try again")
		case "access_denied":
			return errors.New("authorization was cancelled")
		case "incorrect_client_credentials":
			return fmt.Errorf("GitHub rejected the registry's OAuth app (client ID %s); it may have device flow disabled or be misconfigured. "+
				"Ask the registry owner to fix it, or log in with a token instead: chainlink login --api %s --token <token>", clientID, api)
		default:
			return fmt.Errorf("GitHub auth error: %s (%s)", tr.Error, tr.ErrorDescription)
		}
	}
}

type packageManifest struct {
	Package packageManifestPackage `toml:"package"`
}

type packageManifestPackage struct {
	Name        string `toml:"name"`
	Version     string `toml:"version"`
	Registry    string `toml:"registry"`
	License     string `toml:"license"`
	Description string `toml:"description"`
	Homepage    string `toml:"homepage"`
	Realm       string `toml:"realm"`
}

// buildPackageManifest renders the manifest (wally.toml) uploaded with a published package.
// Values go through the TOML encoder so quotes, backslashes and newlines are escaped.
func buildPackageManifest(pkg PackageInfo, registry string) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	err := enc.Encode(packageManifest{Package: packageManifestPackage{
		Name:        pkg.Name,
		Version:     pkg.Version,
		Registry:    registry,
		License:     pkg.License,
		Description: pkg.Description,
		Homepage:    pkg.Website,
		Realm:       "shared",
	}})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildPackageZip writes the publish archive (packages/, src/ and wally.toml)
// straight from the project directory, without staging a copy on disk.
func buildPackageZip(w io.Writer, projectDir string, manifest []byte) error {
	zw := zip.NewWriter(w)
	for _, dir := range []string{"packages", "src"} {
		root := filepath.Join(projectDir, dir)
		info, err := os.Stat(root)
		if errors.Is(err, fs.ErrNotExist) {
			warnf("%s directory not found, skipping", dir)
			continue
		} else if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", root)
		}
		if err := zipTree(zw, root, dir); err != nil {
			return fmt.Errorf("failed to add %s: %w", dir, err)
		}
	}
	f, err := zw.Create("wally.toml")
	if err != nil {
		return err
	}
	if _, err := f.Write(manifest); err != nil {
		return err
	}
	return zw.Close()
}

// zipTree adds every regular file under root to zw as prefix/<rel>, using
// forward slashes as the zip format requires. Symlinks are followed; a link
// back into one of its own ancestors is reported as an error.
func zipTree(zw *zip.Writer, root, prefix string) error {
	return zipTreeVisit(zw, root, prefix, map[string]bool{})
}

func zipTreeVisit(zw *zip.Writer, dir, prefix string, active map[string]bool) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if active[resolved] {
		return fmt.Errorf("symlink cycle at %s", dir)
	}
	active[resolved] = true
	defer delete(active, resolved)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		name := path.Join(prefix, e.Name())
		var info fs.FileInfo
		if e.Type()&fs.ModeSymlink != 0 {
			info, err = os.Stat(p)
		} else {
			info, err = e.Info()
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := zipTreeVisit(zw, p, name, active); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := zipFile(zw, p, name); err != nil {
			return err
		}
	}
	return nil
}

func zipFile(zw *zip.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

func handlePublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	tokenFlag := fs.String("token", "", "API token to use for publishing (overrides auth.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var config ChainLinkConfig
	if _, err := toml.DecodeFile("chain.toml", &config); err != nil {
		return fmt.Errorf("could not find chain.toml: %w", err)
	}

	if config.Package.Name == "" || config.Package.Version == "" {
		return fmt.Errorf("package name and version must be defined in chain.toml for publishing")
	}

	status("Packaging", sgrCyan, "%s@%s", config.Package.Name, config.Package.Version)

	// Fetch the registry from the project's config to include it in the manifest
	registry := projectRegistry(".")

	manifest, err := buildPackageManifest(config.Package, registry)
	if err != nil {
		return fmt.Errorf("failed to create wally.toml: %w", err)
	}

	var zipData bytes.Buffer
	if err := buildPackageZip(&zipData, ".", manifest); err != nil {
		return fmt.Errorf("failed to create zip: %w", err)
	}

	status("Uploading", sgrCyan, "to the package registry %s", dim(fmt.Sprintf("(%d KiB)", zipData.Len()/1024)))

	if zipData.Len() > 2*1024*1024 {
		return fmt.Errorf("package size exceeds 2MB. Reduce package size and try again")
	}

	api := "https://api.wally.run"
	cfg, err := fetchIndexConfig(registry)
	if err == nil {
		api = cfg.API
	}

	var token string
	if *tokenFlag != "" {
		token = *tokenFlag
	} else {
		var err error
		token, err = getToken(api)
		if err != nil {
			return fmt.Errorf("authentication error: %w. Please run 'chainlink login' or use --token", err)
		}
	}

	publishURL := strings.TrimSuffix(api, "/") + "/v1/publish"

	req, err := http.NewRequest("POST", publishURL, bytes.NewReader(zipData.Bytes()))
	if err != nil {
		return fmt.Errorf("failed to create upload request: %w", err)
	}

	req.Header.Set("accept", "application/json")
	req.Header.Set("Wally-Version", "0.3.2")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := registryClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute upload request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("version already exists")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("failed to publish package: %s\n%s", resp.Status, string(respBody))
	}

	status("Published", sgrGreen, "%s@%s", config.Package.Name, config.Package.Version)
	return nil
}

// errToolNotDefined is wrapped by resolveToolSpec when chain.toml has no entry for a tool.
var errToolNotDefined = errors.New("not defined in chain.toml")

// resolveToolSpec looks up a tool's spec, preferring an exact name match and
// falling back to a case-insensitive one.
func resolveToolSpec(cfg *ChainLinkConfig, name string) (string, error) {
	if spec, ok := cfg.Tools[name]; ok {
		return spec, nil
	}
	for k, spec := range cfg.Tools {
		if strings.EqualFold(k, name) {
			return spec, nil
		}
	}
	return "", fmt.Errorf("tool %s %w", name, errToolNotDefined)
}

func proxyToTool(toolName string, args []string) error {
	var config ChainLinkConfig
	if _, err := toml.DecodeFile("chain.toml", &config); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("tool %s %w (config missing)", toolName, errToolNotDefined)
		}
		return fmt.Errorf("could not read chain.toml: %w", err)
	}

	spec, err := resolveToolSpec(&config, toolName)
	if err != nil {
		return err
	}
	return runToolBySpec(spec, toolName, args)
}

// runToolBySpec runs the tool spec names, installing it first if needed.
// alias is the name the tool was invoked as, used to find legacy installs.
func runToolBySpec(spec, alias string, args []string) error {
	s, err := parseToolSpec(spec)
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	// Installs are staged and renamed into place, so an existing dir is complete.
	toolDir, installed := installedToolDir(home, s, alias)
	if !installed {
		status("Missing", sgrYellow, "%s, installing it first", spec)
		toolDir, err = InstallToolDir(s.Name, spec)
		if err != nil {
			return fmt.Errorf("could not find tool %s and installation failed: %v", s.Name, err)
		}
	}

	binaryPath, err := findToolBinary(toolDir, s.Name, runtime.GOOS)
	if err != nil {
		return fmt.Errorf("could not find binary for tool %s in %s: %w", s.Name, toolDir, err)
	}

	cmd := exec.Command(binaryPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	err = cmd.Run()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			os.Exit(exitError.ExitCode())
		}
		return err
	}

	return nil
}
