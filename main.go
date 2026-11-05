package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
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

var (
	ExampleController = `function mod:init()
	print("Hello world!")
end`
	ExampleService = `function mod:init()
	print("Hello world!")
end`
	ExampleData = `{
	"hello": "world"
}`
	VSCodeSettings = `{
    "luau-lsp.sourcemap.rojoPath": "chainlink",
    "luau-lsp.sourcemap.rojoProjectFile": "chain.toml",
	"luau-lsp.sourcemap.useVSCodeWatcher": true,
    "luau-lsp.types.definitionFiles": [
        ".vscode/globals.d.luau"
    ]
}`
	VSCodeGlobals = `declare mod: any
declare use: (module: any) -> any
declare new: (instance: string) -> Instance
declare module: any`
)

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
			fmt.Printf("Error installing globally: %v\n", err)
			os.Exit(1)
		}
		return
	case invokeToolProxy:
		// If the tool ran but returned an exit code, runToolBySpec already exited with it.
		if err := proxyToTool(invokedAs, os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	cmd := os.Args[1]
	switch cmd {
	case "help":
		printUsage()
	case "init":
		if err := handleInit(); err != nil {
			fmt.Printf("Error initializing: %v\n", err)
			os.Exit(1)
		}
	case "add":
		if len(os.Args) < 3 {
			fmt.Println("Usage: chainlink add <spec> [--tool]")
			os.Exit(1)
		}
		if err := handleAdd(os.Args[2:]); err != nil {
			fmt.Printf("Error adding: %v\n", err)
			os.Exit(1)
		}
	case "remove":
		if len(os.Args) < 3 {
			fmt.Println("Usage: chainlink remove <name>")
			os.Exit(1)
		}
		if err := handleRemove(os.Args[2]); err != nil {
			fmt.Printf("Error removing: %v\n", err)
			os.Exit(1)
		}
	case "install":
		if len(os.Args) > 2 {
			fmt.Println("Error: 'chainlink install' does not take arguments.")
			os.Exit(1)
		}
		if err := installAll(); err != nil {
			fmt.Printf("Error installing: %v\n", err)
			os.Exit(1)
		}
	case "plugin":
		if err := installPlugin(); err != nil {
			fmt.Printf("Error installing plugin: %v\n", err)
			os.Exit(1)
		}
	case "login":
		if err := handleLogin(os.Args[2:]); err != nil {
			fmt.Printf("Error logging in: %v\n", err)
			os.Exit(1)
		}
	case "publish":
		if err := handlePublish(os.Args[2:]); err != nil {
			fmt.Printf("Error publishing: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		if err := serve(); err != nil {
			fmt.Printf("Error serving: %v\n", err)
			os.Exit(1)
		}
	case "build":
		if len(os.Args) < 3 {
			fmt.Println("Usage: chainlink build <output.rbxlx>")
			os.Exit(1)
		}
		if err := handleBuild(os.Args[2]); err != nil {
			fmt.Printf("Error building: %v\n", err)
			os.Exit(1)
		}
	case "sourcemap":
		outputPath := ""
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "-o" && i+1 < len(os.Args) {
				outputPath = os.Args[i+1]
				break
			}
		}
		if err := handleSourcemap(outputPath); err != nil {
			fmt.Printf("Error generating sourcemap: %v\n", err)
			os.Exit(1)
		}
	case "run":

		if len(os.Args) < 3 {
			fmt.Println("Usage: chainlink run <spec> [args]")
			os.Exit(1)
		}
		spec := os.Args[2]
		if err := runToolBySpec(spec, os.Args[3:]); err != nil {
			fmt.Printf("Error running tool %s: %v\n", spec, err)
			os.Exit(1)
		}
	case "global":
		if err := handleGlobal(); err != nil {
			fmt.Printf("Error installing globally: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: chainlink <command> [args]")
	fmt.Println("Commands: init, install, plugin, login, publish, add, remove, serve, build, run, global, help")
}

func createPlaceholderFile(path string, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func handleInit() error {
	// 1. Git initialization
	if _, err := os.Stat(".git"); os.IsNotExist(err) {
		fmt.Println("Initializing git repository...")
		if err := exec.Command("git", "init").Run(); err != nil {
			return fmt.Errorf("failed to run git init: %w", err)
		}
	} else {
		fmt.Println("Git repository already exists.")
	}

	// 2. Create chain.toml
	if _, err := os.Stat("chain.toml"); err == nil {
		return fmt.Errorf("chain.toml already exists")
	}

	fmt.Println("Creating chain.toml...")
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
	fmt.Println("Creating project structure...")

	// Client: ExampleController
	if err := os.MkdirAll("client", 0755); err != nil {
		return fmt.Errorf("failed to create directory client: %w", err)
	}
	if err := createPlaceholderFile(filepath.Join("client", "ExampleController.luau"), ExampleController); err != nil {
		return err
	}

	// Server: ExampleService
	if err := os.MkdirAll("server", 0755); err != nil {
		return fmt.Errorf("failed to create directory server: %w", err)
	}
	if err := createPlaceholderFile(filepath.Join("server", "ExampleService.luau"), ExampleService); err != nil {
		return err
	}

	// Shared: ExampleData
	if err := os.MkdirAll("shared", 0755); err != nil {
		return fmt.Errorf("failed to create directory shared: %w", err)
	}
	if err := createPlaceholderFile(filepath.Join("shared", "ExampleData.json"), ExampleData); err != nil {
		return err
	}

	// VSCode configuration
	if err := createPlaceholderFile(filepath.Join(".vscode", "settings.json"), VSCodeSettings); err != nil {
		return err
	}
	if err := createPlaceholderFile(filepath.Join(".vscode", "globals.d.luau"), VSCodeGlobals); err != nil {
		return err
	}

	return nil
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
	_, name, _, err := parseToolSpec(spec)
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

func installAll() error {
	var config ChainLinkConfig
	if _, err := toml.DecodeFile("chain.toml", &config); err != nil {
		return fmt.Errorf("could not find chain.toml: %w", err)
	}

	fmt.Println("Installing packages...")
	if err := InstallPackageList(config.Packages); err != nil {
		return fmt.Errorf("package installation failed: %w", err)
	}

	fmt.Println("Installing tools...")
	if err := InstallToolList(config.Tools); err != nil {
		return fmt.Errorf("tool installation failed: %w", err)
	}

	return nil
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

	destName := "chainlink"
	if runtime.GOOS == "windows" {
		destName += ".exe"
	}
	destPath := filepath.Join(binDir, destName)

	// Copy binary to global bin
	if err := installBinary(exePath, destPath); err != nil {
		return fmt.Errorf("failed to write binary to %s: %w", destPath, err)
	}

	fmt.Printf("Chainlink installed globally to %s\n", destPath)

	if err := InstallChainlink(); err != nil {
		fmt.Printf("Warning: failed to update PATH: %v\n", err)
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
		fmt.Println("Token saved.")
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
		fmt.Println("Token saved.")
		return nil
	}

	if cfg.GithubOAuthID == "" {
		return promptAPIKey(cfg.API)
	}
	return promptGithubAuth(cfg.API, cfg.GithubOAuthID)
}

func promptAPIKey(api string) error {
	fmt.Printf("Wally requires an API key to publish to %s.\n", api)
	fmt.Print("Enter an API key: ")

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
	fmt.Println("API key saved.")
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
	fmt.Printf("Go to %s\n", dc.VerificationURI)
	fmt.Printf("And enter the code: %s\n", dc.UserCode)
	fmt.Println()
	fmt.Println("Awaiting authorization...")

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
			fmt.Println("Authorization successful!")
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
		default:
			return fmt.Errorf("GitHub auth error: %s (%s)", tr.Error, tr.ErrorDescription)
		}
	}
}

type wallyManifest struct {
	Package wallyManifestPackage `toml:"package"`
}

type wallyManifestPackage struct {
	Name        string `toml:"name"`
	Version     string `toml:"version"`
	Registry    string `toml:"registry"`
	License     string `toml:"license"`
	Description string `toml:"description"`
	Homepage    string `toml:"homepage"`
	Realm       string `toml:"realm"`
}

// buildWallyManifest renders the wally.toml uploaded with a published package.
// Values go through the TOML encoder so quotes, backslashes and newlines are escaped.
func buildWallyManifest(pkg PackageInfo, registry string) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	err := enc.Encode(wallyManifest{Package: wallyManifestPackage{
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
			fmt.Printf("Warning: %s directory not found, skipping.\n", dir)
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

	fmt.Printf("Preparing to publish %s version %s...\n", config.Package.Name, config.Package.Version)

	// Fetch the registry from the project's config to include it in the manifest
	registry := projectRegistry(".")

	manifest, err := buildWallyManifest(config.Package, registry)
	if err != nil {
		return fmt.Errorf("failed to create wally.toml: %w", err)
	}

	var zipData bytes.Buffer
	if err := buildPackageZip(&zipData, ".", manifest); err != nil {
		return fmt.Errorf("failed to create zip: %w", err)
	}

	fmt.Println("Uploading to Wally registry...")

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

	fmt.Println("Package published successfully!")
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
	return runToolBySpec(spec, args)
}

// parseToolSpec splits "owner/repo@tag" into its repo, short name and tag.
// The owner is optional ("repo@tag"), but exactly one non-empty tag is required.
func parseToolSpec(spec string) (repo, name, tag string, err error) {
	repo, tag, ok := strings.Cut(spec, "@")
	if !ok || repo == "" || tag == "" || strings.Contains(tag, "@") {
		return "", "", "", fmt.Errorf("invalid tool spec: %s", spec)
	}
	name = repo[strings.LastIndex(repo, "/")+1:]
	if name == "" {
		return "", "", "", fmt.Errorf("invalid tool spec: %s", spec)
	}
	return repo, name, tag, nil
}

// toolTagDirs lists the install directory names a tag may live under. InstallTool
// retries GitHub with a 'v' prefix, so "1.0" can end up installed as "v1.0".
func toolTagDirs(tag string) []string {
	if strings.HasPrefix(tag, "v") {
		return []string{tag}
	}
	return []string{tag, "v" + tag}
}

var errToolBinaryNotFound = errors.New("tool binary not found")

// locateToolBinary finds the installed binary of a tool under toolRoot
// (~/.chainlink/tools/<name>), checking every directory the tag may map to.
func locateToolBinary(toolRoot, toolName, tag string) (string, error) {
	for _, t := range toolTagDirs(tag) {
		if bin, err := findToolBinary(filepath.Join(toolRoot, t), toolName); err == nil {
			return bin, nil
		}
	}
	return "", errToolBinaryNotFound
}

// findToolBinary picks the executable for toolName inside an extracted release.
// A file named after the tool (ignoring case and .exe) wins; otherwise the first
// executable that isn't a LICENSE/README-style document is used.
func findToolBinary(dir, toolName string) (string, error) {
	var match, fallback string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isRunnableFile(d) {
			return nil
		}
		name := d.Name()
		if strings.EqualFold(filepath.Ext(name), ".exe") {
			name = name[:len(name)-len(".exe")]
		}
		if strings.EqualFold(name, toolName) {
			match = p
			return filepath.SkipAll
		}
		if fallback == "" && !isDocFile(name) {
			fallback = p
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if match != "" {
		return match, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", errToolBinaryNotFound
}

func isRunnableFile(d fs.DirEntry) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Ext(d.Name()), ".exe")
	}
	info, err := d.Info()
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

// isDocFile reports names that release archives ship next to the binary and
// that makeBinariesExecutable marks executable because they have no extension.
func isDocFile(name string) bool {
	lower := strings.ToLower(name)
	for _, doc := range []string{"license", "licence", "readme", "changelog", "copying", "notice", "authors", "contributors"} {
		if strings.HasPrefix(lower, doc) {
			return true
		}
	}
	return false
}

func runToolBySpec(spec string, args []string) error {
	_, toolName, tag, err := parseToolSpec(spec)
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	toolRoot := filepath.Join(home, ".chainlink", "tools", toolName)
	binaryPath, err := locateToolBinary(toolRoot, toolName, tag)
	if err != nil {
		// Try to install the tool if it's missing
		fmt.Printf("Tool %s not found locally. Attempting to install...\n", toolName)

		// A directory without a usable binary is left over from an interrupted
		// install; remove it so InstallTool doesn't report "already installed".
		for _, t := range toolTagDirs(tag) {
			os.RemoveAll(filepath.Join(toolRoot, t))
		}

		if installErr := InstallTool(toolName, spec); installErr != nil {
			return fmt.Errorf("could not find binary for tool %s in %s and installation failed: %v", toolName, toolRoot, installErr)
		}

		// InstallTool may have installed under the 'v'-prefixed tag, so look in every candidate dir again.
		binaryPath, err = locateToolBinary(toolRoot, toolName, tag)
		if err != nil {
			return fmt.Errorf("installed tool %s but could not find binary in %s", toolName, toolRoot)
		}
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
