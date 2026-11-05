package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvertToSourcemapNode(t *testing.T) {
	tree := &Instance{Name: "Root", Class: "Folder", Children: []*Instance{
		{Name: "A", Class: "ModuleScript", FilePath: "shared/A.luau"},
		{Name: "B", Class: "Folder", Children: []*Instance{
			{Name: "C", Class: "Script", FilePath: "shared/B/C.server.luau"},
		}},
		{Name: "Tree", Class: "Model", FilePath: "prefab/Tree.rbxmx"},
	}}
	node := convertToSourcemapNode(tree)
	if node.FilePaths != nil || len(node.Children) != 3 {
		t.Fatalf("node = %+v", node)
	}
	if a := node.Children[0]; a.Name != "A" || a.ClassName != "ModuleScript" || len(a.FilePaths) != 1 || a.FilePaths[0] != "shared/A.luau" {
		t.Errorf("A = %+v", a)
	}
	if c := node.Children[1].Children[0]; c.Name != "C" || c.ClassName != "Script" {
		t.Errorf("C = %+v", c)
	}
	if p := node.Children[2]; p.Name != "Tree" || p.ClassName != "Model" || p.FilePaths[0] != "prefab/Tree.rbxmx" {
		t.Errorf("prefab = %+v", p)
	}

	data, err := json.Marshal(convertToSourcemapNode(&Instance{Name: "X", Class: "Folder"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `{"name":"X","className":"Folder"}` {
		t.Errorf("omitempty not honored: %s", got)
	}
}

func TestHandleSourcemap(t *testing.T) {
	buildHermeticHome(t)
	root := filepath.Join(t.TempDir(), "MyGame")
	buildWriteFile(t, root, "chain.toml", "")
	buildWriteFile(t, root, "shared/Util.luau", "return 1")
	buildWriteFile(t, root, "shared/Foo/Child.luau", "return 1")
	buildWriteFile(t, root, "shared/Foo.server.luau", "print(1)")
	buildWriteFile(t, root, "shared/Note.txt", "hi")
	buildWriteFile(t, root, "prefab/Trees.rbxmx", buildPrefab)
	t.Chdir(root)

	out := filepath.Join(t.TempDir(), "sourcemap.json")
	if err := handleSourcemap(out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var sm SourcemapNode
	if err := json.Unmarshal(data, &sm); err != nil {
		t.Fatal(err)
	}
	// Regression: the root used to be named "Teast".
	if sm.Name != "MyGame" || sm.ClassName != "DataModel" || sm.FilePaths[0] != "chain.toml" {
		t.Errorf("root = %+v", sm)
	}

	find := func(names ...string) *SourcemapNode {
		t.Helper()
		cur := &sm
		for _, n := range names {
			var next *SourcemapNode
			for _, c := range cur.Children {
				if c.Name == n {
					next = c
				}
			}
			if next == nil {
				t.Fatalf("node %v not found", names)
			}
			cur = next
		}
		return cur
	}

	// Regression: file paths used to be absolute.
	if u := find("ReplicatedStorage", "shared", "Util"); u.ClassName != "ModuleScript" || u.FilePaths[0] != "shared/Util.luau" {
		t.Errorf("Util = %+v", u)
	}
	if f := find("ReplicatedStorage", "shared", "Foo"); f.ClassName != "Script" || len(f.Children) != 1 {
		t.Errorf("Foo = %+v", f)
	}
	if n := find("ReplicatedStorage", "shared", "Note"); n.ClassName != "StringValue" {
		t.Errorf("Note = %+v", n)
	}
	// Regression: prefab nodes used to have empty name and className.
	// Models are named after their file, and their contents are listed.
	if p := find("ServerStorage", "Trees"); p.ClassName != "Model" || p.FilePaths[0] != "prefab/Trees.rbxmx" {
		t.Errorf("prefab = %+v", p)
	}
	if trunk := find("ServerStorage", "Trees", "Trunk"); trunk.ClassName != "Part" || trunk.FilePaths != nil {
		t.Errorf("prefab child = %+v", trunk)
	}
	if strings.Contains(string(data), `"name": ""`) {
		t.Error("sourcemap contains an empty name")
	}
	if strings.Contains(string(data), root) {
		t.Error("sourcemap contains absolute project paths")
	}
}
