package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ---------- helpers ----------

// serveFakeHome points the home dir (and so packagesBase) at a temp dir.
func serveFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func serveWriteFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// serveNewServer builds a Server over a temp project root with a real watcher.
func serveNewServer(t *testing.T, cfg ProjectConfig) *Server {
	t.Helper()
	serveFakeHome(t)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	go func() { // keep the watcher from blocking on undelivered events
		for {
			select {
			case _, ok := <-w.Events:
				if !ok {
					return
				}
			case <-w.Errors:
			}
		}
	}()
	return &Server{
		cfg:     cfg,
		active:  presentPackages(cfg),
		root:    t.TempDir(),
		queue:   NewQueue(),
		watcher: w,
		known:   map[string]bool{},
		pending: map[string]*time.Timer{},
		owners:  map[string]string{},
	}
}

// serveDrain returns every event pushed since the last drain.
func serveDrain(t *testing.T, q *Queue, since *uint64) []Event {
	t.Helper()
	evs, latest, ok := q.Since(*since)
	if !ok {
		t.Fatalf("Since(%d) not ok", *since)
	}
	*since = latest
	return evs
}

func serveFind(evs []Event, op, path string) (Event, bool) {
	for _, ev := range evs {
		if ev.Op == op && ev.Path == path {
			return ev, true
		}
	}
	return Event{}, false
}

func serveDescribe(evs []Event) string {
	var parts []string
	for _, ev := range evs {
		parts = append(parts, fmt.Sprintf("%s %s %v", ev.Op, ev.Path, ev.InstancePath))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// ---------- classification ----------

func TestClassify(t *testing.T) {
	on := defaultConfig()
	off := defaultConfig()
	off.DisableBootstrap = true
	const rs = "ReplicatedStorage.shared"

	tests := []struct {
		name  string
		cfg   ProjectConfig
		rel   string
		isDir bool
		want  Classified
	}{
		{"module", on, "shared/foo.luau", false, Classified{rs, []string{"foo"}, "ModuleScript", false, true}},
		{"lua module", on, "shared/foo.lua", false, Classified{rs, []string{"foo"}, "ModuleScript", false, true}},
		{"server script", on, "shared/a/b.server.luau", false, Classified{rs, []string{"a", "b"}, "Script", false, true}},
		{"client script lua", on, "shared/c.client.lua", false, Classified{rs, []string{"c"}, "LocalScript", false, true}},
		{"json", on, "shared/data.json", false, Classified{rs, []string{"data"}, "ModuleScript", false, true}},
		{"txt", on, "shared/note.txt", false, Classified{rs, []string{"note"}, "StringValue", false, true}},
		{"rbxm", on, "shared/m.rbxm", false, Classified{rs, []string{"m"}, "Model", false, true}},
		{"rbxmx", on, "shared/m.rbxmx", false, Classified{rs, []string{"m"}, "Model", false, true}},
		{"upper ext", on, "shared/Up.LUAU", false, Classified{rs, []string{"Up"}, "ModuleScript", false, true}},
		{"unknown ext", on, "shared/x.png", false, Classified{}},
		{"init in subdir", on, "shared/sub/init.luau", false, Classified{rs, []string{"sub"}, "ModuleScript", true, true}},
		{"init server in subdir", on, "shared/sub/init.server.luau", false, Classified{rs, []string{"sub"}, "Script", true, true}},
		{"root-level init", on, "shared/init.luau", false, Classified{}},
		{"root-level init bootstrap off", off, "server/init.server.luau", false, Classified{}},
		{"bootstrapped file", on, "server/foo.luau", false, Classified{"ServerScriptService", []string{"init", "foo"}, "ModuleScript", false, true}},
		{"bootstrapped client", on, "client/ui/x.client.luau", false, Classified{"StarterPlayer.StarterPlayerScripts", []string{"init", "ui", "x"}, "LocalScript", false, true}},
		{"bootstrap off file", off, "server/foo.luau", false, Classified{"ServerScriptService", []string{"foo"}, "ModuleScript", false, true}},
		{"bootstrapped root dir", on, "server", true, Classified{}},
		{"shadowed root init", on, "server/init.server.luau", false, Classified{}},
		{"bootstrapped nested init", on, "server/sub/init.luau", false, Classified{"ServerScriptService", []string{"init", "sub"}, "ModuleScript", true, true}},
		{"directory", on, "shared/dir", true, Classified{rs, []string{"dir"}, "Folder", false, true}},
		{"bootstrapped directory", on, "server/dir", true, Classified{"ServerScriptService", []string{"init", "dir"}, "Folder", false, true}},
		{"non-bootstrapped root dir", on, "shared", true, Classified{rs, nil, "Folder", false, true}},
		{"root file itself", on, "shared", false, Classified{}},
		{"outside roots", on, "other/foo.luau", false, Classified{}},
		{"prefix collision", on, "shared2/x.luau", false, Classified{}},
		{"top-level file", on, "foo.luau", false, Classified{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.cfg, tt.rel, tt.isDir)
			if got.Target != tt.want.Target || !slices.Equal(got.InstancePath, tt.want.InstancePath) ||
				got.ClassName != tt.want.ClassName || got.IsInit != tt.want.IsInit || got.Syncable != tt.want.Syncable {
				t.Errorf("classify(%q, %v) = %+v, want %+v", tt.rel, tt.isDir, got, tt.want)
			}
		})
	}
}

func TestMapPathPackages(t *testing.T) {
	cfg := defaultConfig()
	cfg.Packages = []GlobalPackageMapping{{PackageName: "a_b/1.0/src", Target: "ReplicatedStorage.packages.b"}}

	target, base, rest, boot, ok := mapPath(cfg, "~pkg/a_b/1.0/src/x.luau")
	if !ok || boot || target != "ReplicatedStorage.packages" || !slices.Equal(base, []string{"b"}) || rest != "x.luau" {
		t.Errorf("package file: got (%q, %v, %q, %v, %v)", target, base, rest, boot, ok)
	}
	if root, ok := packageRootOf(cfg, "~pkg/a_b/1.0/src/x.luau"); !ok || root != "~pkg/a_b/1.0/src" {
		t.Errorf("packageRootOf = %q, %v", root, ok)
	}
	if _, _, rest, _, ok := mapPath(cfg, "~pkg/a_b/1.0/src"); !ok || rest != "" {
		t.Errorf("package root: rest=%q ok=%v", rest, ok)
	}
	if _, _, _, _, ok := mapPath(cfg, "~pkg/a_b/1.0/src2/x.luau"); ok {
		t.Error("src2 must not match the src package")
	}
	if _, ok := packageRootOf(cfg, "~pkg/a_b/1.0/src2/x.luau"); ok {
		t.Error("packageRootOf matched src2")
	}
	if _, ok := packageRootOf(cfg, "shared/x.luau"); ok {
		t.Error("packageRootOf matched a non-package path")
	}
	target, base, rest, _, ok = mapPath(cfg, "shared/x.luau")
	if !ok || target != "ReplicatedStorage.shared" || base != nil || rest != "x.luau" {
		t.Errorf("roots fallthrough: got (%q, %v, %q, %v)", target, base, rest, ok)
	}
	c := classify(cfg, "~pkg/a_b/1.0/src/init.luau", false)
	if !c.IsInit || !slices.Equal(c.InstancePath, []string{"b"}) {
		t.Errorf("package root init: %+v", c)
	}
}

func TestSplitTarget(t *testing.T) {
	tests := []struct{ in, parent, name string }{
		{"A.B.c", "A.B", "c"},
		{"c", "", "c"},
		{"ReplicatedStorage.packages.razorinput", "ReplicatedStorage.packages", "razorinput"},
	}
	for _, tt := range tests {
		if p, n := splitTarget(tt.in); p != tt.parent || n != tt.name {
			t.Errorf("splitTarget(%q) = (%q, %q), want (%q, %q)", tt.in, p, n, tt.parent, tt.name)
		}
	}
}

func TestIsUnder(t *testing.T) {
	base := filepath.Join(string(filepath.Separator), "a", "b")
	tests := []struct {
		p    string
		want bool
	}{
		{base, true},
		{filepath.Join(base, "c"), true},
		{filepath.Join(base, "c", "d"), true},
		{filepath.Join(string(filepath.Separator), "a", "bc"), false},
		{base + string(filepath.Separator) + ".." + string(filepath.Separator) + "c", false},
		{filepath.Join(string(filepath.Separator), "a"), false},
		{filepath.Join(base, "..foo"), true}, // a name starting with ".." is still inside
	}
	for _, tt := range tests {
		if got := isUnder(base, tt.p); got != tt.want {
			t.Errorf("isUnder(%q, %q) = %v, want %v", base, tt.p, got, tt.want)
		}
	}
}

func TestAddLuauHeader(t *testing.T) {
	once := addLuauHeader("return 1")
	if !strings.HasPrefix(once, "--!strict\n") || !strings.HasSuffix(once, "return 1") {
		t.Fatalf("header not added: %q", once)
	}
	if twice := addLuauHeader(once); twice != once {
		t.Errorf("not idempotent: %q", twice)
	}
	if got := addLuauHeader("--!strict\nreturn 2"); got != "--!strict\nreturn 2" {
		t.Errorf("existing --!strict changed: %q", got)
	}
}

func TestBootstrapHelpers(t *testing.T) {
	on := defaultConfig()
	off := defaultConfig()
	off.DisableBootstrap = true

	for _, dir := range []string{"server", "client"} {
		if !isBootstrapped(on, dir) {
			t.Errorf("isBootstrapped(on, %q) = false", dir)
		}
		if isBootstrapped(off, dir) {
			t.Errorf("isBootstrapped(off, %q) = true", dir)
		}
	}
	if isBootstrapped(on, "shared") {
		t.Error("shared must not be bootstrapped")
	}

	tests := []struct {
		cfg  ProjectConfig
		rel  string
		want bool
	}{
		{on, "server/init.server.luau", true},
		{on, "client/init.client.luau", true},
		{on, "server/init.luau", true},
		{on, "server/Init.lua", true},
		{on, "server/foo.luau", false},
		{on, "server/sub/init.luau", false},
		{on, "shared/init.luau", false},
		{off, "server/init.server.luau", false},
	}
	for _, tt := range tests {
		if got := isShadowedRootInit(tt.cfg, tt.rel); got != tt.want {
			t.Errorf("isShadowedRootInit(bootstrap off=%v, %q) = %v, want %v", tt.cfg.DisableBootstrap, tt.rel, got, tt.want)
		}
	}
}

func TestInstanceKeyUnique(t *testing.T) {
	if instanceKey("T", []string{"a.b"}) == instanceKey("T", []string{"a", "b"}) {
		t.Error("[a.b] and [a b] collide")
	}
	if instanceKey("A.B", []string{"c"}) == instanceKey("A", []string{"B", "c"}) {
		t.Error("target/path boundary collides")
	}
	if instanceKey("T", []string{"a"}) != instanceKey("T", []string{"a"}) {
		t.Error("key not stable")
	}
}

// ---------- jsonToLuau ----------

func TestJSONToLuau(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "nil"},
		{"true", true, "true"},
		{"false", false, "false"},
		{"int", 3.0, "3"},
		{"negative", -42.0, "-42"},
		{"float", 1.5, "1.5"},
		{"big", 1e21, "1e+21"},
		{"empty array", []any{}, "{}"},
		{"array", []any{1.0, "a", nil, true}, `{1, "a", nil, true}`},
		{"object sorted", map[string]any{"b": 1.0, "a": []any{}, "c": map[string]any{"z": "y"}},
			`{["a"] = {}, ["b"] = 1, ["c"] = {["z"] = "y"}}`},
		{"escapes", "q\"b\\n\nr\rt\t", `"q\"b\\n\nr\rt\t"`},
		{"control", "\x01\x1f\x7f", `"\x01\x1f\x7f"`},
		{"nbsp", "a b", `"a\u{a0}b"`},
		{"zwsp", "​", `"\u{200b}"`},
		{"non-bmp unprintable", "\U000E0001", `"\u{e0001}"`},
		{"printable unicode", "héllo ✓ 😀", `"héllo ✓ 😀"`},
		{"invalid utf8", "\xff", `"\xff"`},
		{"escaped key", map[string]any{"a\"b ": 1.0}, `{["a\"b\u{a0}"] = 1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jsonToLuau(tt.in); got != tt.want {
				t.Errorf("jsonToLuau = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestJSONToLuauDeterministic(t *testing.T) {
	m := map[string]any{}
	for i := 0; i < 64; i++ {
		m[fmt.Sprintf("k%d", i)] = map[string]any{"x": float64(i), "y": "v"}
	}
	first := jsonToLuau(m)
	for i := 0; i < 20; i++ {
		if got := jsonToLuau(m); got != first {
			t.Fatal("jsonToLuau output differs between calls")
		}
	}
}

// ---------- Queue ----------

func TestQueuePushSince(t *testing.T) {
	q := NewQueue()
	q.Push() // no-op
	q.Push(Event{Path: "a"}, Event{Path: "b"})
	q.Push(Event{Path: "c"})

	evs, latest, ok := q.Since(0)
	if !ok || latest != 3 || len(evs) != 3 {
		t.Fatalf("Since(0) = %d events, latest %d, ok %v", len(evs), latest, ok)
	}
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d has seq %d", i, ev.Seq)
		}
	}
	if evs, _, ok := q.Since(1); !ok || len(evs) != 2 || evs[0].Path != "b" {
		t.Errorf("Since(1) = %v, %v", evs, ok)
	}
	if evs, latest, ok := q.Since(3); !ok || len(evs) != 0 || latest != 3 {
		t.Errorf("Since(3) = %v, %d, %v", evs, latest, ok)
	}
	if _, _, ok := q.Since(4); ok {
		t.Error("Since(future) should not be ok")
	}

	// The returned slice must not alias the queue's storage.
	evs, _, _ = q.Since(0)
	evs[0].Path = "mutated"
	if again, _, _ := q.Since(0); again[0].Path != "a" {
		t.Error("Since result aliases queue storage")
	}

	empty := NewQueue()
	if evs, latest, ok := empty.Since(0); !ok || len(evs) != 0 || latest != 0 {
		t.Errorf("empty Since(0) = %v, %d, %v", evs, latest, ok)
	}
}

func TestQueueTrim(t *testing.T) {
	q := NewQueue()
	const extra = 5
	for i := 0; i < maxQueueLen+extra; i++ {
		q.Push(Event{})
	}
	if _, _, ok := q.Since(0); ok {
		t.Error("Since(0) after trimming should require a full snapshot")
	}
	if _, _, ok := q.Since(extra - 1); ok {
		t.Error("Since(before oldest-1) should not be ok")
	}
	evs, latest, ok := q.Since(extra) // last trimmed seq
	if !ok || len(evs) != maxQueueLen || latest != maxQueueLen+extra {
		t.Fatalf("Since(lastTrimmed) = %d events, latest %d, ok %v", len(evs), latest, ok)
	}
	if evs[0].Seq != extra+1 || evs[len(evs)-1].Seq != maxQueueLen+extra {
		t.Errorf("seq range %d..%d", evs[0].Seq, evs[len(evs)-1].Seq)
	}
}

func TestQueueNotify(t *testing.T) {
	q := NewQueue()
	ch := q.Notify()
	select {
	case <-ch:
		t.Fatal("notify closed before any push")
	default:
	}
	q.Push(Event{})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("notify not closed after push")
	}
	if q.Notify() == ch {
		t.Error("Notify should hand out a fresh channel after a push")
	}
}

func TestQueueConcurrentPush(t *testing.T) {
	q := NewQueue()
	const workers, per = 32, 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				q.Push(Event{})
				q.Since(0)
			}
		}()
	}
	wg.Wait()
	evs, latest, ok := q.Since(0)
	if !ok || latest != workers*per || len(evs) != workers*per {
		t.Fatalf("got %d events, latest %d, ok %v", len(evs), latest, ok)
	}
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("seq %d at index %d", ev.Seq, i)
		}
	}
}

// ---------- loadConfig ----------

func TestLoadConfigDefaults(t *testing.T) {
	serveFakeHome(t)
	t.Chdir(t.TempDir())
	got := loadConfig()
	want := defaultConfig()
	if got.Port != want.Port || got.Token != "" || !slices.Equal(got.Roots, want.Roots) ||
		len(got.Packages) != 0 || got.DisableBootstrap {
		t.Errorf("loadConfig() = %+v, want defaults", got)
	}
}

func TestLoadConfigJSON(t *testing.T) {
	serveFakeHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	serveWriteFile(t, dir, configFileName,
		`{"port": 9000, "token": "secret", "roots": [{"dir": "src", "target": "ReplicatedStorage"}],
		  "packages": [{"package": "x/1.0", "target": "ReplicatedStorage.packages.x"}]}`)
	got := loadConfig()
	if got.Port != 9000 || got.Token != "secret" {
		t.Errorf("port/token = %d/%q", got.Port, got.Token)
	}
	if !slices.Equal(got.Roots, []RootMapping{{Dir: "src", Target: "ReplicatedStorage"}}) {
		t.Errorf("roots = %+v", got.Roots)
	}
	if len(got.Packages) != 1 || got.Packages[0].PackageName != "x/1.0" {
		t.Errorf("packages = %+v", got.Packages)
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	serveFakeHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	serveWriteFile(t, dir, configFileName, `{"port": 9000,`)
	got := loadConfig()
	if got.Port != defaultConfig().Port || !slices.Equal(got.Roots, defaultConfig().Roots) {
		t.Errorf("invalid JSON should keep defaults, got %+v", got)
	}
}

func TestLoadConfigChainToml(t *testing.T) {
	home := serveFakeHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	pkgs := filepath.Join(home, ".chainlink", "packages")
	for _, d := range []string{"scope_foo/1.2.3/src", "bar/2.0.0", "scope/qux/3.0.0"} {
		if err := os.MkdirAll(filepath.Join(pkgs, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	serveWriteFile(t, dir, chainTomlName, `DisableBootstrap = true

[Packages]
foo = "scope/foo@1.2.3"
bar = "scope/bar@2.0.0"
qux = "scope/qux@3.0.0"
dup = "scope/foo@1.2.3"
missing = "scope/missing@1.0.0"
`)
	got := loadConfig()
	if !got.DisableBootstrap {
		t.Error("DisableBootstrap not read from chain.toml")
	}
	want := []GlobalPackageMapping{
		{PackageName: "bar/2.0.0", Target: "ReplicatedStorage.packages.bar"},
		{PackageName: "scope/qux/3.0.0", Target: "ReplicatedStorage.packages.qux"},
		{PackageName: "scope_foo/1.2.3/src", Target: "ReplicatedStorage.packages.foo"},
		{PackageName: "scope_missing/1.0.0", Target: "ReplicatedStorage.packages.missing"},
	}
	if !slices.Equal(got.Packages, want) {
		t.Errorf("packages =\n  %+v\nwant\n  %+v", got.Packages, want)
	}
}

func TestLoadConfigBadToml(t *testing.T) {
	serveFakeHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	serveWriteFile(t, dir, chainTomlName, "this is = = not toml")
	if got := loadConfig(); got.DisableBootstrap || len(got.Packages) != 0 {
		t.Errorf("bad toml should be ignored, got %+v", got)
	}
}

// ---------- Server: fileEvent / claim ----------

func TestFileEvent(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "shared/a.luau", "return 1")
	serveWriteFile(t, s.root, "shared/d.json", `{"b": 1, "a": "x "}`)
	serveWriteFile(t, s.root, "shared/bad.json", `not json`)
	serveWriteFile(t, s.root, "shared/t.txt", "hello")
	model := []byte{0x3c, 0x72, 0x00, 0xff, 0xfe}
	serveWriteFile(t, s.root, "shared/m.rbxm", string(model))
	serveWriteFile(t, s.root, "shared/x.rbxmx", "<roblox/>")

	ev, ok := s.fileEvent("shared/a.luau", "CREATE")
	if !ok || ev.ClassName != "ModuleScript" || !strings.HasPrefix(ev.Source, "--!strict") ||
		!strings.Contains(ev.Source, "return 1") || ev.Op != "CREATE" || ev.Target != "ReplicatedStorage.shared" {
		t.Errorf("luau event = %+v", ev)
	}
	if ev, _ := s.fileEvent("shared/d.json", "UPDATE"); ev.Source != `return {["a"] = "x\u{a0}", ["b"] = 1}` {
		t.Errorf("json source = %q", ev.Source)
	}
	if ev, _ := s.fileEvent("shared/bad.json", "UPDATE"); ev.Source != "return not json" {
		t.Errorf("bad json source = %q", ev.Source)
	}
	if ev, _ := s.fileEvent("shared/t.txt", "CREATE"); ev.ClassName != "StringValue" || ev.Source != "hello" {
		t.Errorf("txt event = %+v", ev)
	}
	ev, _ = s.fileEvent("shared/m.rbxm", "CREATE")
	if ev.ModelData != base64.StdEncoding.EncodeToString(model) || ev.ModelFormat != "rbxm" || ev.Source != "" {
		t.Errorf("rbxm event = %+v", ev)
	}
	if ev, _ := s.fileEvent("shared/x.rbxmx", "CREATE"); ev.ModelFormat != "rbxmx" || ev.ClassName != "Model" {
		t.Errorf("rbxmx event = %+v", ev)
	}
	if _, ok := s.fileEvent("shared/missing.luau", "CREATE"); ok {
		t.Error("unreadable file should not produce an event")
	}
	if _, ok := s.fileEvent("shared/x.png", "CREATE"); ok {
		t.Error("unknown extension should not produce an event")
	}
}

func TestFileEventDeleteKeepChildren(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "shared/merged/child.luau", "")

	if ev, ok := s.fileEvent("shared/merged.luau", "DELETE"); !ok || !ev.KeepChildren {
		t.Errorf("file with sibling dir: %+v", ev)
	}
	if ev, ok := s.fileEvent("shared/lone.luau", "DELETE"); !ok || ev.KeepChildren {
		t.Errorf("lone file: %+v", ev)
	}
	if ev, ok := s.fileEvent("shared/merged/init.luau", "DELETE"); !ok || !ev.KeepChildren {
		t.Errorf("init file: %+v", ev)
	}
}

func TestClaimRelease(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "shared/foo.luau", "")
	serveWriteFile(t, s.root, "shared/foo.lua", "")
	owner, _ := s.fileEvent("shared/foo.luau", "CREATE")
	other, _ := s.fileEvent("shared/foo.lua", "CREATE")

	if !s.claim(owner) {
		t.Fatal("first claim rejected")
	}
	if !s.claim(owner) {
		t.Error("re-claim by the owner rejected")
	}
	if s.claim(other) {
		t.Error("second file claimed an owned instance")
	}
	if s.release(other) {
		t.Error("release by a non-owner succeeded")
	}
	if err := os.Remove(filepath.Join(s.root, "shared", "foo.luau")); err != nil {
		t.Fatal(err)
	}
	if !s.claim(other) {
		t.Error("claim rejected after the owner disappeared")
	}
	if !s.release(other) {
		t.Error("release by the new owner failed")
	}
	if s.release(other) {
		t.Error("double release succeeded")
	}
}

// ---------- Server: resolve ----------

func TestResolveFileLifecycle(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	var since uint64
	serveWriteFile(t, s.root, "shared/foo.luau", "return 1")

	s.resolve("shared/foo.luau")
	evs := serveDrain(t, s.queue, &since)
	if len(evs) != 1 || evs[0].Op != "CREATE" || !slices.Equal(evs[0].InstancePath, []string{"foo"}) {
		t.Fatalf("create: %s", serveDescribe(evs))
	}

	serveWriteFile(t, s.root, "shared/foo.luau", "return 2")
	s.resolve("shared/foo.luau")
	evs = serveDrain(t, s.queue, &since)
	if len(evs) != 1 || evs[0].Op != "UPDATE" || !strings.Contains(evs[0].Source, "return 2") {
		t.Fatalf("update: %s", serveDescribe(evs))
	}

	os.Remove(filepath.Join(s.root, "shared", "foo.luau"))
	s.resolve("shared/foo.luau")
	evs = serveDrain(t, s.queue, &since)
	if len(evs) != 1 || evs[0].Op != "DELETE" || evs[0].KeepChildren {
		t.Fatalf("delete: %s", serveDescribe(evs))
	}

	// A second resolve of a path that is already gone is a no-op.
	s.resolve("shared/foo.luau")
	if evs := serveDrain(t, s.queue, &since); len(evs) != 0 {
		t.Errorf("repeat delete: %s", serveDescribe(evs))
	}
}

func TestResolveNewDirectory(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	var since uint64
	serveWriteFile(t, s.root, "shared/dir/a.luau", "")
	serveWriteFile(t, s.root, "shared/dir/sub/b.server.luau", "")
	serveWriteFile(t, s.root, "shared/dir/ignored.png", "")

	s.resolve("shared/dir")
	evs := serveDrain(t, s.queue, &since)
	a, okA := serveFind(evs, "CREATE", "shared/dir/a.luau")
	b, okB := serveFind(evs, "CREATE", "shared/dir/sub/b.server.luau")
	if !okA || !okB || len(evs) != 2 {
		t.Fatalf("events: %s", serveDescribe(evs))
	}
	if !slices.Equal(a.InstancePath, []string{"dir", "a"}) || b.ClassName != "Script" {
		t.Errorf("a=%+v b=%+v", a, b)
	}
	s.mu.Lock()
	isDir, known := s.known["shared/dir/sub"]
	s.mu.Unlock()
	if !known || !isDir {
		t.Error("nested directory not recorded as known")
	}
}

// Regression: deleting foo/ next to foo.luau used to DELETE foo.luau's instance.
func TestResolveMergedDirDelete(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "shared/foo.luau", "")
	serveWriteFile(t, s.root, "shared/foo/bar.luau", "")
	serveWriteFile(t, s.root, "shared/foo/init.luau", "") // shadowed by foo.luau
	serveWriteFile(t, s.root, "shared/foo/sub/deep.luau", "")
	s.primeKnown()
	// Claim in this order so foo.luau (outside the folder) owns the instance.
	for _, rel := range []string{"shared/foo.luau", "shared/foo/init.luau", "shared/foo/bar.luau", "shared/foo/sub/deep.luau"} {
		s.resolve(rel)
	}
	_, since, _ := s.queue.Since(0)

	if err := os.RemoveAll(filepath.Join(s.root, "shared", "foo")); err != nil {
		t.Fatal(err)
	}
	s.resolve("shared/foo")
	evs := serveDrain(t, s.queue, &since)
	for _, ev := range evs {
		if slices.Equal(ev.InstancePath, []string{"foo"}) {
			t.Fatalf("merged file's instance touched: %s", serveDescribe(evs))
		}
	}
	if _, ok := serveFind(evs, "DELETE", "shared/foo/bar.luau"); !ok {
		t.Errorf("missing child delete: %s", serveDescribe(evs))
	}
	if _, ok := serveFind(evs, "DELETE", "shared/foo/sub"); !ok {
		t.Errorf("missing subfolder delete: %s", serveDescribe(evs))
	}
	if len(evs) != 2 {
		t.Errorf("want only direct children deleted: %s", serveDescribe(evs))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, owner := range s.owners {
		if strings.HasPrefix(owner, "shared/foo/") {
			t.Errorf("owner %q for %q not released", owner, k)
		}
	}
	if s.owners[instanceKey("ReplicatedStorage.shared", []string{"foo"})] != "shared/foo.luau" {
		t.Error("foo.luau lost ownership")
	}
}

// Deleting a folder whose own init owns the instance deletes it outright.
func TestResolveInitDirDelete(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "shared/foo/init.luau", "")
	serveWriteFile(t, s.root, "shared/foo/x.luau", "")
	s.primeKnown()
	s.snapshot()
	_, since, _ := s.queue.Since(0)

	os.RemoveAll(filepath.Join(s.root, "shared", "foo"))
	s.resolve("shared/foo")
	evs := serveDrain(t, s.queue, &since)
	if len(evs) != 1 || evs[0].Op != "DELETE" || evs[0].Path != "shared/foo" ||
		!slices.Equal(evs[0].InstancePath, []string{"foo"}) {
		t.Fatalf("events: %s", serveDescribe(evs))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.owners) != 0 {
		t.Errorf("owners not released: %v", s.owners)
	}
}

// Regression: when foo/init.luau owned the instance and the folder goes away,
// a shadowed foo.luau takes over.
func TestResolveInitDirDeletePromotesFile(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "shared/foo/init.luau", "")
	s.primeKnown()
	s.snapshot()
	serveWriteFile(t, s.root, "shared/foo.luau", "return 'file'")
	s.resolve("shared/foo.luau") // rejected: init owns it
	_, since, _ := s.queue.Since(0)

	os.RemoveAll(filepath.Join(s.root, "shared", "foo"))
	s.resolve("shared/foo")
	evs := serveDrain(t, s.queue, &since)
	if len(evs) != 2 || evs[0].Op != "DELETE" || evs[1].Op != "CREATE" || evs[1].Path != "shared/foo.luau" {
		t.Fatalf("events: %s", serveDescribe(evs))
	}
}

// Regression: deleting the owner of a duplicate mapping promotes the shadowed file.
func TestResolvePromotesShadowedDuplicate(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	var since uint64
	serveWriteFile(t, s.root, "shared/foo.luau", "return 'luau'")
	serveWriteFile(t, s.root, "shared/foo.lua", "return 'lua'")
	s.resolve("shared/foo.luau")
	s.resolve("shared/foo.lua")
	if evs := serveDrain(t, s.queue, &since); len(evs) != 1 || evs[0].Path != "shared/foo.luau" {
		t.Fatalf("setup: %s", serveDescribe(evs))
	}

	os.Remove(filepath.Join(s.root, "shared", "foo.luau"))
	s.resolve("shared/foo.luau")
	evs := serveDrain(t, s.queue, &since)
	if len(evs) != 2 || evs[0].Op != "DELETE" || evs[1].Op != "CREATE" || evs[1].Path != "shared/foo.lua" ||
		!strings.Contains(evs[1].Source, "'lua'") {
		t.Fatalf("events: %s", serveDescribe(evs))
	}
	// The promoted file is now the owner and syncs updates normally.
	serveWriteFile(t, s.root, "shared/foo.lua", "return 'lua2'")
	s.resolve("shared/foo.lua")
	if evs := serveDrain(t, s.queue, &since); len(evs) != 1 || evs[0].Op != "UPDATE" {
		t.Errorf("update after promotion: %s", serveDescribe(evs))
	}
}

// Regression: deleting a shadowed (non-owner) file emits nothing.
func TestResolveShadowedDeleteIsSilent(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	var since uint64
	serveWriteFile(t, s.root, "shared/foo.luau", "")
	serveWriteFile(t, s.root, "shared/foo.lua", "")
	s.primeKnown()
	s.resolve("shared/foo.luau")
	s.resolve("shared/foo.lua")
	serveDrain(t, s.queue, &since)

	os.Remove(filepath.Join(s.root, "shared", "foo.lua"))
	s.resolve("shared/foo.lua")
	if evs := serveDrain(t, s.queue, &since); len(evs) != 0 {
		t.Errorf("events: %s", serveDescribe(evs))
	}
}

// Regression: concurrent resolves of one new file used to push several CREATEs.
func TestResolveConcurrentNoDuplicateCreate(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "shared/foo.luau", "")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.resolve("shared/foo.luau")
		}()
	}
	wg.Wait()
	evs, _, _ := s.queue.Since(0)
	creates := 0
	for _, ev := range evs {
		if ev.Op == "CREATE" {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("%d CREATE events: %s", creates, serveDescribe(evs))
	}
}

func TestScheduleDebounces(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "shared/foo.luau", "")
	abs := filepath.Join(s.root, "shared", "foo.luau")
	for i := 0; i < 5; i++ {
		s.schedule(abs)
	}
	deadline := time.After(2 * time.Second)
	for {
		evs, _, _ := s.queue.Since(0)
		if len(evs) > 0 {
			if len(evs) != 1 || evs[0].Op != "CREATE" {
				t.Fatalf("events: %s", serveDescribe(evs))
			}
			return
		}
		select {
		case <-s.queue.Notify():
		case <-deadline:
			t.Fatal("debounced resolve never ran")
		}
	}
}

func TestWatchErrorReturned(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	missing := filepath.Join(s.root, "does-not-exist")
	if err := s.watch(missing); err == nil {
		t.Error("watching a missing dir should fail")
	}
	if _, logged := s.watchFailed.Load(missing); !logged {
		t.Error("failure not recorded")
	}
	if err := s.watch(s.root); err != nil {
		t.Errorf("watch(root) = %v", err)
	}
}

// ---------- snapshot ordering ----------

func TestSortByDepthStable(t *testing.T) {
	evs := []Event{
		{Path: "c", InstancePath: []string{"init", "c"}},
		{Path: "a", InstancePath: []string{"a"}},
		{Path: "d", InstancePath: []string{"init", "d", "e"}},
		{Path: "b", InstancePath: []string{"b"}},
		{Path: "e", InstancePath: []string{"init", "a"}},
	}
	sortByDepth(evs)
	var got []string
	for _, ev := range evs {
		got = append(got, ev.Path)
	}
	if want := []string{"a", "b", "c", "e", "d"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestSnapshotOrdering(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "server/a/b/c.luau", "")
	serveWriteFile(t, s.root, "server/svc.luau", "")
	serveWriteFile(t, s.root, "server/init.server.luau", "") // shadowed by the bootstrap
	serveWriteFile(t, s.root, "client/ctl.client.luau", "")
	serveWriteFile(t, s.root, "shared/m.luau", "")

	evs := s.snapshot()
	initAt := map[string]int{}
	for i, ev := range evs {
		if i > 0 && len(ev.InstancePath) < len(evs[i-1].InstancePath) {
			t.Fatalf("not sorted by depth: %s", serveDescribe(evs))
		}
		if slices.Equal(ev.InstancePath, []string{"init"}) {
			initAt[ev.Target] = i
		}
		if ev.Path == "server/init.server.luau" {
			t.Error("shadowed root init included in snapshot")
		}
	}
	if len(initAt) != 2 {
		t.Fatalf("bootstrap init events: %v (%s)", initAt, serveDescribe(evs))
	}
	for i, ev := range evs {
		if len(ev.InstancePath) > 1 && ev.InstancePath[0] == "init" {
			if at, ok := initAt[ev.Target]; !ok || at > i {
				t.Errorf("%s comes before its bootstrap init", ev.Path)
			}
		}
	}
	if len(evs) != 6 {
		t.Errorf("want 2 bootstrap + 4 files, got %s", serveDescribe(evs))
	}
}

// ---------- HTTP ----------

func serveHTTP(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.auth(s.handleHealth))
	mux.HandleFunc("/changes", s.auth(s.handleChanges))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func serveGetChanges(t *testing.T, url string) ChangesResponse {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out ChangesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func serveSetLongPoll(t *testing.T, d time.Duration) {
	t.Helper()
	old := longPollTimeout
	longPollTimeout = d
	t.Cleanup(func() { longPollTimeout = old })
}

func TestHandleChangesFullSnapshot(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	serveWriteFile(t, s.root, "shared/a.luau", "")
	s.queue.Push(Event{Op: "UPDATE"})
	ts := serveHTTP(t, s)

	for _, q := range []string{"", "?session=stale&since=1"} {
		got := serveGetChanges(t, ts.URL+"/changes"+q)
		if !got.Full || got.SessionID != s.queue.sessionID || got.Seq != 1 {
			t.Errorf("%q: full=%v session=%q seq=%d", q, got.Full, got.SessionID, got.Seq)
		}
		if _, ok := serveFind(got.Events, "CREATE", "shared/a.luau"); !ok {
			t.Errorf("%q: snapshot missing file: %s", q, serveDescribe(got.Events))
		}
	}
}

func TestHandleChangesPending(t *testing.T) {
	serveSetLongPoll(t, 5*time.Second)
	s := serveNewServer(t, defaultConfig())
	s.queue.Push(Event{Op: "UPDATE", Path: "x"}, Event{Op: "UPDATE", Path: "y"})
	ts := serveHTTP(t, s)

	start := time.Now()
	got := serveGetChanges(t, ts.URL+"/changes?session="+s.queue.sessionID+"&since=1")
	if got.Full || got.Seq != 2 || len(got.Events) != 1 || got.Events[0].Path != "y" {
		t.Errorf("response = %+v", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("pending events should return immediately")
	}

	// A seq from the future (server restarted with the same session id) forces a snapshot.
	if got := serveGetChanges(t, ts.URL+"/changes?session="+s.queue.sessionID+"&since=99"); !got.Full {
		t.Error("future seq should force a full snapshot")
	}
}

func TestHandleChangesLongPollWakes(t *testing.T) {
	serveSetLongPoll(t, 10*time.Second)
	s := serveNewServer(t, defaultConfig())
	ts := serveHTTP(t, s)

	go func() {
		time.Sleep(100 * time.Millisecond)
		s.queue.Push(Event{Op: "CREATE", Path: "late"})
	}()
	start := time.Now()
	got := serveGetChanges(t, ts.URL+"/changes?session="+s.queue.sessionID+"&since=0")
	if len(got.Events) != 1 || got.Events[0].Path != "late" || got.Seq != 1 {
		t.Errorf("response = %+v", got)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("long poll did not wake on push")
	}
}

func TestHandleChangesTimeout(t *testing.T) {
	serveSetLongPoll(t, 50*time.Millisecond)
	s := serveNewServer(t, defaultConfig())
	ts := serveHTTP(t, s)
	got := serveGetChanges(t, ts.URL+"/changes?session="+s.queue.sessionID+"&since=0")
	if got.Full || got.Events == nil || len(got.Events) != 0 || got.Seq != 0 {
		t.Errorf("timeout response = %+v", got)
	}
}

func TestHandleChangesContextCancel(t *testing.T) {
	serveSetLongPoll(t, time.Minute)
	s := serveNewServer(t, defaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/changes?session="+s.queue.sessionID, nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.handleChanges(rec, req)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after cancel")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("cancelled request wrote a body: %q", rec.Body.String())
	}
}

func TestAuth(t *testing.T) {
	cfg := defaultConfig()
	cfg.Token = "s3cret"
	s := serveNewServer(t, cfg)
	h := s.auth(s.handleHealth)

	tests := []struct {
		name, host, token string
		want              int
	}{
		{"no token", "127.0.0.1:8013", "", http.StatusUnauthorized},
		{"wrong token", "127.0.0.1:8013", "nope", http.StatusUnauthorized},
		{"prefix token", "127.0.0.1:8013", "s3cre", http.StatusUnauthorized},
		{"right token", "127.0.0.1:8013", "s3cret", http.StatusOK},
		{"localhost", "localhost:8013", "s3cret", http.StatusOK},
		{"localhost no port", "LOCALHOST", "s3cret", http.StatusOK},
		{"ipv6 loopback", "[::1]:8013", "s3cret", http.StatusOK},
		{"rebound host", "attacker.example:8013", "s3cret", http.StatusForbidden},
		{"rebound host no token", "attacker.example", "", http.StatusForbidden},
		{"localhost suffix", "localhost.attacker.example:8013", "s3cret", http.StatusForbidden},
		{"empty host", "", "s3cret", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/health", nil)
			req.Host = tt.host
			if tt.token != "" {
				req.Header.Set("X-ChainLink-Token", tt.token)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// Regression: with no token configured a foreign Host must still be refused.
func TestAuthNoTokenRejectsForeignHost(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	ts := serveHTTP(t, s)

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("loopback health status %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/changes", nil)
	req.Host = "rebind.attacker.example:8013"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign host status %d, want 403", resp.StatusCode)
	}
}

// ---------- benchmarks ----------

func serveBenchJSON(b *testing.B) any {
	m := map[string]any{}
	for i := 0; i < 50; i++ {
		m[fmt.Sprintf("key%02d", i)] = map[string]any{
			"name":  fmt.Sprintf("item \"%d\"\n", i),
			"price": float64(i) * 1.5,
			"tags":  []any{"a", "b", true, nil, 3.0},
		}
	}
	data, _ := json.Marshal(m)
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		b.Fatal(err)
	}
	return v
}

func BenchmarkJSONToLuau(b *testing.B) {
	v := serveBenchJSON(b)
	b.ReportAllocs()
	for b.Loop() {
		_ = jsonToLuau(v)
	}
}

// BenchmarkQueueSince models a client polling a full queue for its last few events.
func BenchmarkQueueSince(b *testing.B) {
	q := NewQueue()
	for i := 0; i < maxQueueLen+500; i++ {
		q.Push(Event{Op: "UPDATE", Path: "shared/x.luau"})
	}
	_, latest, _ := q.Since(q.seq)
	b.ReportAllocs()
	for b.Loop() {
		evs, _, ok := q.Since(latest - 5)
		if !ok || len(evs) != 5 {
			b.Fatal("bad since")
		}
	}
}

func BenchmarkClassify(b *testing.B) {
	cfg := defaultConfig()
	b.ReportAllocs()
	for b.Loop() {
		classify(cfg, "server/services/combat/DamageService.luau", false)
	}
}
