package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParsePackageSpec(t *testing.T) {
	tests := []struct {
		spec                 string
		scope, name, version string
		wantErr              bool
	}{
		{"vibeonline/razorinput@0.1.0", "vibeonline", "razorinput", "0.1.0", false},
		{"sleitnick/knit@1.7.0", "sleitnick", "knit", "1.7.0", false},
		{"vibeonline/razorinput", "", "", "", true},
		{"vibeonline/razorinput@0.1.0@1", "", "", "", true},
		{"razorinput@0.1.0", "", "", "", true},
		{"a/b/c@1", "", "", "", true},
		{"/razorinput@1", "", "", "", true},
		{"scope/@1", "", "", "", true},
		{"scope/name@", "", "", "", true},
		{"scope/name@../../x", "", "", "", true},
		{"scope/name@..", "", "", "", true},
		{"scope/name@1/2", "", "", "", true},
		{`scope/name@1\2`, "", "", "", true},
		{"../name@1", "", "", "", true},
		{`sc\ope/name@1`, "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			scope, name, version, err := parsePackageSpec(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePackageSpec(%q) err = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if scope != tt.scope || name != tt.name || version != tt.version {
				t.Errorf("parsePackageSpec(%q) = %q, %q, %q", tt.spec, scope, name, version)
			}
		})
	}
}

// installFakePackageRegistry serves package zips by "scope/name/version".
type installFakePackageRegistry struct {
	pkgs      map[string][]byte
	fail      atomic.Bool
	requests  atomic.Int64
	userAgent atomic.Value
}

func installNewFakePackageRegistry(t testing.TB) *installFakePackageRegistry {
	t.Helper()
	w := &installFakePackageRegistry{pkgs: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.requests.Add(1)
		w.userAgent.Store(r.Header.Get("User-Agent") + " " + r.Header.Get("Wally-Version"))
		if w.fail.Load() {
			http.Error(rw, "down", http.StatusServiceUnavailable)
			return
		}
		body, ok := w.pkgs[strings.TrimPrefix(r.URL.Path, "/v1/package-contents/")]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		rw.Write(body)
	}))
	t.Cleanup(srv.Close)
	old := packageRegistryAPI
	packageRegistryAPI = srv.URL + "/v1/package-contents"
	t.Cleanup(func() { packageRegistryAPI = old })
	return w
}

func installPackageHome(t testing.TB) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestInstallPackage(t *testing.T) {
	home := installPackageHome(t)
	w := installNewFakePackageRegistry(t)
	w.pkgs["sleitnick/knit/1.7.0"] = installMakeZip(t, []installEntry{
		{Name: "wally.toml", Body: "[package]"},
		{Name: "src/", Dir: true},
		{Name: "src/init.luau", Body: "return {}"},
	})

	if err := InstallPackage("Knit", "sleitnick/knit@1.7.0"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".chainlink", "packages", "sleitnick_knit", "1.7.0")
	if got := installReadFile(t, filepath.Join(dir, "src", "init.luau")); got != "return {}" {
		t.Errorf("init.luau = %q", got)
	}
	if got := w.userAgent.Load(); got != "wally/0.3.2 0.3.2" {
		t.Errorf("headers = %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".chainlink", ".staging"))
	if len(entries) != 0 {
		t.Errorf("staging not cleaned: %v", entries)
	}
}

// Regression: every package was downloaded before the installed check.
func TestInstallPackageSkipsInstalledWithoutRequest(t *testing.T) {
	home := installPackageHome(t)
	w := installNewFakePackageRegistry(t)
	os.MkdirAll(filepath.Join(home, ".chainlink", "packages", "sleitnick_knit", "1.7.0"), 0755)

	if err := InstallPackage("Knit", "sleitnick/knit@1.7.0"); err != nil {
		t.Fatal(err)
	}
	if n := w.requests.Load(); n != 0 {
		t.Errorf("made %d requests for an installed package", n)
	}
}

// Regression: a failed extract used to leave the version dir behind, and the
// next install skipped the broken package.
func TestInstallPackageFailureRetries(t *testing.T) {
	home := installPackageHome(t)
	w := installNewFakePackageRegistry(t)
	dir := filepath.Join(home, ".chainlink", "packages", "sleitnick_knit", "1.7.0")

	w.pkgs["sleitnick/knit/1.7.0"] = []byte("not a zip")
	if err := InstallPackage("Knit", "sleitnick/knit@1.7.0"); err == nil {
		t.Fatal("expected extract error")
	}
	if installExists(dir) {
		t.Fatal("failed install left the version dir behind")
	}

	w.fail.Store(true)
	if err := InstallPackage("Knit", "sleitnick/knit@1.7.0"); err == nil {
		t.Fatal("expected download error")
	}
	if installExists(dir) {
		t.Fatal("failed download left the version dir behind")
	}

	w.fail.Store(false)
	w.pkgs["sleitnick/knit/1.7.0"] = installMakeZip(t, []installEntry{{Name: "init.luau", Body: "ok"}})
	if err := InstallPackage("Knit", "sleitnick/knit@1.7.0"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := installReadFile(t, filepath.Join(dir, "init.luau")); got != "ok" {
		t.Errorf("init.luau = %q", got)
	}
	if n := w.requests.Load(); n != 3 {
		t.Errorf("requests = %d, want 3", n)
	}
}

func TestInstallPackageRejectsTraversal(t *testing.T) {
	home := installPackageHome(t)
	w := installNewFakePackageRegistry(t)
	w.pkgs["evil/pkg/1.0.0"] = installMakeZip(t, []installEntry{
		{Name: "init.luau", Body: "ok"},
		{Name: "../../../../evil.luau", Body: "pwned"},
	})
	if err := InstallPackage("Evil", "evil/pkg@1.0.0"); err == nil {
		t.Fatal("expected traversal error")
	}
	if installExists(filepath.Join(home, ".chainlink", "packages", "evil_pkg", "1.0.0")) {
		t.Error("version dir created")
	}
	for _, p := range []string{filepath.Join(home, "evil.luau"), filepath.Join(home, ".chainlink", "evil.luau")} {
		if installExists(p) {
			t.Errorf("wrote %s", p)
		}
	}
}

func TestInstallPackageRejectsBadSpec(t *testing.T) {
	installPackageHome(t)
	w := installNewFakePackageRegistry(t)
	for _, spec := range []string{"scope/name@../../x", "scope/name", "../x@1"} {
		if err := InstallPackage("x", spec); err == nil {
			t.Errorf("InstallPackage(%q) succeeded", spec)
		}
	}
	if n := w.requests.Load(); n != 0 {
		t.Errorf("made %d requests for invalid specs", n)
	}
}

func TestInstallPackageList(t *testing.T) {
	home := installPackageHome(t)
	w := installNewFakePackageRegistry(t)
	pkgs := map[string]string{}
	for _, n := range []string{"a", "b", "c"} {
		w.pkgs["s/"+n+"/1.0.0"] = installMakeZip(t, []installEntry{{Name: "init.luau", Body: n}})
		pkgs[strings.ToUpper(n)] = "s/" + n + "@1.0.0"
	}
	if err := InstallPackageList(pkgs); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		if got := installReadFile(t, filepath.Join(home, ".chainlink", "packages", "s_"+n, "1.0.0", "init.luau")); got != n {
			t.Errorf("%s = %q", n, got)
		}
	}
	// A second run is all skips.
	before := w.requests.Load()
	if err := InstallPackageList(pkgs); err != nil {
		t.Fatal(err)
	}
	if n := w.requests.Load() - before; n != 0 {
		t.Errorf("re-install made %d requests", n)
	}
}

// BenchmarkInstallPackageInstalled is the common `chainlink install` path:
// the package is already in ~/.chainlink/packages.
func BenchmarkInstallPackageInstalled(b *testing.B) {
	home := installPackageHome(b)
	w := installNewFakePackageRegistry(b)
	w.pkgs["sleitnick/knit/1.7.0"] = installMakeZip(b, installBenchEntries())
	os.MkdirAll(filepath.Join(home, ".chainlink", "packages", "sleitnick_knit", "1.7.0"), 0755)

	stdout := os.Stdout
	os.Stdout, _ = os.Open(os.DevNull)
	defer func() { os.Stdout = stdout }()

	b.ReportAllocs()
	for b.Loop() {
		if err := InstallPackage("Knit", "sleitnick/knit@1.7.0"); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(w.requests.Load())/float64(b.N), "req/op")
}

// BenchmarkInstallPackageFresh measures a full download and extract.
func BenchmarkInstallPackageFresh(b *testing.B) {
	home := installPackageHome(b)
	w := installNewFakePackageRegistry(b)
	w.pkgs["sleitnick/knit/1.7.0"] = installMakeZip(b, installBenchEntries())
	pkgDir := filepath.Join(home, ".chainlink", "packages", "sleitnick_knit")

	stdout := os.Stdout
	os.Stdout, _ = os.Open(os.DevNull)
	defer func() { os.Stdout = stdout }()

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		os.RemoveAll(pkgDir)
		b.StartTimer()
		if err := InstallPackage("Knit", "sleitnick/knit@1.7.0"); err != nil {
			b.Fatal(err)
		}
	}
}
