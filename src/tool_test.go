package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ulikunitz/xz"
)

// installEntry describes one archive member for the in-memory archive builders.
type installEntry struct {
	Name     string
	Body     string
	Mode     os.FileMode
	Dir      bool
	Symlink  string // tar/zip symlink target
	Hardlink string // tar hard link target
}

func installMakeZip(t testing.TB, entries []installEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.Name, Method: zip.Deflate}
		switch {
		case e.Dir:
			h.SetMode(os.ModeDir | 0755)
		case e.Symlink != "":
			h.SetMode(os.ModeSymlink | 0777)
		default:
			mode := e.Mode
			if mode == 0 {
				mode = 0644
			}
			h.SetMode(mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		body := e.Body
		if e.Symlink != "" {
			body = e.Symlink
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installMakeTar builds a tar archive compressed with "gz" or "xz".
func installMakeTar(t testing.TB, entries []installEntry, compression string) []byte {
	t.Helper()
	var buf bytes.Buffer
	var cw io.WriteCloser
	switch compression {
	case "gz":
		cw = gzip.NewWriter(&buf)
	case "xz":
		w, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		cw = w
	default:
		t.Fatalf("unknown compression %q", compression)
	}
	tw := tar.NewWriter(cw)
	for _, e := range entries {
		h := &tar.Header{Name: e.Name, Mode: int64(e.Mode)}
		switch {
		case e.Dir:
			h.Typeflag = tar.TypeDir
			h.Mode = 0755
		case e.Symlink != "":
			h.Typeflag = tar.TypeSymlink
			h.Linkname = e.Symlink
		case e.Hardlink != "":
			h.Typeflag = tar.TypeLink
			h.Linkname = e.Hardlink
		default:
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.Body))
			if h.Mode == 0 {
				h.Mode = 0644
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(tw, e.Body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func installWriteFile(t testing.TB, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is filtered by umask; set it exactly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func installReadFile(t testing.TB, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func installExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// installTestHome points HOME at a temp dir and stubs the executable used as
// the symlink target. It returns the home dir and the fake executable path.
func installTestHome(t *testing.T) (home, exe string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GITHUB_TOKEN", "")

	exe = filepath.Join(t.TempDir(), "chainlink-test")
	installWriteFile(t, exe, []byte("#!/bin/sh\n"), 0755)
	// The temp dir may sit behind a symlink (/var -> /private/var on macOS),
	// and symlink targets are resolved.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	oldExe := installExecutable
	installExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { installExecutable = oldExe })
	return home, exe
}

// installFakeGitHub serves releases and asset downloads for one repo.
type installFakeGitHub struct {
	srv       *httptest.Server
	repo      string
	tags      map[string][]string // tag -> asset names
	files     map[string][]byte   // asset name -> body
	failDL    atomic.Bool
	requests  atomic.Int64
	downloads atomic.Int64
	apiAuth   atomic.Value // Authorization header of the last API request
	dlAuth    atomic.Value // Authorization header of the last download
}

func installNewFakeGitHub(t *testing.T, repo string) *installFakeGitHub {
	t.Helper()
	g := &installFakeGitHub{repo: repo, tags: map[string][]string{}, files: map[string][]byte{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		prefix := "/repos/" + g.repo + "/releases/tags/"
		switch {
		case strings.HasPrefix(r.URL.Path, prefix):
			g.apiAuth.Store(r.Header.Get("Authorization"))
			names, ok := g.tags[strings.TrimPrefix(r.URL.Path, prefix)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			var rel GitHubRelease
			for _, n := range names {
				rel.Assets = append(rel.Assets, GitHubAsset{Name: n, BrowserDownloadURL: g.srv.URL + "/dl/" + n})
			}
			json.NewEncoder(w).Encode(rel)
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			g.downloads.Add(1)
			g.dlAuth.Store(r.Header.Get("Authorization"))
			if g.failDL.Load() {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			body, ok := g.files[strings.TrimPrefix(r.URL.Path, "/dl/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)

	oldBase := githubAPIBase
	githubAPIBase = g.srv.URL
	t.Cleanup(func() { githubAPIBase = oldBase })
	return g
}

// installHostAsset is a release asset name for the machine running the tests.
func installHostAsset(prefix, ext string) string {
	return fmt.Sprintf("%s-%s-%s%s", prefix, runtime.GOOS, runtime.GOARCH, ext)
}

func installBinName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func TestSelectAsset(t *testing.T) {
	assets := func(names ...string) []GitHubAsset {
		out := make([]GitHubAsset, len(names))
		for i, n := range names {
			out[i] = GitHubAsset{Name: n, BrowserDownloadURL: "https://example.invalid/" + n}
		}
		return out
	}
	tests := []struct {
		name    string
		assets  []GitHubAsset
		goos    string
		goarch  string
		want    string
		wantErr bool
	}{
		{"exact match wins over os-only", assets("tool-linux.zip", "tool-linux-arm64.zip"), "linux", "arm64", "tool-linux-arm64.zip", false},
		{"macos alias", assets("tool-windows-x86_64.zip", "tool-macos-aarch64.zip"), "darwin", "arm64", "tool-macos-aarch64.zip", false},
		{"darwin alias", assets("tool-x86_64-apple-darwin.tar.gz"), "darwin", "amd64", "tool-x86_64-apple-darwin.tar.gz", false},
		{"aarch64 is arm64", assets("tool-linux-x86_64.zip", "tool-linux-aarch64.zip"), "linux", "arm64", "tool-linux-aarch64.zip", false},
		{"x86_64 is amd64", assets("tool-linux-aarch64.zip", "tool-linux-x86_64.zip"), "linux", "amd64", "tool-linux-x86_64.zip", false},
		{"checksum listed first is skipped", assets("tool-linux-x86_64.zip.sha256", "tool-linux-x86_64.zip.sig", "tool-linux-x86_64.zip"), "linux", "amd64", "tool-linux-x86_64.zip", false},
		{"only checksums is an error", assets("tool-linux-x86_64.zip.sha256"), "linux", "amd64", "", true},
		{"os-only fallback", assets("tool-windows.zip", "tool-macos.zip"), "darwin", "arm64", "tool-macos.zip", false},
		{"os-only fallback skips other arch", assets("tool-linux-x86_64.zip", "tool-linux.zip"), "linux", "arm64", "tool-linux.zip", false},
		{"other arch only is an error", assets("tool-linux-x86_64.zip"), "linux", "arm64", "", true},
		// Regression: Apple Silicon and Windows on ARM run x64 builds under emulation.
		{"rosetta fallback", assets("tool-x86_64-apple-darwin.tar.gz", "tool-x86_64-linux.zip"), "darwin", "arm64", "tool-x86_64-apple-darwin.tar.gz", false},
		{"windows arm64 runs x64", assets("tool-windows-x64.zip"), "windows", "arm64", "tool-windows-x64.zip", false},
		{"native beats emulated", assets("tool-macos-x86_64.zip", "tool-macos-aarch64.zip"), "darwin", "arm64", "tool-macos-aarch64.zip", false},
		{"universal beats emulated", assets("tool-macos-x86_64.zip", "tool-macos.zip"), "darwin", "arm64", "tool-macos.zip", false},
		{"no emulation for amd64 hosts", assets("tool-macos-aarch64.zip"), "darwin", "amd64", "", true},
		{"no match", assets("tool-windows-x86_64.zip"), "linux", "amd64", "", true},
		{"empty", nil, "linux", "amd64", "", true},
		{"windows exe", assets("tool-linux-x86_64", "tool-windows-x86_64.exe"), "windows", "amd64", "tool-windows-x86_64.exe", false},
		{"windows zip", assets("tool-windows-x64.zip"), "windows", "amd64", "tool-windows-x64.zip", false},
		{"windows win64", assets("tool-darwin64.zip", "tool-win64.zip"), "windows", "amd64", "tool-win64.zip", false},
		{"darwin64 is not windows", assets("tool-darwin64.zip"), "windows", "amd64", "", true},
		{"archive preferred over raw", assets("tool-linux-x86_64.deb", "tool-linux-x86_64.tar.gz"), "linux", "amd64", "tool-linux-x86_64.tar.gz", false},
		{"case-insensitive", assets("Tool-Linux-X86_64.ZIP"), "linux", "amd64", "Tool-Linux-X86_64.ZIP", false},
		{"raw binary", assets("lune-0.8.0-linux-x86_64"), "linux", "amd64", "lune-0.8.0-linux-x86_64", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectAsset(tt.assets, tt.goos, tt.goarch)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("selectAsset = %q, want error", got.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectAsset error: %v", err)
			}
			if got.Name != tt.want {
				t.Errorf("selectAsset = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestParseToolSpec(t *testing.T) {
	tests := []struct {
		spec    string
		want    toolSpec
		wantErr bool
	}{
		{"Kampfkarren/selene@0.32.0", toolSpec{Repo: "Kampfkarren/selene", Name: "selene", Tag: "0.32.0"}, false},
		{"JohnnyMorganz/StyLua@v0.20.0", toolSpec{Repo: "JohnnyMorganz/StyLua", Name: "StyLua", Tag: "v0.20.0"}, false},
		{"Kampfkarren/selene", toolSpec{}, true},
		{"Kampfkarren/selene@1@2", toolSpec{}, true},
		{"selene@0.32.0", toolSpec{}, true},
		{"a/b/c@1", toolSpec{}, true},
		{"/selene@1", toolSpec{}, true},
		{"owner/@1", toolSpec{}, true},
		{"owner/repo@", toolSpec{}, true},
		{"@", toolSpec{}, true},
		{"owner/repo@../../etc", toolSpec{}, true},
		{"owner/repo@..", toolSpec{}, true},
		{"owner/repo@a/b", toolSpec{}, true},
		{`owner/repo@a\b`, toolSpec{}, true},
		{"owner/..@1", toolSpec{}, true},
		{"../repo@1", toolSpec{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := parseToolSpec(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseToolSpec(%q) err = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseToolSpec(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestInstallToolRejectsBadAlias(t *testing.T) {
	installTestHome(t)
	g := installNewFakeGitHub(t, "o/tool")
	for _, alias := range []string{"", "..", "../x", "a/b", `a\b`} {
		if _, err := InstallToolDir(alias, "o/tool@1.0.0"); err == nil {
			t.Errorf("InstallToolDir(%q) succeeded, want error", alias)
		}
	}
	if n := g.requests.Load(); n != 0 {
		t.Errorf("made %d requests for invalid aliases", n)
	}
}

func TestSafeJoin(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dest")
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{"a.txt", filepath.Join(dest, "a.txt"), false},
		{"bin/sub/tool", filepath.Join(dest, "bin", "sub", "tool"), false},
		{"bin/", filepath.Join(dest, "bin"), false},
		{"./a", filepath.Join(dest, "a"), false},
		{"a/../b", filepath.Join(dest, "b"), false},
		{`win\style\path`, filepath.Join(dest, "win", "style", "path"), false},
		{".", dest, false},
		{"", dest, false},
		{"./", dest, false},
		{"..", "", true},
		{"../x", "", true},
		{"a/../../x", "", true},
		{`..\x`, "", true},
		{"/etc/passwd", "", true},
		{`C:\x`, "", true},
		{"C:/x", "", true},
		{`\\server\share\x`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := safeJoin(dest, tt.name)
			if (err != nil) != tt.wantErr {
				t.Fatalf("safeJoin(%q) = %q, %v; wantErr %v", tt.name, got, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("safeJoin(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestExtractZip(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "a.zip")
	installWriteFile(t, src, installMakeZip(t, []installEntry{
		{Name: "emptydir/", Dir: true},
		{Name: "tool", Body: "binary", Mode: 0755},
		{Name: "docs/readme.md", Body: "hello", Mode: 0644},
		{Name: "a/b/c.txt", Body: "deep", Mode: 0600},
		{Name: "link", Symlink: "/etc/passwd"},
	}), 0644)
	dest := filepath.Join(root, "out")

	if err := extractZip(src, dest); err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dest, "emptydir")); err != nil || !info.IsDir() {
		t.Errorf("explicit dir entry not created: %v", err)
	}
	for name, want := range map[string]struct {
		body string
		mode os.FileMode
	}{
		"tool":           {"binary", 0755},
		"docs/readme.md": {"hello", 0644},
		"a/b/c.txt":      {"deep", 0600},
	} {
		p := filepath.Join(dest, filepath.FromSlash(name))
		if got := installReadFile(t, p); got != want.body {
			t.Errorf("%s = %q, want %q", name, got, want.body)
		}
		if runtime.GOOS != "windows" {
			info, _ := os.Stat(p)
			if info.Mode().Perm() != want.mode {
				t.Errorf("%s mode = %v, want %v", name, info.Mode().Perm(), want.mode)
			}
		}
	}
	if installExists(filepath.Join(dest, "link")) {
		t.Error("symlink entry should be skipped")
	}
}

func TestExtractZipTraversal(t *testing.T) {
	for _, evil := range []string{"../evil", "a/../../evil", "/abs/evil", `..\evil`} {
		t.Run(evil, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "a.zip")
			installWriteFile(t, src, installMakeZip(t, []installEntry{{Name: evil, Body: "pwned"}}), 0644)
			dest := filepath.Join(root, "out")
			if err := extractZip(src, dest); err == nil {
				t.Fatal("extractZip accepted a traversal entry")
			}
			if installExists(filepath.Join(root, "evil")) {
				t.Fatal("file written outside dest")
			}
		})
	}
}

func TestExtractTar(t *testing.T) {
	extractors := map[string]func(src, dest string) error{
		"gz": extractTarGz,
		"xz": extractTarXz,
	}
	for comp, extract := range extractors {
		t.Run(comp, func(t *testing.T) {
			t.Run("files", func(t *testing.T) {
				root := t.TempDir()
				src := filepath.Join(root, "a.tar")
				installWriteFile(t, src, installMakeTar(t, []installEntry{
					{Name: "pkg/", Dir: true},
					{Name: "pkg/tool", Body: "binary", Mode: 0755},
					{Name: "pkg/LICENSE", Body: "mit", Mode: 0644},
					{Name: "nodir/file.txt", Body: "x", Mode: 0640},
					{Name: "pkg/sym", Symlink: "/etc/passwd"},
					{Name: "pkg/hard", Hardlink: "pkg/tool"},
				}, comp), 0644)
				dest := filepath.Join(root, "out")
				if err := extract(src, dest); err != nil {
					t.Fatalf("extract: %v", err)
				}
				if got := installReadFile(t, filepath.Join(dest, "pkg", "tool")); got != "binary" {
					t.Errorf("tool = %q", got)
				}
				if got := installReadFile(t, filepath.Join(dest, "nodir", "file.txt")); got != "x" {
					t.Errorf("file.txt = %q", got)
				}
				if runtime.GOOS != "windows" {
					for name, mode := range map[string]os.FileMode{"pkg/tool": 0755, "pkg/LICENSE": 0644, "nodir/file.txt": 0640} {
						info, err := os.Stat(filepath.Join(dest, filepath.FromSlash(name)))
						if err != nil {
							t.Fatal(err)
						}
						if info.Mode().Perm() != mode {
							t.Errorf("%s mode = %v, want %v", name, info.Mode().Perm(), mode)
						}
					}
				}
				for _, link := range []string{"pkg/sym", "pkg/hard"} {
					if installExists(filepath.Join(dest, filepath.FromSlash(link))) {
						t.Errorf("%s should be skipped", link)
					}
				}
			})

			t.Run("traversal", func(t *testing.T) {
				for _, evil := range []string{"../evil", "a/../../evil", "/evil"} {
					root := t.TempDir()
					src := filepath.Join(root, "a.tar")
					installWriteFile(t, src, installMakeTar(t, []installEntry{
						{Name: "ok.txt", Body: "fine"},
						{Name: evil, Body: "pwned"},
					}, comp), 0644)
					if err := extract(src, filepath.Join(root, "out")); err == nil {
						t.Errorf("%q: extract accepted traversal entry", evil)
					}
					if installExists(filepath.Join(root, "evil")) {
						t.Errorf("%q: file written outside dest", evil)
					}
				}
			})

			t.Run("traversal dir", func(t *testing.T) {
				root := t.TempDir()
				src := filepath.Join(root, "a.tar")
				installWriteFile(t, src, installMakeTar(t, []installEntry{{Name: "../evildir/", Dir: true}}, comp), 0644)
				if err := extract(src, filepath.Join(root, "out")); err == nil {
					t.Error("extract accepted traversal dir")
				}
				if installExists(filepath.Join(root, "evildir")) {
					t.Error("dir created outside dest")
				}
			})

			t.Run("truncated", func(t *testing.T) {
				root := t.TempDir()
				data := installMakeTar(t, []installEntry{{Name: "big", Body: strings.Repeat("abcdefgh", 4096)}}, comp)
				src := filepath.Join(root, "a.tar")
				installWriteFile(t, src, data[:len(data)/2], 0644)
				if err := extract(src, filepath.Join(root, "out")); err == nil {
					t.Error("extract of truncated archive succeeded")
				}
			})
		})
	}
}

func TestExtractSizeLimit(t *testing.T) {
	old := maxExtractBytes
	maxExtractBytes = 10
	t.Cleanup(func() { maxExtractBytes = old })

	root := t.TempDir()
	src := filepath.Join(root, "a.zip")
	installWriteFile(t, src, installMakeZip(t, []installEntry{
		{Name: "a", Body: "123456"},
		{Name: "b", Body: "123456"},
	}), 0644)
	if err := extractZip(src, filepath.Join(root, "out")); err == nil {
		t.Error("extractZip ignored the size limit")
	}

	// Exactly at the limit is fine.
	src2 := filepath.Join(root, "b.zip")
	installWriteFile(t, src2, installMakeZip(t, []installEntry{{Name: "a", Body: "1234567890"}}), 0644)
	if err := extractZip(src2, filepath.Join(root, "out2")); err != nil {
		t.Errorf("extractZip at limit: %v", err)
	}
}

func TestExtractArchiveUnknown(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "tool-linux")
	installWriteFile(t, src, []byte("raw"), 0644)
	ok, err := extractArchive(src, filepath.Join(root, "out"))
	if ok || err != nil {
		t.Errorf("extractArchive(raw) = %v, %v; want false, nil", ok, err)
	}
}

var installELF = []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}

func TestMakeBinariesExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no exec bits on windows")
	}
	dir := t.TempDir()
	installWriteFile(t, filepath.Join(dir, "LICENSE"), []byte("MIT"), 0644)
	installWriteFile(t, filepath.Join(dir, "README"), []byte("hi"), 0644)
	installWriteFile(t, filepath.Join(dir, "sub", "tool"), installELF, 0644)
	installWriteFile(t, filepath.Join(dir, "macho"), []byte{0xcf, 0xfa, 0xed, 0xfe, 0}, 0600)

	if err := makeBinariesExecutable(dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{
		"LICENSE":  0644,
		"README":   0644,
		"sub/tool": 0755,
		"macho":    0755,
	} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", name, info.Mode().Perm(), want)
		}
	}
}

func TestFindToolBinary(t *testing.T) {
	t.Run("prefers tool name over LICENSE", func(t *testing.T) {
		dir := t.TempDir()
		installWriteFile(t, filepath.Join(dir, "LICENSE"), []byte("MIT"), 0755)
		installWriteFile(t, filepath.Join(dir, "README"), []byte("hi"), 0644)
		installWriteFile(t, filepath.Join(dir, "selene"), installELF, 0755)
		got, err := findToolBinary(dir, "selene", "linux")
		if err != nil || got != filepath.Join(dir, "selene") {
			t.Errorf("findToolBinary = %q, %v", got, err)
		}
	})
	t.Run("case-insensitive name", func(t *testing.T) {
		dir := t.TempDir()
		installWriteFile(t, filepath.Join(dir, "a-helper"), installELF, 0755)
		installWriteFile(t, filepath.Join(dir, "bin", "stylua"), installELF, 0755)
		got, err := findToolBinary(dir, "StyLua", "darwin")
		if err != nil || got != filepath.Join(dir, "bin", "stylua") {
			t.Errorf("findToolBinary = %q, %v", got, err)
		}
	})
	t.Run("windows exe", func(t *testing.T) {
		dir := t.TempDir()
		installWriteFile(t, filepath.Join(dir, "LICENSE"), []byte("MIT"), 0644)
		installWriteFile(t, filepath.Join(dir, "selene.exe"), []byte("MZ"), 0644)
		got, err := findToolBinary(dir, "selene", "windows")
		if err != nil || got != filepath.Join(dir, "selene.exe") {
			t.Errorf("findToolBinary = %q, %v", got, err)
		}
	})
	t.Run("dotted raw asset name", func(t *testing.T) {
		dir := t.TempDir()
		installWriteFile(t, filepath.Join(dir, "README.md"), []byte("hi"), 0644)
		installWriteFile(t, filepath.Join(dir, "lune-0.8.0-linux-x86_64"), installELF, 0644)
		got, err := findToolBinary(dir, "lune", "linux")
		if err != nil || got != filepath.Join(dir, "lune-0.8.0-linux-x86_64") {
			t.Errorf("findToolBinary = %q, %v", got, err)
		}
	})
	t.Run("empty dir", func(t *testing.T) {
		if got, err := findToolBinary(t.TempDir(), "selene", "linux"); err == nil {
			t.Errorf("findToolBinary on empty dir = %q, want error", got)
		}
	})
	t.Run("missing dir", func(t *testing.T) {
		if _, err := findToolBinary(filepath.Join(t.TempDir(), "nope"), "selene", "linux"); err == nil {
			t.Error("findToolBinary on missing dir succeeded")
		}
	})
}

func TestToolInstallDir(t *testing.T) {
	home := t.TempDir()
	tools := filepath.Join(home, ".chainlink", "tools")

	if dir, ok := toolInstallDir(home, "selene", "0.32.0"); ok || dir != filepath.Join(tools, "selene", "0.32.0") {
		t.Errorf("fresh: %q, %v", dir, ok)
	}

	// A legacy v-prefixed install is found for an un-prefixed tag.
	os.MkdirAll(filepath.Join(tools, "selene", "v0.32.0"), 0755)
	if dir, ok := toolInstallDir(home, "selene", "0.32.0"); !ok || dir != filepath.Join(tools, "selene", "v0.32.0") {
		t.Errorf("v-dir: %q, %v", dir, ok)
	}

	// The exact tag wins over the v-dir.
	os.MkdirAll(filepath.Join(tools, "selene", "0.32.0"), 0755)
	if dir, ok := toolInstallDir(home, "selene", "0.32.0"); !ok || dir != filepath.Join(tools, "selene", "0.32.0") {
		t.Errorf("exact: %q, %v", dir, ok)
	}

	// A 'v' tag does not look for "vv".
	if _, ok := toolInstallDir(home, "selene", "v1.0.0"); ok {
		t.Error("v1.0.0 reported installed")
	}

	// A plain file is not an install.
	installWriteFile(t, filepath.Join(tools, "x", "1.0.0"), nil, 0644)
	if _, ok := toolInstallDir(home, "x", "1.0.0"); ok {
		t.Error("file reported as installed dir")
	}
}

// installSetupRelease registers a release with one host-matching tar.gz asset
// containing LICENSE and the tool binary.
func installSetupRelease(t *testing.T, g *installFakeGitHub, tag, bin string) string {
	t.Helper()
	asset := installHostAsset(bin, ".tar.gz")
	g.tags[tag] = []string{asset + ".sha256", asset}
	g.files[asset+".sha256"] = []byte("deadbeef")
	g.files[asset] = installMakeTar(t, []installEntry{
		{Name: "LICENSE", Body: "MIT", Mode: 0644},
		{Name: installBinName(bin), Body: string(installELF), Mode: 0644},
	}, "gz")
	return asset
}

func TestInstallToolFresh(t *testing.T) {
	home, exe := installTestHome(t)
	g := installNewFakeGitHub(t, "Kampfkarren/selene")
	installSetupRelease(t, g, "0.32.0", "selene")

	dir, err := InstallToolDir("selene", "Kampfkarren/selene@0.32.0")
	if err != nil {
		t.Fatalf("InstallToolDir: %v", err)
	}
	want := filepath.Join(home, ".chainlink", "tools", "selene", "0.32.0")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	bin, err := findToolBinary(dir, "selene", runtime.GOOS)
	if err != nil || filepath.Base(bin) != installBinName("selene") {
		t.Errorf("findToolBinary = %q, %v", bin, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(bin)
		if info.Mode().Perm()&0111 == 0 {
			t.Error("binary not executable")
		}
		info, _ = os.Stat(filepath.Join(dir, "LICENSE"))
		if info.Mode().Perm()&0111 != 0 {
			t.Error("LICENSE was made executable")
		}
	}
	if installExists(filepath.Join(dir, installHostAsset("selene", ".tar.gz"))) {
		t.Error("archive left in install dir")
	}
	if got, err := os.Readlink(filepath.Join(home, ".chainlink", "bin", "selene")); err != nil || got != exe {
		t.Errorf("symlink = %q, %v; want %q", got, err, exe)
	}
	if n := g.downloads.Load(); n != 1 {
		t.Errorf("downloads = %d, want 1 (checksum asset must not be fetched)", n)
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".chainlink", ".staging"))
	if len(entries) != 0 {
		t.Errorf("staging not cleaned: %v", entries)
	}
}

func TestInstallToolSkipsInstalled(t *testing.T) {
	home, _ := installTestHome(t)
	g := installNewFakeGitHub(t, "Kampfkarren/selene")
	for _, dirTag := range []string{"0.32.0", "v0.32.0"} {
		t.Run(dirTag, func(t *testing.T) {
			tools := filepath.Join(home, ".chainlink", "tools")
			os.RemoveAll(tools)
			want := filepath.Join(tools, "selene", dirTag)
			os.MkdirAll(want, 0755)
			before := g.requests.Load()

			dir, err := InstallToolDir("selene", "Kampfkarren/selene@0.32.0")
			if err != nil {
				t.Fatal(err)
			}
			if dir != want {
				t.Errorf("dir = %q, want %q", dir, want)
			}
			if n := g.requests.Load() - before; n != 0 {
				t.Errorf("made %d HTTP requests for an installed tool", n)
			}
		})
	}
}

// Regression: a release found only as 'v'+tag used to be installed under
// tools/<name>/v<tag>, where the tool proxy (which looks in tools/<name>/<tag>)
// could not find it right after the auto-install.
func TestInstallToolVPrefixFallback(t *testing.T) {
	home, _ := installTestHome(t)
	g := installNewFakeGitHub(t, "Kampfkarren/selene")
	installSetupRelease(t, g, "v0.32.0", "selene")

	dir, err := InstallToolDir("selene", "Kampfkarren/selene@0.32.0")
	if err != nil {
		t.Fatalf("InstallToolDir: %v", err)
	}
	if n := g.requests.Load(); n != 3 {
		t.Errorf("requests = %d, want 3 (404, v-tag, download)", n)
	}
	// The same path runToolBySpec computes for this spec.
	proxyDir := filepath.Join(home, ".chainlink", "tools", "selene", "0.32.0")
	if dir != proxyDir {
		t.Errorf("dir = %q, want %q", dir, proxyDir)
	}
	if _, err := findToolBinary(proxyDir, "selene", runtime.GOOS); err != nil {
		t.Errorf("binary not found where the proxy looks: %v", err)
	}

	before := g.requests.Load()
	if _, err := InstallToolDir("selene", "Kampfkarren/selene@0.32.0"); err != nil {
		t.Fatal(err)
	}
	if n := g.requests.Load() - before; n != 0 {
		t.Errorf("second install made %d requests", n)
	}
}

func TestInstallToolNotFound(t *testing.T) {
	home, _ := installTestHome(t)
	installNewFakeGitHub(t, "o/tool")
	if _, err := InstallToolDir("tool", "o/tool@9.9.9"); err == nil {
		t.Fatal("expected error for missing release")
	}
	if installExists(filepath.Join(home, ".chainlink", "tools", "tool", "9.9.9")) {
		t.Error("version dir created for missing release")
	}
}

// Regression: a failed download used to leave the version dir behind, and
// every later install skipped the broken tool as "already installed".
func TestInstallToolFailedDownloadRetries(t *testing.T) {
	home, _ := installTestHome(t)
	g := installNewFakeGitHub(t, "o/tool")
	installSetupRelease(t, g, "1.0.0", "tool")
	versionDir := filepath.Join(home, ".chainlink", "tools", "tool", "1.0.0")

	g.failDL.Store(true)
	if _, err := InstallToolDir("tool", "o/tool@1.0.0"); err == nil {
		t.Fatal("expected download error")
	}
	if installExists(versionDir) {
		t.Fatal("failed install left the version dir behind")
	}

	g.failDL.Store(false)
	if _, err := InstallToolDir("tool", "o/tool@1.0.0"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n := g.downloads.Load(); n != 2 {
		t.Errorf("downloads = %d, want 2", n)
	}
	if _, err := findToolBinary(versionDir, "tool", runtime.GOOS); err != nil {
		t.Errorf("retry did not install: %v", err)
	}
}

func TestInstallToolBadArchiveLeavesNothing(t *testing.T) {
	home, _ := installTestHome(t)
	g := installNewFakeGitHub(t, "o/tool")
	asset := installHostAsset("tool", ".tar.gz")
	g.tags["1.0.0"] = []string{asset}
	g.files[asset] = installMakeTar(t, []installEntry{
		{Name: "tool", Body: "x", Mode: 0755},
		{Name: "../../../../evil", Body: "pwned"},
	}, "gz")

	if _, err := InstallToolDir("tool", "o/tool@1.0.0"); err == nil {
		t.Fatal("expected traversal error")
	}
	if installExists(filepath.Join(home, ".chainlink", "tools", "tool", "1.0.0")) {
		t.Error("version dir left after failed extract")
	}
	if installExists(filepath.Join(home, ".chainlink", "evil")) || installExists(filepath.Join(home, "evil")) {
		t.Error("traversal entry written outside the install dir")
	}
}

// Regression: install keyed the tool dir by the chain.toml alias while the
// proxy looks it up by the repo name.
func TestInstallToolAliasUsesRepoDir(t *testing.T) {
	home, exe := installTestHome(t)
	g := installNewFakeGitHub(t, "Kampfkarren/selene")
	installSetupRelease(t, g, "0.32.0", "selene")

	dir, err := InstallToolDir("lint", "Kampfkarren/selene@0.32.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".chainlink", "tools", "selene", "0.32.0"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if installExists(filepath.Join(home, ".chainlink", "tools", "lint")) {
		t.Error("tool dir created under the alias")
	}
	if got, _ := os.Readlink(filepath.Join(home, ".chainlink", "bin", "lint")); got != exe {
		t.Errorf("bin/lint -> %q, want %q", got, exe)
	}

	// The proxy's lookup (repo name) now finds it without another download.
	before := g.requests.Load()
	if _, err := InstallToolDir("selene", "Kampfkarren/selene@0.32.0"); err != nil {
		t.Fatal(err)
	}
	if n := g.requests.Load() - before; n != 0 {
		t.Errorf("proxy-style install made %d requests", n)
	}
}

func TestInstallToolRawAsset(t *testing.T) {
	home, _ := installTestHome(t)
	g := installNewFakeGitHub(t, "lune-org/lune")
	asset := installHostAsset("lune-0.8.0", "")
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	g.tags["0.8.0"] = []string{asset}
	g.files[asset] = installELF

	dir, err := InstallToolDir("lune", "lune-org/lune@0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, installBinName("lune"))
	if got := installReadFile(t, bin); got != string(installELF) {
		t.Errorf("binary content = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(bin)
		if info.Mode().Perm()&0111 == 0 {
			t.Error("raw binary not executable")
		}
	}
	if found, err := findToolBinary(dir, "lune", runtime.GOOS); err != nil || found != bin {
		t.Errorf("findToolBinary = %q, %v", found, err)
	}
	if installExists(filepath.Join(home, ".chainlink", "tools", "lune", "0.8.0", asset)) {
		t.Error("raw asset kept under its release name")
	}
}

func TestInstallToolSendsGitHubToken(t *testing.T) {
	installTestHome(t)
	g := installNewFakeGitHub(t, "o/tool")
	installSetupRelease(t, g, "1.0.0", "tool")
	t.Setenv("GITHUB_TOKEN", "secret")

	if _, err := InstallToolDir("tool", "o/tool@1.0.0"); err != nil {
		t.Fatal(err)
	}
	if got := g.apiAuth.Load(); got != "Bearer secret" {
		t.Errorf("API Authorization = %q", got)
	}
	// The token goes to the API only, not to asset downloads.
	if got := g.dlAuth.Load(); got != "" {
		t.Errorf("download sent Authorization %q", got)
	}
}

func TestEnsureSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "chainlink")
	installWriteFile(t, target, nil, 0755)
	link := filepath.Join(dir, "selene")

	if err := ensureSymlink(target, link); err != nil {
		t.Fatal(err)
	}
	info1, _ := os.Lstat(link)
	if err := ensureSymlink(target, link); err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Lstat(link)
	if !os.SameFile(info1, info2) {
		t.Error("correct link was recreated")
	}

	other := filepath.Join(dir, "other")
	if err := ensureSymlink(other, link); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != other {
		t.Errorf("link -> %q, want %q", got, other)
	}

	if err := ensureSymlink(link, link); err == nil {
		t.Error("self link accepted")
	}
}

// Regression: when a tool was auto-installed through its own proxy symlink,
// os.Executable returned that symlink and it was relinked to itself.
func TestToolSymlinkTargetResolvesSelf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	home, exe := installTestHome(t)
	binDir := filepath.Join(home, ".chainlink", "bin")
	os.MkdirAll(binDir, 0755)
	link := filepath.Join(binDir, "selene")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	installExecutable = func() (string, error) { return link, nil }

	if err := linkTool(home, "selene"); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	wantExe, _ := filepath.EvalSymlinks(exe)
	if got != wantExe {
		t.Errorf("selene -> %q, want %q", got, wantExe)
	}
	if _, err := os.Stat(link); err != nil {
		t.Errorf("link does not resolve: %v", err)
	}
}

func TestToolSymlinkTargetPrefersStable(t *testing.T) {
	home, _ := installTestHome(t)
	stable := filepath.Join(home, ".chainlink", "bin", chainlinkBinaryName(runtime.GOOS))
	installWriteFile(t, stable, nil, 0755)
	got, err := toolSymlinkTarget(home)
	if err != nil || got != stable {
		t.Errorf("toolSymlinkTarget = %q, %v; want %q", got, err, stable)
	}
	if err := linkTool(home, "chainlink"); err == nil {
		t.Error("linkTool replaced the chainlink binary with a self link")
	}
	if info, err := os.Lstat(stable); err != nil || !info.Mode().IsRegular() {
		t.Error("chainlink binary was removed")
	}
}

// Regression: on Windows `chainlink global` installs chainlink.exe, so looking
// for bin/chainlink never found the stable binary.
func TestChainlinkBinaryName(t *testing.T) {
	if got := chainlinkBinaryName("windows"); got != "chainlink.exe" {
		t.Errorf("windows = %q", got)
	}
	for _, goos := range []string{"darwin", "linux"} {
		if got := chainlinkBinaryName(goos); got != "chainlink" {
			t.Errorf("%s = %q", goos, got)
		}
	}
}

// Regression: creating .bash_profile on macOS stops bash login shells from
// reading an existing .profile.
func TestShellConfigForDarwinBashKeepsExistingProfile(t *testing.T) {
	for _, tt := range []struct {
		existing []string
		want     string
	}{
		{nil, ".bash_profile"},
		{[]string{".profile"}, ".profile"},
		{[]string{".bash_login", ".profile"}, ".bash_login"},
		{[]string{".bash_profile", ".profile"}, ".bash_profile"},
	} {
		home := t.TempDir()
		for _, f := range tt.existing {
			installWriteFile(t, filepath.Join(home, f), []byte("# rc\n"), 0644)
		}
		got, err := shellConfigFor("darwin", "/bin/bash", home)
		if err != nil || got != filepath.Join(home, tt.want) {
			t.Errorf("with %v: shellConfigFor = %q, %v; want %s", tt.existing, got, err, tt.want)
		}
	}
}

// Regression: tools installed by older versions live under the chain.toml
// alias, not the repo name, and were downloaded again.
func TestInstalledToolDirLegacyAlias(t *testing.T) {
	home := t.TempDir()
	s := toolSpec{Repo: "JohnnyMorganz/StyLua", Name: "StyLua", Tag: "0.20.0"}
	tools := filepath.Join(home, ".chainlink", "tools")

	if dir, ok := installedToolDir(home, s, "fmt"); ok || dir != filepath.Join(tools, "StyLua", "0.20.0") {
		t.Fatalf("nothing installed: %q, %v", dir, ok)
	}
	legacy := filepath.Join(tools, "fmt", "0.20.0")
	os.MkdirAll(legacy, 0755)
	if dir, ok := installedToolDir(home, s, "fmt"); !ok || dir != legacy {
		t.Errorf("legacy alias dir: %q, %v", dir, ok)
	}
	if _, ok := installedToolDir(home, s, ""); ok {
		t.Error("no alias should not match the legacy dir")
	}
	current := filepath.Join(tools, "StyLua", "0.20.0")
	os.MkdirAll(current, 0755)
	if dir, ok := installedToolDir(home, s, "fmt"); !ok || dir != current {
		t.Errorf("current dir should win: %q, %v", dir, ok)
	}
}

func TestShellConfigFor(t *testing.T) {
	home := "/home/u"
	tests := []struct {
		goos, shell, want string
		wantErr           bool
	}{
		{"darwin", "/bin/zsh", ".zshrc", false},
		{"linux", "/usr/bin/zsh", ".zshrc", false},
		{"darwin", "/bin/bash", ".bash_profile", false},
		{"darwin", "/opt/homebrew/bin/bash", ".bash_profile", false},
		{"linux", "/bin/bash", ".bashrc", false},
		{"linux", "", ".bashrc", false},
		{"linux", "/usr/bin/fish", ".bashrc", false},
		{"windows", "", "", false},
		{"plan9", "/bin/rc", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.goos+tt.shell, func(t *testing.T) {
			got, err := shellConfigFor(tt.goos, tt.shell, home)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			want := ""
			if tt.want != "" {
				want = filepath.Join(home, tt.want)
			}
			if got != want {
				t.Errorf("shellConfigFor = %q, want %q", got, want)
			}
		})
	}
}

func TestPathListContains(t *testing.T) {
	sep := string(os.PathListSeparator)
	dir := filepath.FromSlash("/u/.chainlink/bin")
	tests := []struct {
		path string
		want bool
	}{
		{strings.Join([]string{"/usr/bin", dir}, sep), true},
		{strings.Join([]string{dir + "/", "/usr/bin"}, sep), true},
		{strings.Join([]string{dir + "-old", "/usr/bin"}, sep), false},
		{strings.Join([]string{"/x" + dir}, sep), false},
		{"", false},
	}
	for _, tt := range tests {
		if got := pathListContains(tt.path, dir); got != tt.want {
			t.Errorf("pathListContains(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestMentionsDir(t *testing.T) {
	dir := "/u/.chainlink/bin"
	tests := []struct {
		text string
		want bool
	}{
		{"export PATH=\"/u/.chainlink/bin:$PATH\"\n", true},
		{"export PATH=/u/.chainlink/bin", true},
		{"export PATH=\"/u/.chainlink/bin-old:$PATH\"\n", false},
		{"PATH=/u/.chainlink/binary:/u/.chainlink/bin:$PATH", true},
		{"export PATH=\"/u/.chainlink/bin/:$PATH\"", true},
		{"  export PATH=$PATH:/u/.chainlink/bin # chainlink", true},
		{"# export PATH=\"/u/.chainlink/bin:$PATH\"\n", false},
		{"   #export PATH=/u/.chainlink/bin\n", false},
		{"alias selene=/u/.chainlink/bin/selene\n", false},
		{"export PATH=\"/u/.chainlink/bin/selene:$PATH\"", false},
		{"cd /u/.chainlink/bin\n", false},
		{"# old\nexport PATH=\"/u/.chainlink/bin:$PATH\"\n", true},
		{"", false},
	}
	for _, tt := range tests {
		if got := mentionsDir(tt.text, dir); got != tt.want {
			t.Errorf("mentionsDir(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestInstallChainlink(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("rc files are only written on darwin/linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	binDir := filepath.Join(home, ".chainlink", "bin")
	rc := filepath.Join(home, ".zshrc")

	// A look-alike PATH entry must not count as installed.
	t.Setenv("PATH", binDir+"-old"+string(os.PathListSeparator)+"/usr/bin")

	// Regression: running twice before the shell restarts appended twice.
	for i := 0; i < 2; i++ {
		if err := InstallChainlink(); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(binDir); err != nil || !info.IsDir() {
		t.Errorf("bin dir not created: %v", err)
	}
	if n := strings.Count(installReadFile(t, rc), binDir+":"); n != 1 {
		t.Errorf("rc has %d PATH lines, want 1", n)
	}

	// Already on PATH: the rc file is not touched.
	rc2 := filepath.Join(home, ".bashrc")
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin"+string(os.PathListSeparator)+binDir)
	if err := InstallChainlink(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		rc2 = filepath.Join(home, ".bash_profile")
	}
	if installExists(rc2) {
		t.Error("rc file written although bin dir is on PATH")
	}
}

func TestPluginsDirFor(t *testing.T) {
	home := "/h"
	for goos, want := range map[string]string{
		"windows": filepath.Join(home, "AppData", "Local", "Roblox", "Plugins"),
		"darwin":  filepath.Join(home, "Documents", "Roblox", "Plugins"),
		"linux":   filepath.Join(home, ".local", "share", "roblox", "Plugins"),
	} {
		if got, err := pluginsDirFor(goos, home); err != nil || got != want {
			t.Errorf("pluginsDirFor(%s) = %q, %v", goos, got, err)
		}
	}
	if _, err := pluginsDirFor("plan9", home); err == nil {
		t.Error("expected error for unsupported os")
	}
}

// installProtectedSource parses an rbxmx and returns the Source property.
func installProtectedSource(t *testing.T, data []byte) string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	inSource := false
	var sb strings.Builder
	found := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("rbxmx is not valid XML: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if el.Name.Local == "ProtectedString" {
				for _, a := range el.Attr {
					if a.Name.Local == "name" && a.Value == "Source" {
						inSource, found = true, true
					}
				}
			}
		case xml.EndElement:
			inSource = false
		case xml.CharData:
			if inSource {
				sb.Write(el)
			}
		}
	}
	if !found {
		t.Fatal("no Source ProtectedString in rbxmx")
	}
	return sb.String()
}

func TestInstallPlugin(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("plugin dir layout tested on darwin/linux")
	}
	tricky := "local s = \"<b>&amp;</b>\" -- 100% 'quoted' ]]> %s %d {{Source}}\n\tprint(a < b and c > d & e)\n"
	for name, src := range map[string]string{"embedded": pluginSource, "tricky": tricky} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			old := pluginSource
			pluginSource = src
			t.Cleanup(func() { pluginSource = old })

			dir, err := pluginsDirFor(runtime.GOOS, home)
			if err != nil {
				t.Fatal(err)
			}
			stale := filepath.Join(dir, "ChainLink.rbxm")
			installWriteFile(t, stale, []byte("old"), 0644)

			if err := installPlugin(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "ChainLink.rbxmx"))
			if err != nil {
				t.Fatal(err)
			}
			if got := installProtectedSource(t, data); got != src {
				t.Errorf("Source did not round-trip:\n got %q\nwant %q", got, src)
			}
			if installExists(stale) {
				t.Error("stale ChainLink.rbxm not removed")
			}
		})
	}
}

func BenchmarkSelectAsset(b *testing.B) {
	var assets []GitHubAsset
	for _, goos := range []string{"linux", "windows", "macos"} {
		for _, arch := range []string{"x86_64", "aarch64"} {
			n := fmt.Sprintf("tool-1.0.0-%s-%s.zip", goos, arch)
			assets = append(assets, GitHubAsset{Name: n + ".sha256"}, GitHubAsset{Name: n})
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := selectAsset(assets, "darwin", "arm64"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSafeJoin(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := safeJoin("/tmp/dest", "pkg/src/Shared/Module.luau"); err != nil {
			b.Fatal(err)
		}
	}
}

func installBenchEntries() []installEntry {
	var entries []installEntry
	for i := 0; i < 200; i++ {
		entries = append(entries, installEntry{
			Name: fmt.Sprintf("src/dir%d/file%d.luau", i%10, i),
			Body: strings.Repeat("local x = 1\n", 200),
			Mode: 0644,
		})
	}
	return entries
}

func BenchmarkExtractZip(b *testing.B) {
	root := b.TempDir()
	src := filepath.Join(root, "a.zip")
	installWriteFile(b, src, installMakeZip(b, installBenchEntries()), 0644)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if err := extractZip(src, filepath.Join(root, fmt.Sprint("out", i))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExtractTarGz(b *testing.B) {
	root := b.TempDir()
	src := filepath.Join(root, "a.tar.gz")
	installWriteFile(b, src, installMakeTar(b, installBenchEntries(), "gz"), 0644)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if err := extractTarGz(src, filepath.Join(root, fmt.Sprint("out", i))); err != nil {
			b.Fatal(err)
		}
	}
}
