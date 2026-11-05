package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// cliSetHome points the user's home directory at a fresh temp dir.
func cliSetHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// cliChdirTemp switches into a fresh temp dir for the duration of the test.
func cliChdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func cliWriteFile(t testing.TB, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func cliReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func cliDecodeChainToml(t *testing.T) map[string]any {
	t.Helper()
	raw := map[string]any{}
	if _, err := toml.DecodeFile("chain.toml", &raw); err != nil {
		t.Fatalf("chain.toml no longer parses: %v", err)
	}
	return raw
}

func cliTable(t *testing.T, raw map[string]any, name string) map[string]any {
	t.Helper()
	tbl, ok := raw[name].(map[string]any)
	if !ok {
		t.Fatalf("missing table %s in %v", name, raw)
	}
	return tbl
}

func TestContainsUppercase(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"Kampfkarren/selene@0.27.1", true},
		{"sleitnick/signal@2.0.1", false},
		{"ś", false},
		{"ž", false},
		{"×", false},
		{"", false},
		{"Ž", true},
	}
	for _, tt := range tests {
		if got := containsUppercase(tt.in); got != tt.want {
			t.Errorf("containsUppercase(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://api.wally.run", "https://api.wally.run/", false},
		{" https://x.com/a ", "https://x.com/a", false},
		{"https://x.com/v1", "https://x.com/v1", false},
		{"api.wally.run", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeURL(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("normalizeURL(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizeURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAuthStore(t *testing.T) {
	t.Run("missing file gives empty store", func(t *testing.T) {
		cliSetHome(t)
		store, err := loadAuthStore()
		if err != nil {
			t.Fatal(err)
		}
		if store.Tokens == nil || len(store.Tokens) != 0 {
			t.Fatalf("want empty non-nil map, got %#v", store.Tokens)
		}
	})

	t.Run("round trip and trailing slash", func(t *testing.T) {
		home := cliSetHome(t)
		if err := setToken("https://api.wally.run", "secret"); err != nil {
			t.Fatal(err)
		}
		for _, api := range []string{"https://api.wally.run", "https://api.wally.run/"} {
			got, err := getToken(api)
			if err != nil || got != "secret" {
				t.Errorf("getToken(%q) = %q, %v", api, got, err)
			}
		}
		if _, err := getToken("https://other.example"); err == nil {
			t.Error("expected error for unknown API")
		}
		if runtime.GOOS != "windows" {
			fi, err := os.Stat(filepath.Join(home, ".chainlink", "auth.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0600 {
				t.Errorf("auth.toml mode = %v, want 0600", fi.Mode().Perm())
			}
			di, err := os.Stat(filepath.Join(home, ".chainlink"))
			if err != nil {
				t.Fatal(err)
			}
			if di.Mode().Perm() != 0700 {
				t.Errorf(".chainlink mode = %v, want 0700", di.Mode().Perm())
			}
		}
	})

	t.Run("token without trailing slash in file", func(t *testing.T) {
		home := cliSetHome(t)
		cliWriteFile(t, filepath.Join(home, ".chainlink", "auth.toml"), "[tokens]\n\"https://x.com\" = \"tok\"\n", 0600)
		got, err := getToken("https://x.com")
		if err != nil || got != "tok" {
			t.Fatalf("getToken = %q, %v", got, err)
		}
	})

	t.Run("empty token deletes", func(t *testing.T) {
		cliSetHome(t)
		if err := setToken("https://api.wally.run", "secret"); err != nil {
			t.Fatal(err)
		}
		if err := setToken("https://api.wally.run", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := getToken("https://api.wally.run"); err == nil {
			t.Fatal("token still present after delete")
		}
	})

	t.Run("malformed file errors", func(t *testing.T) {
		home := cliSetHome(t)
		cliWriteFile(t, filepath.Join(home, ".chainlink", "auth.toml"), "[tokens\nbroken", 0600)
		if _, err := loadAuthStore(); err == nil {
			t.Fatal("expected error for malformed auth.toml")
		}
		if err := setToken("https://api.wally.run", "x"); err == nil {
			t.Fatal("setToken should not overwrite malformed auth.toml")
		}
	})

	t.Run("no tokens table", func(t *testing.T) {
		home := cliSetHome(t)
		cliWriteFile(t, filepath.Join(home, ".chainlink", "auth.toml"), "other = 1\n", 0600)
		store, err := loadAuthStore()
		if err != nil {
			t.Fatal(err)
		}
		if store.Tokens == nil {
			t.Fatal("Tokens map is nil")
		}
	})

	t.Run("invalid api", func(t *testing.T) {
		cliSetHome(t)
		if err := setToken("not a url", "x"); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestHandleAdd(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		section string
		key     string
	}{
		{"tool heuristic", []string{"Kampfkarren/selene@0.27.1"}, "Tools", "selene"},
		{"tool owner case", []string{"Owner/Repo@v1"}, "Tools", "Repo"},
		{"package heuristic", []string{"scope/name@1.0.0"}, "Packages", "name"},
		{"tool flag", []string{"rojo-rbx/rojo@7.4.0", "--tool"}, "Tools", "rojo"},
		{"package flag", []string{"Sleitnick/Signal@2.0.1", "--package"}, "Packages", "Signal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cliChdirTemp(t)
			if err := handleAdd(tt.args); err != nil {
				t.Fatal(err)
			}
			raw := cliDecodeChainToml(t)
			if got := cliTable(t, raw, tt.section)[tt.key]; got != tt.args[0] {
				t.Fatalf("%s[%s] = %v, want %s", tt.section, tt.key, got, tt.args[0])
			}
			other := "Packages"
			if tt.section == "Packages" {
				other = "Tools"
			}
			if len(cliTable(t, raw, other)) != 0 {
				t.Fatalf("%s should be empty", other)
			}
		})
	}
}

func TestHandleAddInvalidSpecs(t *testing.T) {
	for _, spec := range []string{"scope/name", "Owner/Repo", "a/b@", "a/b@1@2", "@1.0", ""} {
		t.Run(spec, func(t *testing.T) {
			cliChdirTemp(t)
			if err := handleAdd([]string{spec}); err == nil {
				t.Fatalf("handleAdd(%q) succeeded", spec)
			}
			if _, err := os.Stat("chain.toml"); err == nil {
				t.Fatal("chain.toml created for invalid spec")
			}
		})
	}
	if err := handleAdd(nil); err == nil {
		t.Fatal("handleAdd(nil) succeeded")
	}
}

// Regression: a chain.toml with no [Packages] table used to panic with
// "assignment to entry in nil map".
func TestHandleAddMissingTable(t *testing.T) {
	cliChdirTemp(t)
	cliWriteFile(t, "chain.toml", "[Package]\nName = \"x\"\n\n[Tools]\nselene = \"Kampfkarren/selene@0.27.1\"\n", 0644)
	if err := handleAdd([]string{"sleitnick/signal@2.0.1"}); err != nil {
		t.Fatal(err)
	}
	raw := cliDecodeChainToml(t)
	if cliTable(t, raw, "Packages")["signal"] != "sleitnick/signal@2.0.1" {
		t.Fatal("package not added")
	}
	if cliTable(t, raw, "Tools")["selene"] != "Kampfkarren/selene@0.27.1" {
		t.Fatal("existing tool lost")
	}
}

// Regression: a chain.toml with a syntax error used to be replaced wholesale.
func TestHandleAddMalformedConfigUntouched(t *testing.T) {
	cliChdirTemp(t)
	const bad = "[Tools\nselene = \"oops\n"
	cliWriteFile(t, "chain.toml", bad, 0644)
	if err := handleAdd([]string{"sleitnick/signal@2.0.1"}); err == nil {
		t.Fatal("expected parse error")
	}
	if got := cliReadFile(t, "chain.toml"); got != bad {
		t.Fatalf("chain.toml modified: %q", got)
	}
	if err := handleRemove("selene"); err == nil {
		t.Fatal("expected parse error from remove")
	}
	if got := cliReadFile(t, "chain.toml"); got != bad {
		t.Fatalf("chain.toml modified by remove: %q", got)
	}
}

// Regression: add/remove used to drop every key ChainLinkConfig didn't model.
func TestAddRemovePreservesUnknownKeys(t *testing.T) {
	cliChdirTemp(t)
	cliWriteFile(t, "chain.toml", `DisableBootstrap = true

[package]
name = "mylib"
version = "1.2.3"
registry = "https://github.com/me/my-index"

[Tools]
selene = "Kampfkarren/selene@0.27.1"

[Packages]

[Custom]
answer = 42
list = ["a", "b"]
`, 0640)

	if err := handleAdd([]string{"sleitnick/signal@2.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := handleRemove("SELENE"); err != nil {
		t.Fatal(err)
	}

	raw := cliDecodeChainToml(t)
	if raw["DisableBootstrap"] != true {
		t.Errorf("DisableBootstrap lost: %v", raw)
	}
	pkg := cliTable(t, raw, "package")
	if pkg["registry"] != "https://github.com/me/my-index" || pkg["name"] != "mylib" {
		t.Errorf("package table changed: %v", pkg)
	}
	if got := projectRegistry("."); got != "https://github.com/me/my-index" {
		t.Errorf("projectRegistry = %q", got)
	}
	custom := cliTable(t, raw, "Custom")
	if custom["answer"] != int64(42) {
		t.Errorf("Custom.answer = %#v", custom["answer"])
	}
	if len(cliTable(t, raw, "Tools")) != 0 {
		t.Errorf("selene not removed")
	}
	if cliTable(t, raw, "Packages")["signal"] != "sleitnick/signal@2.0.1" {
		t.Errorf("signal not added")
	}
	// No ChainLinkConfig section should be duplicated with a different case.
	if _, ok := raw["Package"]; ok {
		t.Errorf("duplicate [Package] table written")
	}

	var typed ChainLinkConfig
	if _, err := toml.DecodeFile("chain.toml", &typed); err != nil {
		t.Fatal(err)
	}
	if typed.Package.Name != "mylib" {
		t.Errorf("typed decode Package.Name = %q", typed.Package.Name)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat("chain.toml")
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0640 {
			t.Errorf("chain.toml mode = %v, want 0640 preserved", fi.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestHandleAddLowercaseSectionNames(t *testing.T) {
	cliChdirTemp(t)
	cliWriteFile(t, "chain.toml", "[tools]\n\n[packages]\n", 0644)
	if err := handleAdd([]string{"Kampfkarren/selene@0.27.1"}); err != nil {
		t.Fatal(err)
	}
	raw := cliDecodeChainToml(t)
	if cliTable(t, raw, "tools")["selene"] == nil {
		t.Fatalf("tool not added to existing [tools]: %v", raw)
	}
	if _, ok := raw["Tools"]; ok {
		t.Fatal("duplicate [Tools] table created")
	}
}

func TestHandleAddNonTableSection(t *testing.T) {
	cliChdirTemp(t)
	cliWriteFile(t, "chain.toml", "Tools = \"oops\"\n", 0644)
	if err := handleAdd([]string{"Kampfkarren/selene@0.27.1"}); err == nil {
		t.Fatal("expected error when Tools is not a table")
	}
}

func TestHandleRemove(t *testing.T) {
	cliChdirTemp(t)
	if err := handleRemove("x"); err == nil {
		t.Fatal("expected error without chain.toml")
	}
	cliWriteFile(t, "chain.toml", "[Tools]\nStyLua = \"JohnnyMorganz/StyLua@0.20.0\"\n\n[Packages]\nsignal = \"sleitnick/signal@2.0.1\"\n", 0644)
	if err := handleRemove("unknown"); err == nil {
		t.Fatal("expected error for unknown item")
	}
	if err := handleRemove("stylua"); err != nil {
		t.Fatal(err)
	}
	if err := handleRemove("Signal"); err != nil {
		t.Fatal(err)
	}
	raw := cliDecodeChainToml(t)
	if len(cliTable(t, raw, "Tools")) != 0 || len(cliTable(t, raw, "Packages")) != 0 {
		t.Fatalf("items not removed: %v", raw)
	}
}

func TestHandleRemoveWithoutTables(t *testing.T) {
	cliChdirTemp(t)
	cliWriteFile(t, "chain.toml", "[Package]\nName = \"x\"\n", 0644)
	if err := handleRemove("x"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestProjectRegistry(t *testing.T) {
	tests := []struct {
		name   string
		chain  string
		legacy string
		want   string
	}{
		{"chain lowercase", "[package]\nregistry = \"https://github.com/a/chain\"\n", "", "https://github.com/a/chain"},
		{"chain capitalized", "[Package]\nregistry = \"https://github.com/a/cap\"\n", "", "https://github.com/a/cap"},
		{"legacy only", "[Package]\nName = \"x\"\n", "[package]\nregistry = \"https://github.com/a/legacy\"\n", "https://github.com/a/legacy"},
		{"chain wins", "[package]\nregistry = \"https://github.com/a/chain\"\n", "[package]\nregistry = \"https://github.com/a/legacy\"\n", "https://github.com/a/chain"},
		{"neither", "", "", defaultRegistry},
		{"malformed", "[package\n", "registry = = \n", defaultRegistry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.chain != "" {
				cliWriteFile(t, filepath.Join(dir, "chain.toml"), tt.chain, 0644)
			}
			if tt.legacy != "" {
				cliWriteFile(t, filepath.Join(dir, "wally.toml"), tt.legacy, 0644)
			}
			if got := projectRegistry(dir); got != tt.want {
				t.Fatalf("projectRegistry = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIndexConfigURL(t *testing.T) {
	const want = "https://raw.githubusercontent.com/UpliftGames/wally-index/HEAD/config.json"
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://github.com/UpliftGames/wally-index", want, false},
		{"https://github.com/UpliftGames/wally-index.git", want, false},
		{"https://github.com/UpliftGames/wally-index/", want, false},
		{"https://gitlab.com/UpliftGames/wally-index", "", true},
		{"https://github.com/", "", true},
		{"::bad", "", true},
	}
	for _, tt := range tests {
		got, err := indexConfigURL(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("indexConfigURL(%q) = %q, %v; want %q, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
	if _, err := fetchIndexConfig("https://example.com/x/y"); err == nil {
		t.Error("fetchIndexConfig accepted non-GitHub registry")
	}
}

func TestFetchIndexConfigFrom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			io.WriteString(w, `{"api":"https://api.example","github_oauth_id":"abc","fallback_registries":["r1"]}`)
		case "/noapi":
			io.WriteString(w, `{"github_oauth_id":"abc"}`)
		case "/bad":
			io.WriteString(w, `{not json`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg, err := fetchIndexConfigFrom(srv.Client(), srv.URL+"/ok")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API != "https://api.example" || cfg.GithubOAuthID != "abc" || len(cfg.FallbackRegs) != 1 {
		t.Fatalf("unexpected config %#v", cfg)
	}
	for _, p := range []string{"/missing", "/noapi", "/bad"} {
		if _, err := fetchIndexConfigFrom(srv.Client(), srv.URL+p); err == nil {
			t.Errorf("%s: expected error", p)
		}
	}
}

func TestPostJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad headers", http.StatusBadRequest)
			return
		}
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/echo":
			json.NewEncoder(w).Encode(map[string]string{"got": in["k"]})
		case "/teapot":
			w.WriteHeader(http.StatusTeapot)
			io.WriteString(w, "short and stout")
		case "/huge":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, strings.Repeat("x", 1<<20))
		case "/malformed":
			io.WriteString(w, "{nope")
		}
	}))
	defer srv.Close()

	var out map[string]string
	if err := postJSON(srv.Client(), srv.URL+"/echo", map[string]string{"k": "v"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["got"] != "v" {
		t.Fatalf("out = %v", out)
	}

	err := postJSON(srv.Client(), srv.URL+"/teapot", map[string]string{}, &out)
	if err == nil || !strings.Contains(err.Error(), "418") || !strings.Contains(err.Error(), "short and stout") {
		t.Fatalf("teapot err = %v", err)
	}

	err = postJSON(srv.Client(), srv.URL+"/huge", map[string]string{}, &out)
	if err == nil || len(err.Error()) > maxErrorBody+100 {
		t.Fatalf("error body not bounded: len=%d", len(fmt.Sprint(err)))
	}

	if err := postJSON(srv.Client(), srv.URL+"/malformed", map[string]string{}, &out); err == nil {
		t.Fatal("expected decode error")
	}
	if err := postJSON(srv.Client(), srv.URL+"/echo", func() {}, &out); err == nil {
		t.Fatal("expected marshal error")
	}
}

// cliDeviceServer fakes GitHub's device-code and token endpoints. Each poll of
// the token endpoint returns the next response in the list.
func cliDeviceServer(t *testing.T, responses []string) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code":
			io.WriteString(w, `{"device_code":"dev","user_code":"USER-CODE","verification_uri":"https://github.com/login/device","expires_in":0,"interval":2}`)
		case "/token":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["device_code"] != "dev" || body["client_id"] != "client" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			i := polls
			polls++
			mu.Unlock()
			if i >= len(responses) {
				http.Error(w, "too many polls", http.StatusTooManyRequests)
				return
			}
			io.WriteString(w, responses[i])
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

func TestGithubDeviceFlow(t *testing.T) {
	const api = "https://api.example/"
	tests := []struct {
		name       string
		responses  []string
		wantErr    string
		wantToken  string
		wantSleeps []time.Duration
	}{
		{
			name:       "pending then success",
			responses:  []string{`{"error":"authorization_pending"}`, `{"access_token":"gho_123"}`},
			wantToken:  "gho_123",
			wantSleeps: []time.Duration{2 * time.Second, 2 * time.Second},
		},
		{
			name:       "slow down with interval",
			responses:  []string{`{"error":"slow_down","interval":7}`, `{"access_token":"t"}`},
			wantToken:  "t",
			wantSleeps: []time.Duration{2 * time.Second, 7 * time.Second},
		},
		{
			name:       "slow down without interval",
			responses:  []string{`{"error":"slow_down"}`, `{"access_token":"t"}`},
			wantToken:  "t",
			wantSleeps: []time.Duration{2 * time.Second, 7 * time.Second},
		},
		{name: "expired", responses: []string{`{"error":"expired_token"}`}, wantErr: "expired"},
		{name: "denied", responses: []string{`{"error":"access_denied"}`}, wantErr: "cancelled"},
		{name: "bad client", responses: []string{`{"error":"incorrect_client_credentials"}`}, wantErr: "--api https://api.example/ --token"},
		{name: "unknown", responses: []string{`{"error":"weird","error_description":"desc"}`}, wantErr: "weird (desc)"},
		{name: "empty token", responses: []string{`{"token_type":"bearer"}`}, wantErr: "empty access token"},
		{name: "http error", responses: nil, wantErr: "failed to poll"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cliSetHome(t)
			srv, _ := cliDeviceServer(t, tt.responses)
			var sleeps []time.Duration
			flow := githubDeviceFlow{
				client:   srv.Client(),
				codeURL:  srv.URL + "/code",
				tokenURL: srv.URL + "/token",
				sleep:    func(d time.Duration) { sleeps = append(sleeps, d) },
			}
			err := flow.run(api, "client")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if _, terr := getToken(api); terr == nil {
					t.Fatal("token saved despite error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := getToken(api)
			if err != nil || got != tt.wantToken {
				t.Fatalf("saved token = %q, %v", got, err)
			}
			if fmt.Sprint(sleeps) != fmt.Sprint(tt.wantSleeps) {
				t.Fatalf("sleeps = %v, want %v", sleeps, tt.wantSleeps)
			}
		})
	}
}

func TestGithubDeviceFlowStartFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	flow := githubDeviceFlow{client: srv.Client(), codeURL: srv.URL + "/code", tokenURL: srv.URL + "/token", sleep: func(time.Duration) {}}
	if err := flow.run("https://api.example", "client"); err == nil || !strings.Contains(err.Error(), "device flow") {
		t.Fatalf("err = %v", err)
	}
}

// Regression: the manifest used to be built with fmt.Sprintf and no escaping.
func TestBuildPackageManifest(t *testing.T) {
	pkg := PackageInfo{
		Name:        "me/lib",
		Version:     "1.0.0",
		License:     "MIT",
		Description: "A \"fast\" lib\\with C:\\paths\nand newlines\"\nrealm = \"server",
		Website:     "https://example.com/?a=\"b\"",
	}
	data, err := buildPackageManifest(pkg, "https://github.com/a/index")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Package map[string]any `toml:"package"`
	}
	md, err := toml.Decode(string(data), &got)
	if err != nil {
		t.Fatalf("manifest is not valid TOML: %v\n%s", err, data)
	}
	if len(md.Undecoded()) != 0 {
		t.Fatalf("unexpected keys %v", md.Undecoded())
	}
	want := map[string]any{
		"name":        pkg.Name,
		"version":     pkg.Version,
		"registry":    "https://github.com/a/index",
		"license":     pkg.License,
		"description": pkg.Description,
		"homepage":    pkg.Website,
		"realm":       "shared",
	}
	if fmt.Sprint(got.Package) != fmt.Sprint(want) {
		t.Fatalf("manifest = %v\nwant %v", got.Package, want)
	}
}

func cliZipEntries(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

func cliKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestBuildPackageZip(t *testing.T) {
	dir := t.TempDir()
	cliWriteFile(t, filepath.Join(dir, "src", "init.luau"), "return 1", 0644)
	cliWriteFile(t, filepath.Join(dir, "src", "util", "deep", "x.luau"), "deep", 0644)
	cliWriteFile(t, filepath.Join(dir, "packages", "Signal.lua"), "sig", 0644)
	cliWriteFile(t, filepath.Join(dir, "other", "ignored.txt"), "no", 0644)
	if err := os.MkdirAll(filepath.Join(dir, "src", "empty"), 0755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := buildPackageZip(&buf, dir, []byte("[package]\n")); err != nil {
		t.Fatal(err)
	}
	got := cliZipEntries(t, buf.Bytes())
	want := map[string]string{
		"packages/Signal.lua":  "sig",
		"src/init.luau":        "return 1",
		"src/util/deep/x.luau": "deep",
		"wally.toml":           "[package]\n",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("entries = %v, want %v", cliKeys(got), cliKeys(want))
	}
	for name := range got {
		if strings.Contains(name, "\\") {
			t.Errorf("entry %q uses backslashes", name)
		}
	}
}

func TestBuildPackageZipMissingDirs(t *testing.T) {
	var buf bytes.Buffer
	if err := buildPackageZip(&buf, t.TempDir(), []byte("m")); err != nil {
		t.Fatal(err)
	}
	if got := cliKeys(cliZipEntries(t, buf.Bytes())); fmt.Sprint(got) != "[wally.toml]" {
		t.Fatalf("entries = %v", got)
	}
}

func TestBuildPackageZipSrcIsFile(t *testing.T) {
	dir := t.TempDir()
	cliWriteFile(t, filepath.Join(dir, "src"), "not a dir", 0644)
	if err := buildPackageZip(io.Discard, dir, nil); err == nil {
		t.Fatal("expected error when src is a file")
	}
}

// Regression: an unreadable file inside src used to be reported as
// "src directory not found" and publish carried on with a partial package.
func TestBuildPackageZipUnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file permissions not enforced")
	}
	dir := t.TempDir()
	cliWriteFile(t, filepath.Join(dir, "src", "secret.luau"), "x", 0000)
	if err := buildPackageZip(io.Discard, dir, nil); err == nil {
		t.Fatal("expected error for unreadable file")
	}
}

func TestBuildPackageZipSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	cliWriteFile(t, filepath.Join(dir, "shared", "lib.luau"), "lib", 0644)
	cliWriteFile(t, filepath.Join(dir, "single.luau"), "single", 0644)
	cliWriteFile(t, filepath.Join(dir, "src", "init.luau"), "init", 0644)
	if err := os.Symlink(filepath.Join(dir, "shared"), filepath.Join(dir, "src", "shared")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "single.luau"), filepath.Join(dir, "src", "linked.luau")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := buildPackageZip(&buf, dir, nil); err != nil {
		t.Fatal(err)
	}
	got := cliZipEntries(t, buf.Bytes())
	if got["src/shared/lib.luau"] != "lib" || got["src/linked.luau"] != "single" || got["src/init.luau"] != "init" {
		t.Fatalf("entries = %v", got)
	}

	// A link back to an ancestor must fail instead of recursing forever.
	if err := os.Symlink(filepath.Join(dir, "src"), filepath.Join(dir, "src", "loop")); err != nil {
		t.Fatal(err)
	}
	if err := buildPackageZip(io.Discard, dir, nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want symlink cycle", err)
	}
}

type cliFailWriter struct{ n int }

func (w *cliFailWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, errors.New("disk full")
	}
	if len(p) > w.n {
		n := w.n
		w.n = 0
		return n, errors.New("disk full")
	}
	w.n -= len(p)
	return len(p), nil
}

// Regression: the zip writer's Close error (central directory write) used to be dropped.
func TestBuildPackageZipWriteErrorPropagated(t *testing.T) {
	dir := t.TempDir()
	cliWriteFile(t, filepath.Join(dir, "src", "a.luau"), "a", 0644)
	for _, limit := range []int{0, 10, 100} {
		if err := buildPackageZip(&cliFailWriter{n: limit}, dir, []byte("m")); err == nil {
			t.Errorf("limit %d: expected write error", limit)
		}
	}
}

func TestReplaceFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	cliWriteFile(t, p, "old", 0644)
	if err := replaceFileAtomic(p, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := cliReadFile(t, p); got != "new" {
		t.Fatalf("content = %q", got)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0600 {
			t.Fatalf("mode = %v", fi.Mode().Perm())
		}
	}
	if err := replaceFileAtomic(filepath.Join(dir, "missing", "f.txt"), []byte("x"), 0644); err == nil {
		t.Fatal("expected error for missing directory")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestInstallBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "chainlink-darwin-arm64")
	dst := filepath.Join(dir, "bin", "chainlink")
	cliWriteFile(t, src, "new binary", 0644)
	cliWriteFile(t, dst, "old binary", 0755)

	// Keep the old file open, as a running `chainlink serve` would.
	old, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	if err := installBinary(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := cliReadFile(t, dst); got != "new binary" {
		t.Fatalf("dst = %q", got)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(dst)
		if fi.Mode().Perm() != 0755 {
			t.Fatalf("mode = %v", fi.Mode().Perm())
		}
		// The running copy must not have been rewritten in place.
		b, _ := io.ReadAll(old)
		if string(b) != "old binary" {
			t.Fatalf("open file rewritten in place: %q", b)
		}
	}

	// Installing a binary onto itself must not truncate it.
	if err := installBinary(dst, dst); err != nil {
		t.Fatal(err)
	}
	if got := cliReadFile(t, dst); got != "new binary" {
		t.Fatalf("self-install changed content: %q", got)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(dst, link); err != nil {
			t.Fatal(err)
		}
		if err := installBinary(link, dst); err != nil {
			t.Fatal(err)
		}
		if got := cliReadFile(t, dst); got != "new binary" {
			t.Fatalf("symlinked self-install changed content: %q", got)
		}
	}

	if err := installBinary(filepath.Join(dir, "nope"), dst); err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestClassifyInvocation(t *testing.T) {
	tests := []struct {
		arg0 string
		mode invocationMode
		name string
	}{
		{"chainlink", invokeCLI, "chainlink"},
		{"/usr/local/bin/chainlink", invokeCLI, "chainlink"},
		{"chainlink.exe", invokeCLI, "chainlink"},
		{"ChainLink", invokeCLI, "ChainLink"},
		{"CHAINLINK.EXE", invokeCLI, "CHAINLINK"},
		{"chainlink-darwin-arm64", invokeGlobalInstall, "chainlink-darwin-arm64"},
		{"chainlink-windows-amd64.exe", invokeGlobalInstall, "chainlink-windows-amd64"},
		{"selene", invokeToolProxy, "selene"},
		{"/home/u/.chainlink/bin/StyLua", invokeToolProxy, "StyLua"},
		{"selene.exe", invokeToolProxy, "selene"},
		{"chainlinker", invokeToolProxy, "chainlinker"},
	}
	for _, tt := range tests {
		mode, name := classifyInvocation(tt.arg0)
		if mode != tt.mode || name != tt.name {
			t.Errorf("classifyInvocation(%q) = %v, %q; want %v, %q", tt.arg0, mode, name, tt.mode, tt.name)
		}
	}
}

func TestResolveToolSpec(t *testing.T) {
	cfg := &ChainLinkConfig{Tools: map[string]string{
		"selene": "Kampfkarren/selene@0.27.1",
		"StyLua": "JohnnyMorganz/StyLua@0.20.0",
	}}
	tests := []struct {
		name string
		want string
	}{
		{"selene", "Kampfkarren/selene@0.27.1"},
		{"SELENE", "Kampfkarren/selene@0.27.1"},
		{"stylua", "JohnnyMorganz/StyLua@0.20.0"},
	}
	for _, tt := range tests {
		got, err := resolveToolSpec(cfg, tt.name)
		if err != nil || got != tt.want {
			t.Errorf("resolveToolSpec(%q) = %q, %v", tt.name, got, err)
		}
	}
	if _, err := resolveToolSpec(cfg, "rojo"); !errors.Is(err, errToolNotDefined) {
		t.Errorf("missing tool err = %v, want errToolNotDefined", err)
	}
	if _, err := resolveToolSpec(&ChainLinkConfig{}, "x"); !errors.Is(err, errToolNotDefined) {
		t.Errorf("nil Tools err = %v", err)
	}

	exact := &ChainLinkConfig{Tools: map[string]string{"Foo": "a/Foo@1", "foo": "a/foo@2"}}
	for i := 0; i < 20; i++ {
		if got, _ := resolveToolSpec(exact, "foo"); got != "a/foo@2" {
			t.Fatalf("exact match not preferred: %q", got)
		}
	}
}

// Regression: every proxy failure used to be reported as "not defined in chain.toml".
func TestProxyToToolErrors(t *testing.T) {
	cliSetHome(t)
	cliChdirTemp(t)

	err := proxyToTool("selene", nil)
	if !errors.Is(err, errToolNotDefined) || !strings.Contains(err.Error(), "config missing") {
		t.Fatalf("missing config err = %v", err)
	}

	cliWriteFile(t, "chain.toml", "[Tools\n", 0644)
	if err := proxyToTool("selene", nil); err == nil || errors.Is(err, errToolNotDefined) {
		t.Fatalf("malformed config err = %v, want a parse error", err)
	}

	cliWriteFile(t, "chain.toml", "[Tools]\nselene = \"Kampfkarren/selene\"\n", 0644)
	err = proxyToTool("selene", nil)
	if err == nil || errors.Is(err, errToolNotDefined) || !strings.Contains(err.Error(), "invalid tool format") {
		t.Fatalf("bad spec err = %v", err)
	}

	err = proxyToTool("rojo", nil)
	if !errors.Is(err, errToolNotDefined) {
		t.Fatalf("undefined tool err = %v", err)
	}
}

// cliMakeExec creates an executable file (with .exe on Windows when asked).
func cliMakeExec(t testing.TB, path string) {
	t.Helper()
	cliWriteFile(t, path, "#!/bin/sh\nexit 0\n", 0755)
}

func TestFindToolBinarySkipsDocs(t *testing.T) {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	t.Run("prefers tool name over LICENSE", func(t *testing.T) {
		dir := t.TempDir()
		// makeBinariesExecutable marks every extensionless file executable.
		cliMakeExec(t, filepath.Join(dir, "LICENSE"))
		cliMakeExec(t, filepath.Join(dir, "README"))
		cliMakeExec(t, filepath.Join(dir, "selene"+exe))
		got, err := findToolBinary(dir, "selene", runtime.GOOS)
		if err != nil || got != filepath.Join(dir, "selene"+exe) {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("nested and case-insensitive", func(t *testing.T) {
		dir := t.TempDir()
		cliMakeExec(t, filepath.Join(dir, "CHANGELOG"))
		cliMakeExec(t, filepath.Join(dir, "stylua-macos", "stylua"+exe))
		got, err := findToolBinary(dir, "StyLua", runtime.GOOS)
		if err != nil || got != filepath.Join(dir, "stylua-macos", "stylua"+exe) {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("fallback skips docs", func(t *testing.T) {
		dir := t.TempDir()
		cliMakeExec(t, filepath.Join(dir, "LICENSE-MIT"))
		cliMakeExec(t, filepath.Join(dir, "rojo-cli"+exe))
		got, err := findToolBinary(dir, "rojo", runtime.GOOS)
		if err != nil || got != filepath.Join(dir, "rojo-cli"+exe) {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("only docs", func(t *testing.T) {
		dir := t.TempDir()
		cliMakeExec(t, filepath.Join(dir, "LICENSE"))
		cliWriteFile(t, filepath.Join(dir, "notes.md"), "x", 0644)
		if _, err := findToolBinary(dir, "selene", runtime.GOOS); !errors.Is(err, errToolBinaryNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty dir", func(t *testing.T) {
		if _, err := findToolBinary(t.TempDir(), "selene", runtime.GOOS); !errors.Is(err, errToolBinaryNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing dir", func(t *testing.T) {
		if _, err := findToolBinary(filepath.Join(t.TempDir(), "nope"), "selene", runtime.GOOS); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v", err)
		}
	})
	if runtime.GOOS != "windows" {
		t.Run("non-executable name match ignored", func(t *testing.T) {
			dir := t.TempDir()
			cliWriteFile(t, filepath.Join(dir, "selene"), "x", 0644)
			if _, err := findToolBinary(dir, "selene", runtime.GOOS); !errors.Is(err, errToolBinaryNotFound) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// Regression: a tool installed under the 'v'-prefixed tag dir was never found by
// runToolBySpec, which kept looking in the plain tag dir.
func TestRunToolBySpecVPrefixedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the tool binary")
	}
	home := cliSetHome(t)
	toolDir := filepath.Join(home, ".chainlink", "tools", "StyLua", "v0.20.0")
	cliMakeExec(t, filepath.Join(toolDir, "LICENSE"))
	out := filepath.Join(t.TempDir(), "ran")
	cliWriteFile(t, filepath.Join(toolDir, "stylua"), "#!/bin/sh\necho \"$@\" > \""+out+"\"\n", 0755)

	if err := runToolBySpec("JohnnyMorganz/StyLua@0.20.0", "", []string{"--check", "src"}); err != nil {
		t.Fatal(err)
	}
	if got := cliReadFile(t, out); got != "--check src\n" {
		t.Fatalf("tool args = %q", got)
	}
	if err := runToolBySpec("JohnnyMorganz/StyLua", "", nil); err == nil {
		t.Fatal("expected invalid spec error")
	}
}

func cliBenchTree(b *testing.B) string {
	b.Helper()
	dir := b.TempDir()
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	if err := os.MkdirAll(filepath.Join(dir, "packages"), 0755); err != nil {
		b.Fatal(err)
	}
	for d := 0; d < 10; d++ {
		for f := 0; f < 20; f++ {
			cliWriteFile(b, filepath.Join(dir, "src", fmt.Sprintf("d%d", d), fmt.Sprintf("m%d.luau", f)), string(payload), 0644)
		}
	}
	return dir
}

// BenchmarkPublishPackaging measures building the publish archive for a
// 200-file src tree. Before: copy src to a temp dir, zip it to a temp file,
// read the zip back (~34ms/op, 9.0MB/op, 7.7k allocs/op on an M2). After:
// zip straight from src into memory (~4.6ms/op, 7.2MB/op, 5.4k allocs/op).
func BenchmarkPublishPackaging(b *testing.B) {
	dir := cliBenchTree(b)
	manifest, err := buildPackageManifest(PackageInfo{Name: "a/b", Version: "1.0.0"}, defaultRegistry)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := buildPackageZip(&buf, dir, manifest); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkContainsUppercase(b *testing.B) {
	for i := 0; i < b.N; i++ {
		containsUppercase("sleitnick/signal-very-long-package-name@2.0.1-rc.1")
	}
}

func BenchmarkFindToolBinary(b *testing.B) {
	dir := b.TempDir()
	for _, n := range []string{"LICENSE", "README", "CHANGELOG", "selene"} {
		cliMakeExec(b, filepath.Join(dir, n))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := findToolBinary(dir, "selene", runtime.GOOS); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSourcemapOutput(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"-o", "sourcemap.json"}, "sourcemap.json"},
		{[]string{"chain.toml", "--output", "out.json", "--include-non-scripts"}, "out.json"},
		{[]string{"--watch", "--output=a b.json"}, "a b.json"},
		{[]string{"-o"}, ""},
	}
	for _, tt := range tests {
		if got := sourcemapOutput(tt.args); got != tt.want {
			t.Errorf("sourcemapOutput(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

// Regression: a tag from chain.toml must never resolve outside ~/.chainlink/tools.
func TestRunToolBySpecRejectsTraversal(t *testing.T) {
	home := cliSetHome(t)
	keep := filepath.Join(home, "keep.txt")
	cliWriteFile(t, keep, "x", 0644)
	for _, spec := range []string{"x/x@../../..", "x/x@..", "x/x@a/b", `x/x@a\b`, "../x@1.0"} {
		if err := runToolBySpec(spec, "", nil); err == nil {
			t.Errorf("runToolBySpec(%q) succeeded", spec)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("file outside tools dir was touched: %v", err)
	}
}

func TestHandleInitWritesTemplate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cliSetHome(t)
	dir := cliChdirTemp(t)
	if err := handleInit(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"client/ExampleController.luau": "function mod:init()",
		"server/ExampleService.luau":    "function mod:init()",
		"shared/ExampleData.json":       `"hello": "world"`,
		".vscode/settings.json":         `"luau-lsp.sourcemap.rojoPath": "chainlink"`,
		".vscode/globals.d.luau":        "declare use: (module: any) -> any",
	}
	for rel, snippet := range want {
		if got := cliReadFile(t, filepath.Join(dir, filepath.FromSlash(rel))); !strings.Contains(got, snippet) {
			t.Errorf("%s = %q, want it to contain %q", rel, got, snippet)
		}
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(cliReadFile(t, filepath.Join(dir, ".vscode", "settings.json"))), &settings); err != nil {
		t.Errorf(".vscode/settings.json is not valid JSON: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "chain.toml")); err != nil {
		t.Errorf("chain.toml not created: %v", err)
	}
	if err := handleInit(); err == nil {
		t.Error("second init should refuse to overwrite chain.toml")
	}
}
