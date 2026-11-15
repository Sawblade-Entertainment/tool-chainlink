package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// guiTestServer returns a UI server with a fake home, run from a temp project dir.
func guiTestServer(t *testing.T) (*guiServer, string, string) {
	t.Helper()
	home := serveFakeHome(t)
	t.Setenv("ROBLOX_API_KEY", "")
	dir := t.TempDir()
	t.Chdir(dir)
	g, err := newGUIServer(home)
	if err != nil {
		t.Fatal(err)
	}
	g.goos = "darwin"
	g.health = func(int, string) bool { return false }
	return g, home, dir
}

func guiGet(t *testing.T, h http.Handler, host, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = host
	if token != "" {
		req.Header.Set(guiTokenHeader, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGUIRejectsForeignHost(t *testing.T) {
	g, _, _ := guiTestServer(t)
	h := g.handler()
	for _, path := range []string{"/", "/app.js", "/api/status"} {
		if rec := guiGet(t, h, "evil.example:8080", path, g.token); rec.Code != http.StatusForbidden {
			t.Errorf("%s from foreign host: got %d, want 403", path, rec.Code)
		}
	}
}

func TestGUIStatusNeedsToken(t *testing.T) {
	g, _, _ := guiTestServer(t)
	h := g.handler()
	for _, tok := range []string{"", "wrong", g.token + "x"} {
		if rec := guiGet(t, h, "127.0.0.1:1234", "/api/status", tok); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: got %d, want 401", tok, rec.Code)
		}
	}
	rec := guiGet(t, h, "127.0.0.1:1234", "/api/status", g.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: got %d, want 200", rec.Code)
	}
	var st guiStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != chainlinkVersion {
		t.Errorf("version = %q, want %q", st.Version, chainlinkVersion)
	}
}

func TestGUIServesPageWithHeaders(t *testing.T) {
	g, _, _ := guiTestServer(t)
	h := g.handler()
	for _, host := range []string{"127.0.0.1:1234", "localhost:1234", "[::1]:1234"} {
		rec := guiGet(t, h, host, "/", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app.js") {
			t.Fatalf("GET / on %s: %d %q", host, rec.Code, rec.Body.String())
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("CSP = %q", csp)
		}
	}
	for _, path := range []string{"/app.js", "/app.css", "/chainlink.svg"} {
		if rec := guiGet(t, h, "127.0.0.1:1234", path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
	}
}

func TestGUIStatusWithoutProject(t *testing.T) {
	g, _, dir := guiTestServer(t)
	st := g.status()
	if st.Project != nil || st.Error != "" {
		t.Fatalf("no chain.toml: project=%v error=%q", st.Project, st.Error)
	}
	if real, _ := filepath.EvalSymlinks(dir); st.Dir != dir && st.Dir != real {
		t.Errorf("dir = %q, want %q", st.Dir, dir)
	}
}

func TestGUIStatusBadChainToml(t *testing.T) {
	g, _, _ := guiTestServer(t)
	writeTestFile(t, chainTomlName, "[Tools\n")
	if st := g.status(); st.Error == "" || st.Project != nil {
		t.Fatalf("broken chain.toml: project=%v error=%q", st.Project, st.Error)
	}
}

func TestGUIStatusProject(t *testing.T) {
	g, home, _ := guiTestServer(t)
	writeTestFile(t, chainTomlName, `
[Package]
  Name = "Demo"
  Version = "1.2.0"

[Tools]
  selene = "Kampfkarren/selene@0.32.0"
  stylua = "JohnnyMorganz/StyLua@v2.0.0"
  broken = "nope"

[Packages]
  razor = "vibeonline/razorinput@0.1.4"

[Roblox]
  PlaceId = 42
  GroupId = 7
`)
	mkdirAll(t, filepath.Join(home, ".chainlink", "tools", "selene", "0.32.0"))
	mkdirAll(t, packageInstallDir(home, "vibeonline", "razorinput", "0.1.4"))

	// One uploaded, one changed since upload, one new, one unsupported.
	writeTestFile(t, "assets/a.png", "a")
	writeTestFile(t, "assets/b.png", "b2")
	writeTestFile(t, "assets/sfx/c.ogg", "c")
	writeTestFile(t, "assets/notes.txt", "x")
	ha, _ := hashFile("assets/a.png")
	lock := assetLock{"a.png": {ID: "1", Hash: ha}, "b.png": {ID: "2", Hash: "old"}}
	if err := lock.save(assetLockName); err != nil {
		t.Fatal(err)
	}

	var probedPort int
	g.health = func(port int, _ string) bool { probedPort = port; return true }

	p := g.status().Project
	if p == nil {
		t.Fatal("project missing")
	}
	if p.Name != "Demo" || p.Version != "1.2.0" {
		t.Errorf("package = %q %q", p.Name, p.Version)
	}

	tools := map[string]guiDep{}
	for _, d := range p.Tools {
		tools[d.Alias] = d
	}
	if !tools["selene"].Installed || tools["stylua"].Installed {
		t.Errorf("tool install state wrong: %+v", p.Tools)
	}
	if tools["broken"].Error == "" {
		t.Errorf("invalid tool spec not reported: %+v", tools["broken"])
	}
	if !slices.IsSortedFunc(p.Tools, func(a, b guiDep) int { return strings.Compare(a.Alias, b.Alias) }) {
		t.Errorf("tools not sorted: %+v", p.Tools)
	}
	if len(p.Packages) != 1 || !p.Packages[0].Installed {
		t.Errorf("packages = %+v", p.Packages)
	}

	if !p.Sync.Running || p.Sync.Port != 8013 || probedPort != 8013 {
		t.Errorf("sync = %+v (probed %d)", p.Sync, probedPort)
	}
	if p.Roblox.PlaceId != 42 || p.Roblox.GroupId != 7 {
		t.Errorf("roblox = %+v", p.Roblox)
	}
	want := guiAssets{Total: 3, Uploaded: 1, Pending: 2, Skipped: 1}
	if p.Assets == nil || *p.Assets != want {
		t.Errorf("assets = %+v, want %+v", p.Assets, want)
	}
}

func TestGUIHashCacheNoticesChanges(t *testing.T) {
	g, _, _ := guiTestServer(t)
	writeTestFile(t, chainTomlName, "[Package]\nName = \"x\"\n")
	writeTestFile(t, "assets/a.png", "one")
	h1, _ := hashFile("assets/a.png")
	if err := (assetLock{"a.png": {ID: "1", Hash: h1}}).save(assetLockName); err != nil {
		t.Fatal(err)
	}
	if a := g.assets(); a.Pending != 0 || a.Uploaded != 1 {
		t.Fatalf("before change: %+v", a)
	}
	writeTestFile(t, "assets/a.png", "two, longer")
	if a := g.assets(); a.Pending != 1 {
		t.Fatalf("after change: %+v", a)
	}
}

func TestGUISetup(t *testing.T) {
	g, home, _ := guiTestServer(t)
	binDir := filepath.Join(home, ".chainlink", "bin")
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SHELL", "/bin/zsh")

	s := g.setup()
	if s.BinInstalled || s.OnPath || s.PathPending || s.PluginInstalled || s.CloudKey {
		t.Fatalf("fresh machine: %+v", s)
	}

	writeTestFile(t, filepath.Join(binDir, "chainlink"), "bin")
	writeTestFile(t, filepath.Join(home, ".zshrc"), "export PATH=\""+binDir+":$PATH\"\n")
	writeTestFile(t, filepath.Join(home, "Documents", "Roblox", "Plugins", "ChainLink.rbxmx"), "<roblox/>")
	t.Setenv("ROBLOX_API_KEY", "k")

	s = g.setup()
	if !s.BinInstalled || s.OnPath || !s.PathPending || !s.PluginInstalled || !s.CloudKey {
		t.Fatalf("after install, before new shell: %+v", s)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin")
	if s = g.setup(); !s.OnPath || s.PathPending {
		t.Fatalf("on PATH: %+v", s)
	}
}

func TestOpenBrowserCommand(t *testing.T) {
	url := "http://127.0.0.1:1/#token=ab"
	for goos, want := range map[string][]string{
		"darwin":  {"open", url},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", url},
		"linux":   {"xdg-open", url},
	} {
		if got := openBrowserCommand(goos, url).Args; !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", goos, got, want)
		}
	}
}

func TestHandleUIRejectsExtraArgs(t *testing.T) {
	if err := handleUI([]string{"extra"}); err == nil {
		t.Fatal("expected an error for an extra argument")
	}
}

func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}
