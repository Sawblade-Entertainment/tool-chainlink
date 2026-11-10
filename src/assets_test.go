package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assetsProject switches into a fresh project with the given files under it.
func assetsProject(t *testing.T, files map[string]string) string {
	t.Helper()
	cliSetHome(t)
	dir := cliChdirTemp(t)
	for rel, content := range files {
		cliWriteFile(t, filepath.Join(dir, filepath.FromSlash(rel)), content, 0644)
	}
	return dir
}

func assetsReadLock(t *testing.T) assetLock {
	t.Helper()
	lock, err := readAssetLock(assetLockName)
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

func TestScanAssets(t *testing.T) {
	assetsProject(t, map[string]string{
		"assets/ui/Icon.PNG":    "png",
		"assets/sfx/hit.ogg":    "ogg",
		"assets/rig.rbxmx":      "<roblox/>",
		"assets/notes.txt":      "not an asset",
		"assets/.DS_Store":      "junk",
		"assets/.cache/big.png": "hidden dir",
	})
	found, err := scanAssets(assetsDir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, a := range found {
		got[a.Rel] = a.Kind.Type + " " + a.Kind.ContentType
		if len(a.Hash) != 64 {
			t.Errorf("%s hash = %q", a.Rel, a.Hash)
		}
	}
	want := map[string]string{
		"ui/Icon.PNG": "Image image/png",
		"sfx/hit.ogg": "Audio audio/ogg",
		"rig.rbxmx":   "Model model/x-rbxm",
	}
	if len(got) != len(want) {
		t.Errorf("found %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestAssetCreator(t *testing.T) {
	if _, err := assetCreator(RobloxConfig{}); err == nil {
		t.Error("no creator accepted")
	}
	if c, _ := assetCreator(RobloxConfig{UserId: 7}); c["userId"] != "7" || len(c) != 1 {
		t.Errorf("user creator = %v", c)
	}
	if c, _ := assetCreator(RobloxConfig{UserId: 7, GroupId: 9}); c["groupId"] != "9" || len(c) != 1 {
		t.Errorf("group creator = %v", c)
	}
}

func TestAssetDisplayName(t *testing.T) {
	if got := assetDisplayName("ui/Play Button.png"); got != "Play Button" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 60) + ".png"
	if got := assetDisplayName(long); len([]rune(got)) != maxAssetName {
		t.Errorf("long name has %d runes", len([]rune(got)))
	}
}

func TestSyncAssetsUploadsOnlyChanges(t *testing.T) {
	dir := assetsProject(t, map[string]string{
		"assets/ui/icon.png": "v1",
		"assets/hit.ogg":     "ogg",
	})
	f, c := cloudNewFake(t)
	creator := map[string]string{"userId": "7"}

	if err := syncAssets(c, creator, false); err != nil {
		t.Fatal(err)
	}
	if len(f.uploads) != 2 {
		t.Fatalf("first sync uploaded %d files", len(f.uploads))
	}
	up := f.uploads[0] // hit.ogg sorts first
	if up.Request["assetType"] != "Audio" || up.Request["displayName"] != "hit" || up.Request["description"] != "assets/hit.ogg" {
		t.Errorf("request = %v", up.Request)
	}
	if ctx, _ := up.Request["creationContext"].(map[string]any); ctx["creator"].(map[string]any)["userId"] != "7" {
		t.Errorf("creationContext = %v", up.Request["creationContext"])
	}
	lock := assetsReadLock(t)
	if lock["hit.ogg"].ID != "1001" || lock["ui/icon.png"].ID != "1002" {
		t.Errorf("lock = %v", lock)
	}

	// Nothing changed: nothing uploads.
	if err := syncAssets(c, creator, false); err != nil {
		t.Fatal(err)
	}
	if len(f.uploads) != 2 {
		t.Errorf("unchanged sync uploaded %d more", len(f.uploads)-2)
	}

	// An edit re-uploads that file; a removal drops it from the lock.
	cliWriteFile(t, filepath.Join(dir, "assets/ui/icon.png"), "v2", 0644)
	os.Remove(filepath.Join(dir, "assets/hit.ogg"))
	if err := syncAssets(c, creator, false); err != nil {
		t.Fatal(err)
	}
	if len(f.uploads) != 3 || f.uploads[2].Filename != "icon.png" || string(f.uploads[2].Data) != "v2" {
		t.Errorf("changed sync uploads = %d", len(f.uploads))
	}
	lock = assetsReadLock(t)
	if _, ok := lock["hit.ogg"]; ok || lock["ui/icon.png"].ID != "1003" || len(lock) != 1 {
		t.Errorf("lock after edit = %v", lock)
	}
}

func TestSyncAssetsKeepsSuccessesOnFailure(t *testing.T) {
	assetsProject(t, map[string]string{
		"assets/a.png": "a",
		"assets/b.png": "b",
		"assets/c.png": "c",
	})
	f, c := cloudNewFake(t)
	f.fail["b"] = "moderated"
	err := syncAssets(c, map[string]string{"userId": "1"}, false)
	if err == nil || !strings.Contains(err.Error(), "1 of 3") {
		t.Errorf("err = %v", err)
	}
	lock := assetsReadLock(t)
	if _, ok := lock["b.png"]; ok || lock["a.png"].ID == "" || lock["c.png"].ID == "" {
		t.Errorf("lock = %v", lock)
	}
}

func TestSyncAssetsDryRun(t *testing.T) {
	assetsProject(t, map[string]string{"assets/a.png": "a"})
	// A zero openCloud would fail any request, so this also proves none is sent.
	if err := syncAssets(openCloud{}, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(assetLockName); !os.IsNotExist(err) {
		t.Error("dry run wrote the lock file")
	}
}

func TestSyncAssetsNoDir(t *testing.T) {
	assetsProject(t, nil)
	if err := syncAssets(openCloud{}, nil, true); err == nil {
		t.Error("missing assets/ accepted")
	}
}

func TestHandleAssetsNeedsCreator(t *testing.T) {
	assetsProject(t, map[string]string{"assets/a.png": "a"})
	t.Setenv("ROBLOX_API_KEY", "k")
	if err := handleAssets(nil); err == nil || !strings.Contains(err.Error(), "creator") {
		t.Errorf("err = %v", err)
	}
}

func TestAssetModuleSource(t *testing.T) {
	src, err := assetModuleSource([]byte(`{"ui/b.png":{"id":"2","hash":"x"},"a.ogg":{"id":"1","hash":"y"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `return {["a.ogg"] = "rbxassetid://1", ["ui/b.png"] = "rbxassetid://2"}`; src != want {
		t.Errorf("got  %s\nwant %s", src, want)
	}
	if _, err := assetModuleSource([]byte("{")); err == nil {
		t.Error("malformed lock accepted")
	}
}

func TestBuildIncludesAssetModule(t *testing.T) {
	buildHermeticHome(t)
	root := t.TempDir()
	buildWriteFile(t, root, assetLockName, `{"a.png":{"id":"5","hash":"h"}}`)
	b := NewBuilder(defaultConfig(), root)
	b.populatePlace()
	inst := buildFind(t, b.top, "ReplicatedStorage", assetModuleName)
	if inst.Class != "ModuleScript" || inst.FilePath != assetLockName {
		t.Errorf("assets module = %s %s", inst.Class, inst.FilePath)
	}
	if src := buildSource(t, inst); !strings.Contains(src, `["a.png"] = "rbxassetid://5"`) {
		t.Errorf("source = %s", src)
	}

	// A malformed lock fails the build instead of shipping a broken module.
	buildWriteFile(t, root, assetLockName, "{")
	b = NewBuilder(defaultConfig(), root)
	b.populatePlace()
	if b.readErrs != 1 {
		t.Errorf("readErrs = %d", b.readErrs)
	}
}

func TestParseScriptInjectsAsset(t *testing.T) {
	cfg := ProjectConfig{DisableBootstrap: true}
	got := parseScript(cfg, `local icon = asset "ui/icon.png"`, "shared/M.luau")
	if !strings.Contains(got, assetGlobal) {
		t.Errorf("asset macro not injected: %q", got)
	}
	if got := parseScript(cfg, `local x = assets.y`, "shared/M.luau"); strings.Contains(got, assetGlobal) {
		t.Error("asset macro injected for '= assets'")
	}
}

func TestServeSyncsAssetLock(t *testing.T) {
	s := serveNewServer(t, defaultConfig())
	var since uint64

	if c := classify(s.conf(), assetLockName, false); c.Target != "ReplicatedStorage" || c.InstancePath[0] != assetModuleName {
		t.Errorf("classify = %+v", c)
	}
	for _, ev := range s.snapshot() {
		if ev.Path == assetLockName {
			t.Fatal("snapshot has the asset module before the lock exists")
		}
	}

	serveWriteFile(t, s.root, assetLockName, `{"a.png":{"id":"1","hash":"h"}}`)
	s.resolve(assetLockName)
	evs := serveDrain(t, s.queue, &since)
	ev, ok := serveFind(evs, "CREATE", assetLockName)
	if !ok || ev.ClassName != "ModuleScript" || !strings.Contains(ev.Source, "rbxassetid://1") {
		t.Fatalf("create = %s", serveDescribe(evs))
	}

	serveWriteFile(t, s.root, assetLockName, `{"a.png":{"id":"2","hash":"h2"}}`)
	s.resolve(assetLockName)
	evs = serveDrain(t, s.queue, &since)
	if ev, ok := serveFind(evs, "UPDATE", assetLockName); !ok || !strings.Contains(ev.Source, "rbxassetid://2") {
		t.Fatalf("update = %s", serveDescribe(evs))
	}

	if _, ok := serveFind(s.snapshot(), "CREATE", assetLockName); !ok {
		t.Error("snapshot is missing the asset module")
	}

	os.Remove(filepath.Join(s.root, assetLockName))
	s.resolve(assetLockName)
	evs = serveDrain(t, s.queue, &since)
	if _, ok := serveFind(evs, "DELETE", assetLockName); !ok {
		t.Fatalf("delete = %s", serveDescribe(evs))
	}
}
