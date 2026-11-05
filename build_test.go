package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

// --- helpers ---

// buildWriteFile writes content to root/rel, creating parent directories.
func buildWriteFile(tb testing.TB, root, rel, content string) string {
	tb.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tb.Fatal(err)
	}
	return p
}

// buildHermeticHome points the home directory (and so packagesBase) at a temp dir.
func buildHermeticHome(tb testing.TB) string {
	tb.Helper()
	home := tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("USERPROFILE", home)
	return home
}

// buildFind follows names from the builder's top-level instances.
func buildFind(tb testing.TB, top []*Instance, names ...string) *Instance {
	tb.Helper()
	list := top
	var cur *Instance
	for _, n := range names {
		cur = nil
		for _, c := range list {
			if c.Name == n {
				cur = c
				break
			}
		}
		if cur == nil {
			tb.Fatalf("instance %q not found (path %v)", n, names)
		}
		list = cur.Children
	}
	return cur
}

func buildProp(inst *Instance, name string) (Property, bool) {
	for _, p := range inst.Props {
		if p.Name == name {
			return p, true
		}
	}
	return Property{}, false
}

func buildSource(tb testing.TB, inst *Instance) string {
	tb.Helper()
	p, ok := buildProp(inst, "Source")
	if !ok {
		tb.Fatalf("%s (%s) has no Source", inst.Name, inst.Class)
	}
	return p.Value
}

// buildXItem / buildXRoot decode the subset of the rbxlx format we emit.
type buildXProp struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}

type buildXItem struct {
	Class    string `xml:"class,attr"`
	Referent string `xml:"referent,attr"`
	Props    struct {
		Strings   []buildXProp `xml:"string"`
		Protected []buildXProp `xml:"ProtectedString"`
	} `xml:"Properties"`
	Items []buildXItem `xml:"Item"`
}

func (it buildXItem) name() string {
	for _, p := range it.Props.Strings {
		if p.Name == "Name" {
			return p.Value
		}
	}
	return ""
}

func (it buildXItem) child(name string) *buildXItem {
	for i := range it.Items {
		if it.Items[i].name() == name {
			return &it.Items[i]
		}
	}
	return nil
}

type buildXRoot struct {
	Items  []buildXItem `xml:"Item"`
	Shared []struct {
		MD5   string `xml:"md5,attr"`
		Value string `xml:",chardata"`
	} `xml:"SharedStrings>SharedString"`
}

func buildParse(tb testing.TB, data []byte) buildXRoot {
	tb.Helper()
	var root buildXRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		tb.Fatalf("output is not valid XML: %v\n%s", err, data)
	}
	return root
}

// buildRbxmx returns an .rbxmx document with n top-level Parts, each carrying
// a SharedString reference and some bulk properties.
func buildRbxmx(n int) string {
	var sb strings.Builder
	sb.WriteString(`<roblox version="4">` + "\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, `<Item class="Part" referent="RBXA%d"><Properties><string name="Name">Part%d</string>`, i, i)
		fmt.Fprintf(&sb, `<SharedString name="PhysicalConfigData">h%d</SharedString>`, i)
		sb.WriteString(`<CoordinateFrame name="CFrame"><X>0</X><Y>0</Y><Z>0</Z><R00>1</R00><R01>0</R01><R02>0</R02></CoordinateFrame>`)
		fmt.Fprintf(&sb, `</Properties><Item class="Attachment" referent="RBXB%d"><Properties><string name="Name">Att</string></Properties></Item></Item>`+"\n", i)
	}
	sb.WriteString("<SharedStrings>\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, `<SharedString md5="h%d">%s</SharedString>`+"\n", i, strings.Repeat("QUJD", 16))
	}
	sb.WriteString("</SharedStrings>\n</roblox>\n")
	return sb.String()
}

const buildPrefab = `<roblox version="4">
<Item class="Model" referent="RBXP1"><Properties><string name="Name">Tree</string></Properties>
<Item class="Part" referent="RBXP2"><Properties><string name="Name">Trunk</string></Properties></Item>
</Item>
<SharedStrings><SharedString md5="abc">ZGF0YQ==</SharedString></SharedStrings>
</roblox>`

// --- scriptClassForFile ---

func TestScriptClassForFile(t *testing.T) {
	tests := []struct {
		in, class, name string
		ok              bool
	}{
		{"Foo.luau", "ModuleScript", "Foo", true},
		{"Foo.lua", "ModuleScript", "Foo", true},
		{"Foo.server.lua", "Script", "Foo", true},
		{"Foo.CLIENT.LUAU", "LocalScript", "Foo", true},
		{"MyMod.Server.Luau", "Script", "MyMod", true},
		{"init.server.luau", "Script", "init", true},
		{"init.luau", "ModuleScript", "init", true},
		{"Foo.txt", "", "", false},
		{"Foo", "", "", false},
		{".luau", "ModuleScript", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			class, name, ok := scriptClassForFile(tt.in)
			if class != tt.class || name != tt.name || ok != tt.ok {
				t.Errorf("scriptClassForFile(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.in, class, name, ok, tt.class, tt.name, tt.ok)
			}
		})
	}
}

// --- sanitize / escaping ---

func TestSanitize(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "hello world", "hello world"},
		{"keeps tab nl cr", "a\tb\nc\rd", "a\tb\nc\rd"},
		{"strips controls", "a\x00b\x0Bc￾d\x1F", "abcd"},
		{"invalid utf8", "a\xffb\xc3", "ab"},
		{"multibyte kept", "héllo ☃ 𝄞", "héllo ☃ 𝄞"},
		// Regression: a literal U+FFFD used to be dropped when the slow path ran.
		{"literal fffd preserved", "x�\x00y", "x�y"},
		{"literal fffd with invalid byte", "�\xfe", "�"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitize(tt.in); got != tt.want {
				t.Errorf("sanitize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeValidNoAlloc(t *testing.T) {
	s := strings.Repeat("valid text \t\n", 10)
	if got := sanitize(s); unsafe.StringData(got) != unsafe.StringData(s) {
		t.Error("sanitize copied a valid string")
	}
	if n := testing.AllocsPerRun(100, func() { _ = sanitize(s) }); n != 0 {
		t.Errorf("sanitize allocated %v times for a valid string", n)
	}
}

func TestEscRoundTrip(t *testing.T) {
	inputs := []string{
		"",
		"plain",
		`a & b < c > d " e ' f`,
		"tabs\tand\nnewlines\r\n",
		"<script>alert('x')</script>",
		"ctrl\x00\x01chars￾",
		"&amp; already escaped",
	}
	for _, in := range inputs {
		attr, text := escAttr(in), escText(in)
		if strings.ContainsAny(attr, `<>"'`) || strings.ContainsAny(text, `<>"'`) {
			t.Errorf("unescaped special char in %q / %q", attr, text)
		}
		doc := `<v a="` + attr + `">` + text + `</v>`
		var v struct {
			A    string `xml:"a,attr"`
			Text string `xml:",chardata"`
		}
		if err := xml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("escape of %q is not valid XML: %v (%s)", in, err, doc)
		}
		want := sanitize(in)
		if v.A != want || v.Text != want {
			t.Errorf("round trip of %q: attr %q text %q, want %q", in, v.A, v.Text, want)
		}
	}
}

func TestCDATARoundTrip(t *testing.T) {
	inputs := []string{
		"",
		"plain source",
		"a]]>b",
		"]]>",
		"x]]>]]>y]]]>z]]",
		"local s = [[nested [[ ]] ]]>",
		"bad\x00char ]]> <&>",
	}
	for _, in := range inputs {
		doc := "<v>" + cdata(in) + "</v>"
		var v struct {
			Text string `xml:",chardata"`
		}
		if err := xml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("cdata(%q) is not valid XML: %v (%s)", in, err, doc)
		}
		if want := sanitize(in); v.Text != want {
			t.Errorf("cdata round trip of %q = %q, want %q", in, v.Text, want)
		}
	}
}

// --- prefab extraction ---

func TestExtractItems(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{
			name: "single",
			in:   `<roblox><Item class="Part"><Properties/></Item></roblox>`,
			want: []string{`<Item class="Part"><Properties/></Item>`},
		},
		{
			name: "multiple",
			in:   "<roblox>\n<Item class=\"A\"></Item>\n<Item class=\"B\"></Item>\n</roblox>",
			want: []string{`<Item class="A"></Item>`, `<Item class="B"></Item>`},
		},
		{
			name: "nested returns outer only",
			in:   `<roblox><Item class="A"><Item class="B"><Item class="C"></Item></Item></Item></roblox>`,
			want: []string{`<Item class="A"><Item class="B"><Item class="C"></Item></Item></Item>`},
		},
		{
			name: "self closing",
			in:   `<roblox><Item class="A"/></roblox>`,
			want: []string{`<Item class="A"/>`},
		},
		{
			name: "attribute with gt",
			in:   `<roblox><Item class="a>b" referent="x>y"><Properties/></Item></roblox>`,
			want: []string{`<Item class="a>b" referent="x>y"><Properties/></Item>`},
		},
		{name: "no items", in: `<roblox><Other/></roblox>`, wantErr: true},
		{name: "malformed", in: `<roblox><Item class="A"></roblox>`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractItems([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			for _, s := range got {
				if !strings.Contains(tt.in, s) || !strings.HasPrefix(s, "<Item") {
					t.Errorf("%q is not an <Item substring of the input", s)
				}
			}
		})
	}
}

func TestExtractItemsLargeIsExactSubstrings(t *testing.T) {
	doc := buildRbxmx(50)
	items, err := extractItems([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 50 {
		t.Fatalf("got %d items, want 50", len(items))
	}
	for _, s := range items {
		if !strings.HasPrefix(s, "<Item") || !strings.HasSuffix(s, "</Item>") || !strings.Contains(doc, s) {
			t.Fatalf("bad item slice %q", s)
		}
	}
}

func TestExtractSharedStrings(t *testing.T) {
	doc := `<roblox>
<Item class="Part"><Properties><SharedString name="X">outside</SharedString></Properties></Item>
<SharedString md5="stray">ignored</SharedString>
<SharedStrings>
	<SharedString md5="a">AAA</SharedString>
	<SharedString>no md5</SharedString>
	<SharedString md5="b">B&amp;B</SharedString>
</SharedStrings>
</roblox>`
	got := map[string]string{}
	if err := extractSharedStrings([]byte(doc), got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "AAA", "b": "B&B"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}

	if err := extractSharedStrings([]byte(`<roblox><SharedStrings><SharedString md5="a">x</roblox>`), map[string]string{}); err == nil {
		t.Error("expected error for malformed XML")
	}
}

func TestScanModelNamesAndRefs(t *testing.T) {
	items, refs, err := scanModel([]byte(buildPrefab), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Class != "Model" || items[0].Name != "Tree" {
		t.Fatalf("items = %+v", items)
	}
	if fmt.Sprint(refs) != "[RBXP1 RBXP2]" {
		t.Errorf("refs = %v", refs)
	}
	// A nested Item's Name must not override the top-level one.
	doc := `<roblox><Item class="Folder"><Item class="Part"><Properties><string name="Name">Inner</string></Properties></Item></Item></roblox>`
	items, _, err = scanModel([]byte(doc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Name != "" || items[0].Class != "Folder" {
		t.Errorf("items = %+v", items)
	}
}

func TestRawStartTag(t *testing.T) {
	data := []byte(`xx<Item class="a">body`)
	end := int64(strings.Index(string(data), ">") + 1)
	if got := string(rawStartTag(data, end)); got != `<Item class="a">` {
		t.Errorf("got %q", got)
	}
	if got := rawStartTag([]byte(`no tag here`), 5); got != nil {
		t.Errorf("got %q, want nil", got)
	}
}

// --- Builder tree ---

func TestResolveTargetAndPlace(t *testing.T) {
	b := NewBuilder(defaultConfig(), "")
	a := b.resolveTarget("ReplicatedStorage.shared.lib")
	if b.resolveTarget("ReplicatedStorage.shared.lib") != a {
		t.Error("resolveTarget is not idempotent")
	}
	if len(b.top) != 1 || b.top[0].Class != "ReplicatedStorage" {
		t.Fatalf("top = %+v", b.top)
	}
	if got := buildFind(t, b.top, "ReplicatedStorage", "shared", "lib"); got != a || got.Class != "Folder" {
		t.Errorf("bad parent chain: %+v", got)
	}

	sps := b.resolveTarget("StarterPlayer.StarterPlayerScripts")
	if sps.Class != "StarterPlayerScripts" {
		t.Errorf("class = %q", sps.Class)
	}
	if b.resolveTarget("StarterPlayer.StarterCharacterScripts").Class != "StarterCharacterScripts" {
		t.Error("StarterCharacterScripts class not set")
	}

	leaf := b.place("ServerScriptService", []string{"a", "b", "C"}, "ModuleScript")
	if leaf.Class != "ModuleScript" || leaf.Name != "C" {
		t.Errorf("leaf = %+v", leaf)
	}
	if mid := buildFind(t, b.top, "ServerScriptService", "a", "b"); mid.Class != "Folder" {
		t.Errorf("intermediate class = %q", mid.Class)
	}
	if b.place("ServerScriptService", []string{"a", "b", "C"}, "ModuleScript") != leaf {
		t.Error("place is not idempotent")
	}
}

// Regression: a directory placed as a Folder first must take the class of an
// init.server/init.client file (or a sibling Foo.server.luau) placed later.
func TestPlaceUpgradesFolder(t *testing.T) {
	b := NewBuilder(defaultConfig(), "")
	dir := b.place("ReplicatedStorage", []string{"Foo"}, "")
	if dir.Class != "Folder" {
		t.Fatalf("class = %q", dir.Class)
	}
	if got := b.place("ReplicatedStorage", []string{"Foo"}, "Script"); got != dir || got.Class != "Script" {
		t.Errorf("got %+v, want upgraded Script", got)
	}
	// An existing script class is not changed.
	if got := b.place("ReplicatedStorage", []string{"Foo"}, "LocalScript"); got.Class != "Script" {
		t.Errorf("class = %q, want Script", got.Class)
	}
}

func TestSetSource(t *testing.T) {
	inst := &Instance{Class: "Folder"}
	setSource(inst, "a")
	if inst.Class != "ModuleScript" {
		t.Errorf("class = %q", inst.Class)
	}
	s := &Instance{Class: "Script"}
	setSource(s, "one")
	setSource(s, "two")
	if s.Class != "Script" || len(s.Props) != 1 || s.Props[0].Value != "two" || s.Props[0].Type != "ProtectedString" {
		t.Errorf("props = %+v class %q", s.Props, s.Class)
	}
}

// --- parseScript ---

func TestParseScript(t *testing.T) {
	const header = "--!strict\n--!native\n--!optimize 2\n"
	on := ProjectConfig{}
	off := ProjectConfig{DisableBootstrap: true}

	got := parseScript(on, "print(1)", "server/FooService.luau")
	if want := header + "local mod = {}\nprint(1)\nreturn mod"; got != want {
		t.Errorf("service wrap:\n%q\nwant\n%q", got, want)
	}
	if got := parseScript(on, "x", "client/BarController.lua"); !strings.Contains(got, "local mod = {}\nx\nreturn mod") {
		t.Errorf("controller not wrapped: %q", got)
	}
	if got := parseScript(off, "print(1)", "server/FooService.luau"); got != header+"print(1)" {
		t.Errorf("wrapped with bootstrap disabled: %q", got)
	}
	if got := parseScript(on, "print(1)", "shared/Util.luau"); got != header+"print(1)" {
		t.Errorf("plain module: %q", got)
	}
	if got := parseScript(on, "--!strict\nprint(1)", "shared/Util.luau"); got != "--!strict\nprint(1)" {
		t.Errorf("header duplicated: %q", got)
	}
	if got := parseScript(on, "x", "shared/U.luau"); strings.Count(got, "--!strict") != 1 {
		t.Errorf("header count: %q", got)
	}

	// Regression: blank lines (and so multi-line strings) are preserved.
	src := "local s = [[a\n\nb]]\n\nprint(s)"
	if got := parseScript(on, src, "server/XService.luau"); !strings.Contains(got, src) {
		t.Errorf("source altered: %q", got)
	}
}

func TestParseScriptInjectGlobals(t *testing.T) {
	cfg := ProjectConfig{DisableBootstrap: true}
	useBody, newBody := InjectGlobals[0].Body, InjectGlobals[1].Body
	tests := []struct {
		src          string
		wantUse, new bool
	}{
		{`local m = use("x")`, true, false},
		{"local m =use'x'", true, false},
		{"local t = {\n\tm =\n\tuse(1)}", true, false},
		{`local p = new("Part")`, false, true},
		{`local user = 1; local x = user`, false, false},        // regression: '= user'
		{`local x = newValue; local y = renew()`, false, false}, // regression: '= newValue'
		{`use("x")`, false, false},
		{"local a = use(1)\nlocal b = new('P')", true, true},
	}
	for _, tt := range tests {
		got := parseScript(cfg, tt.src, "shared/M.luau")
		if strings.Contains(got, useBody) != tt.wantUse || strings.Contains(got, newBody) != tt.new {
			t.Errorf("parseScript(%q): use=%v new=%v, want %v %v", tt.src, strings.Contains(got, useBody), strings.Contains(got, newBody), tt.wantUse, tt.new)
		}
		if !strings.HasSuffix(got, tt.src) {
			t.Errorf("source not preserved at end: %q", got)
		}
	}
}

// Regression: with both macros present the prelude order used to follow map iteration.
func TestParseScriptDeterministic(t *testing.T) {
	cfg := ProjectConfig{}
	src := "local a = use(\"x\")\nlocal p = new(\"Part\")\n"
	first := parseScript(cfg, src, "server/AService.luau")
	if strings.Index(first, "function use") > strings.Index(first, "function new") {
		t.Error("use() should precede new() in the prelude")
	}
	for i := 0; i < 100; i++ {
		if got := parseScript(cfg, src, "server/AService.luau"); got != first {
			t.Fatalf("run %d differs:\n%q\nvs\n%q", i, got, first)
		}
	}
}

// --- Serialization ---

func TestBuildGolden(t *testing.T) {
	b := NewBuilder(defaultConfig(), "")
	b.service("ReplicatedStorage")
	m := b.place("ReplicatedStorage", []string{"lib", "Mod"}, "ModuleScript")
	setSource(m, "return \"]]>\"\x00 & <x>")
	sv := b.place("ReplicatedStorage", []string{"lib", "Note"}, "StringValue")
	setProp(sv, Property{Name: "Value", Type: "string", Value: "a<b"})

	got := string(b.build())
	want := `<roblox xmlns:xmime="http://www.w3.org/2005/05/xmlmime" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:noNamespaceSchemaLocation="http://www.roblox.com/roblox.xsd" version="4">
	<Item class="ReplicatedStorage" referent="RBX00000000000000000000000000000001">
		<Properties>
			<string name="Name">ReplicatedStorage</string>
		</Properties>
		<Item class="Folder" referent="RBX00000000000000000000000000000002">
			<Properties>
				<string name="Name">lib</string>
			</Properties>
			<Item class="ModuleScript" referent="RBX00000000000000000000000000000003">
				<Properties>
					<string name="Name">Mod</string>
					<ProtectedString name="Source"><![CDATA[return "]]]]><![CDATA[>" & <x>]]></ProtectedString>
				</Properties>
			</Item>
			<Item class="StringValue" referent="RBX00000000000000000000000000000004">
				<Properties>
					<string name="Name">Note</string>
					<string name="Value">a&lt;b</string>
				</Properties>
			</Item>
		</Item>
	</Item>
</roblox>
`
	if got != want {
		t.Errorf("build output mismatch:\n%s\nwant:\n%s", got, want)
	}

	root := buildParse(t, []byte(got))
	mod := root.Items[0].child("lib").child("Mod")
	if mod == nil || mod.Props.Protected[0].Value != "return \"]]>\" & <x>" {
		t.Errorf("decoded module = %+v", mod)
	}
}

func TestBuildRawXMLAndSharedStrings(t *testing.T) {
	b := NewBuilder(defaultConfig(), "")
	ss := b.service("ServerStorage")
	raw := `<Item class="Part" referent="RBXraw"><Properties><string name="Name">P</string></Properties></Item>`
	ss.Children = append(ss.Children, &Instance{RawXML: raw, Name: "P", Class: "Part"})
	for _, k := range []string{"zz", "aa", "mm", "bb"} {
		b.sharedStrings[k] = "v-" + k
	}
	out := b.build()
	if !bytes.Contains(out, []byte(raw+"\n")) {
		t.Error("RawXML not emitted verbatim")
	}
	root := buildParse(t, out)
	var order []string
	for _, s := range root.Shared {
		order = append(order, s.MD5+"="+s.Value)
	}
	if got := strings.Join(order, ","); got != "aa=v-aa,bb=v-bb,mm=v-mm,zz=v-zz" {
		t.Errorf("shared strings order = %s", got)
	}
	// Reproducible across builds.
	for i := 0; i < 20; i++ {
		if !bytes.Equal(b.build(), out) {
			t.Fatal("build output is not deterministic")
		}
	}
}

// --- syncDir ---

func buildProject(t *testing.T) (root string, b *Builder) {
	t.Helper()
	buildHermeticHome(t)
	root = t.TempDir()
	return root, NewBuilder(defaultConfig(), root)
}

func TestSyncDirRoots(t *testing.T) {
	root, b := buildProject(t)
	buildWriteFile(t, root, "shared/Util.luau", "return 1")
	buildWriteFile(t, root, "shared/lib/Deep.lua", "return 2")
	buildWriteFile(t, root, "server/Main.server.luau", "print('s')")
	buildWriteFile(t, root, "client/Ui.luau", "return 3")
	b.populatePlace()

	if u := buildFind(t, b.top, "ReplicatedStorage", "shared", "Util"); u.Class != "ModuleScript" || !strings.HasSuffix(buildSource(t, u), "return 1") {
		t.Errorf("Util = %+v", u)
	}
	if d := buildFind(t, b.top, "ReplicatedStorage", "shared", "lib", "Deep"); d.FilePath != "shared/lib/Deep.lua" {
		t.Errorf("FilePath = %q, want relative slash path", d.FilePath)
	}
	// Bootstrapped roots live under the embedded init script.
	init := buildFind(t, b.top, "ServerScriptService", "init")
	if init.Class != "Script" || !strings.Contains(buildSource(t, init), "Server Bootstrapper") {
		t.Errorf("server init = %s", init.Class)
	}
	if m := buildFind(t, b.top, "ServerScriptService", "init", "Main"); m.Class != "Script" {
		t.Errorf("Main class = %q", m.Class)
	}
	if c := buildFind(t, b.top, "StarterPlayer", "StarterPlayerScripts", "init"); c.Class != "LocalScript" {
		t.Errorf("client init class = %q", c.Class)
	}
	buildFind(t, b.top, "StarterPlayer", "StarterPlayerScripts", "init", "Ui")
	if b.readErrs != 0 {
		t.Errorf("readErrs = %d", b.readErrs)
	}
}

func TestSyncDirBootstrapDisabled(t *testing.T) {
	root, b := buildProject(t)
	b.cfg.DisableBootstrap = true
	buildWriteFile(t, root, "server/Main.server.luau", "print('s')")
	b.populatePlace()
	m := buildFind(t, b.top, "ServerScriptService", "Main")
	if m.Class != "Script" {
		t.Errorf("class = %q", m.Class)
	}
	for _, c := range buildFind(t, b.top, "ServerScriptService").Children {
		if c.Name == "init" {
			t.Error("bootstrap init created while disabled")
		}
	}
}

// Regression: Foo/ + Foo.server.luau and dir/init.client.luau used to build as ModuleScripts.
func TestSyncDirScriptMergesWithFolder(t *testing.T) {
	root, b := buildProject(t)
	buildWriteFile(t, root, "shared/Foo/Child.luau", "return 1")
	buildWriteFile(t, root, "shared/Foo.server.luau", "print('foo')")
	buildWriteFile(t, root, "shared/Bar/init.client.luau", "print('bar')")
	buildWriteFile(t, root, "shared/Bar/Sub.luau", "return 2")
	buildWriteFile(t, root, "shared/Baz/init.server.luau", "print('baz')")
	b.populatePlace()

	shared := buildFind(t, b.top, "ReplicatedStorage", "shared")
	count := 0
	for _, c := range shared.Children {
		if c.Name == "Foo" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d Foo instances, want 1", count)
	}
	foo := buildFind(t, b.top, "ReplicatedStorage", "shared", "Foo")
	if foo.Class != "Script" || !strings.HasSuffix(buildSource(t, foo), "print('foo')") {
		t.Errorf("Foo = %s", foo.Class)
	}
	buildFind(t, b.top, "ReplicatedStorage", "shared", "Foo", "Child")

	if bar := buildFind(t, b.top, "ReplicatedStorage", "shared", "Bar"); bar.Class != "LocalScript" || bar.FilePath != "shared/Bar/init.client.luau" {
		t.Errorf("Bar = %s %q", bar.Class, bar.FilePath)
	}
	buildFind(t, b.top, "ReplicatedStorage", "shared", "Bar", "Sub")
	if baz := buildFind(t, b.top, "ReplicatedStorage", "shared", "Baz"); baz.Class != "Script" {
		t.Errorf("Baz = %s", baz.Class)
	}
}

func TestSyncDirFileKinds(t *testing.T) {
	root, b := buildProject(t)
	buildWriteFile(t, root, "shared/Data.json", `{"k": [1, true, null]}`)
	buildWriteFile(t, root, "shared/Bad.json", `{not json`)
	buildWriteFile(t, root, "shared/Note.txt", "hello <world>")
	buildWriteFile(t, root, "shared/Bin.rbxm", "\x00binary")
	buildWriteFile(t, root, "shared/Skip.png", "x")
	buildWriteFile(t, root, "prefab/Trees.rbxmx", buildPrefab)
	buildWriteFile(t, root, "prefab/Nested/Rock.rbxmx", `<roblox><Item class="Part" referent="RBXR"><Properties/></Item></roblox>`)
	b.populatePlace()

	data := buildFind(t, b.top, "ReplicatedStorage", "shared", "Data")
	if src := buildSource(t, data); data.Class != "ModuleScript" || !strings.HasPrefix(src, "return {") {
		t.Errorf("json source = %q", src)
	}
	if src := buildSource(t, buildFind(t, b.top, "ReplicatedStorage", "shared", "Bad")); src != "return {not json" {
		t.Errorf("invalid json source = %q", src)
	}

	// Regression: .txt used to be skipped as "unsupported class".
	note := buildFind(t, b.top, "ReplicatedStorage", "shared", "Note")
	if v, ok := buildProp(note, "Value"); note.Class != "StringValue" || !ok || v.Value != "hello <world>" || v.Type != "string" {
		t.Errorf("Note = %+v", note)
	}
	if note.FilePath != "shared/Note.txt" {
		t.Errorf("Note FilePath = %q", note.FilePath)
	}

	for _, c := range buildFind(t, b.top, "ReplicatedStorage", "shared").Children {
		if c.Name == "Bin" || c.Name == "Skip" {
			t.Errorf("%s should have been skipped", c.Name)
		}
	}

	tree := buildFind(t, b.top, "ServerStorage", "Tree")
	if tree.Class != "Model" || tree.FilePath != "prefab/Trees.rbxmx" || !strings.HasPrefix(tree.RawXML, `<Item class="Model" referent="RBXP1">`) {
		t.Errorf("prefab = %+v", tree)
	}
	// Unnamed prefab items fall back to the file stem.
	if rock := buildFind(t, b.top, "ServerStorage", "Nested", "Rock"); rock.Class != "Part" || rock.RawXML == "" {
		t.Errorf("rock = %+v", rock)
	}
	if b.sharedStrings["abc"] != "ZGF0YQ==" {
		t.Errorf("shared strings = %v", b.sharedStrings)
	}

	root2 := buildParse(t, b.build())
	if len(root2.Shared) != 1 {
		t.Errorf("shared strings in output = %+v", root2.Shared)
	}
}

func TestSyncDirPackagesNotParsed(t *testing.T) {
	root, b := buildProject(t)
	const raw = "local x = use(1)\nreturn x"
	buildWriteFile(t, packagesBase(), "scope_pkg/1.0.0/src/Mod.luau", raw)
	buildWriteFile(t, packagesBase(), "scope_pkg/1.0.0/src/conf.json", `[1]`)
	buildWriteFile(t, root, "shared/App.luau", raw)
	b.cfg.Packages = []GlobalPackageMapping{{PackageName: "scope_pkg/1.0.0/src", Target: "ReplicatedStorage.packages.pkg"}}
	b.populatePlace()

	mod := buildFind(t, b.top, "ReplicatedStorage", "packages", "pkg", "Mod")
	if src := buildSource(t, mod); src != raw {
		t.Errorf("package source transformed: %q", src)
	}
	if !filepath.IsAbs(mod.FilePath) {
		t.Errorf("package FilePath should stay absolute, got %q", mod.FilePath)
	}
	if src := buildSource(t, buildFind(t, b.top, "ReplicatedStorage", "packages", "pkg", "conf")); src != "return {1}" {
		t.Errorf("package json = %q", src)
	}
	if src := buildSource(t, buildFind(t, b.top, "ReplicatedStorage", "shared", "App")); !strings.Contains(src, "function use(") {
		t.Errorf("project script not parsed: %q", src)
	}
}

func TestSyncDirNamesOnly(t *testing.T) {
	root, b := buildProject(t)
	b.namesOnly = true
	buildWriteFile(t, root, "shared/A.luau", "return 1")
	buildWriteFile(t, root, "shared/N.txt", "x")
	buildWriteFile(t, root, "prefab/Trees.rbxmx", buildPrefab)
	b.populatePlace()

	a := buildFind(t, b.top, "ReplicatedStorage", "shared", "A")
	if _, ok := buildProp(a, "Source"); ok || a.FilePath != "shared/A.luau" || a.Class != "ModuleScript" {
		t.Errorf("A = %+v", a)
	}
	if n := buildFind(t, b.top, "ReplicatedStorage", "shared", "N"); n.Class != "StringValue" || len(n.Props) != 0 {
		t.Errorf("N = %+v", n)
	}
	if tree := buildFind(t, b.top, "ServerStorage", "Tree"); tree.RawXML != "" || tree.Class != "Model" {
		t.Errorf("tree = %+v", tree)
	}
	if len(b.sharedStrings) != 0 {
		t.Errorf("shared strings collected in namesOnly mode: %v", b.sharedStrings)
	}
}

// Regression: unreadable files used to be skipped silently with a successful build.
func TestSyncDirReadFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits not enforced")
	}
	root, b := buildProject(t)
	p := buildWriteFile(t, root, "shared/Locked.luau", "return 1")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	b.populatePlace()
	if b.readErrs != 1 {
		t.Errorf("readErrs = %d, want 1", b.readErrs)
	}

	buildWriteFile(t, root, "chain.toml", "")
	t.Chdir(root)
	out := filepath.Join(t.TempDir(), "out.rbxlx")
	if err := handleBuild(out); err == nil {
		t.Error("handleBuild succeeded with an unreadable file")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("output written despite read errors")
	}
}

// --- syncModelDir ---

func TestSyncModelDir(t *testing.T) {
	buildHermeticHome(t)
	dir := t.TempDir()
	buildWriteFile(t, dir, "Svc/init.server.luau", "print('svc')")
	buildWriteFile(t, dir, "Svc/Helper.luau", "return 1")
	buildWriteFile(t, dir, "Folder/Deep/Thing.client.lua", "x")
	buildWriteFile(t, dir, "Folder/cfg.json", `{"a": 1}`)
	buildWriteFile(t, dir, "Folder/bad.json", `nope`)
	buildWriteFile(t, dir, "Folder/Text.txt", "words")
	buildWriteFile(t, dir, "Folder/Tree.rbxmx", buildPrefab)
	buildWriteFile(t, dir, "Folder/Bin.rbxm", "x")
	buildWriteFile(t, dir, "Folder/img.png", "x")

	b := NewBuilder(defaultConfig(), dir)
	rootInst := &Instance{Class: "Folder", Name: "Model"}
	b.top = append(b.top, rootInst)
	b.syncModelDir(dir, rootInst)

	svc := buildFind(t, b.top, "Model", "Svc")
	if svc.Class != "Script" || buildSource(t, svc) != "print('svc')" || svc.FilePath != "Svc/init.server.luau" {
		t.Errorf("Svc = %+v", svc)
	}
	if len(svc.Children) != 1 || svc.Children[0].Name != "Helper" {
		t.Errorf("Svc children = %+v", svc.Children)
	}
	if f := buildFind(t, b.top, "Model", "Folder", "Deep"); f.Class != "Folder" {
		t.Errorf("Deep class = %q", f.Class)
	}
	if th := buildFind(t, b.top, "Model", "Folder", "Deep", "Thing"); th.Class != "LocalScript" {
		t.Errorf("Thing class = %q", th.Class)
	}
	if src := buildSource(t, buildFind(t, b.top, "Model", "Folder", "cfg")); !strings.HasPrefix(src, "return {") {
		t.Errorf("cfg = %q", src)
	}
	if src := buildSource(t, buildFind(t, b.top, "Model", "Folder", "bad")); src != "return nope" {
		t.Errorf("bad = %q", src)
	}
	if txt := buildFind(t, b.top, "Model", "Folder", "Text"); txt.Class != "StringValue" {
		t.Errorf("Text class = %q", txt.Class)
	}
	if tree := buildFind(t, b.top, "Model", "Folder", "Tree"); tree.RawXML == "" {
		t.Error("prefab not embedded")
	}
	for _, c := range buildFind(t, b.top, "Model", "Folder").Children {
		if c.Name == "Bin" || c.Name == "img" || c.Name == "Bin.rbxm" || c.Name == "img.png" {
			t.Errorf("unsupported file %q not skipped", c.Name)
		}
	}
	buildParse(t, b.build())
}

func TestDuplicatePrefabReferentsStillBuild(t *testing.T) {
	root, b := buildProject(t)
	buildWriteFile(t, root, "prefab/A/Tree.rbxmx", buildPrefab)
	buildWriteFile(t, root, "prefab/B/Tree.rbxmx", buildPrefab)
	b.populatePlace()
	if len(b.prefabRefs) != 2 {
		t.Errorf("prefabRefs = %v", b.prefabRefs)
	}
	buildFind(t, b.top, "ServerStorage", "A", "Tree")
	buildFind(t, b.top, "ServerStorage", "B", "Tree")
}

// --- handleBuild ---

func TestHandleBuild(t *testing.T) {
	buildHermeticHome(t)
	root := t.TempDir()
	buildWriteFile(t, root, "chain.toml", "DisableBootstrap = false\n")
	buildWriteFile(t, root, "shared/Util.luau", "return 1")
	buildWriteFile(t, root, "server/Game.server.luau", "print(1)")
	buildWriteFile(t, root, "src/Lib.luau", "return {}")
	t.Chdir(root)
	out := t.TempDir()

	place := filepath.Join(out, "game.rbxlx")
	if err := handleBuild(place); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(place)
	if err != nil {
		t.Fatal(err)
	}
	parsed := buildParse(t, data)
	var services []string
	for _, it := range parsed.Items {
		services = append(services, it.Class)
	}
	if got := strings.Join(services, ","); got != "Workspace,ReplicatedStorage,ServerScriptService,ServerStorage,StarterPlayer" {
		t.Errorf("services = %s", got)
	}
	if rs := parsed.Items[1].child("shared"); rs == nil || rs.child("Util") == nil || rs.child("Util").Class != "ModuleScript" {
		t.Errorf("shared/Util missing: %+v", rs)
	}
	if g := parsed.Items[2].child("init"); g == nil || g.child("Game") == nil || g.child("Game").Class != "Script" {
		t.Error("server/Game missing")
	}

	model := filepath.Join(out, "MyLib.rbxmx")
	if err := handleBuild(model); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(model)
	if err != nil {
		t.Fatal(err)
	}
	parsed = buildParse(t, data)
	if len(parsed.Items) != 1 || parsed.Items[0].Class != "Folder" || parsed.Items[0].name() != "MyLib" {
		t.Fatalf("model top = %+v", parsed.Items)
	}
	if parsed.Items[0].child("Lib") == nil {
		t.Error("model missing Lib")
	}

	for _, bad := range []string{"x.rbxl", "x.rbxm", "x.foo"} {
		if err := handleBuild(filepath.Join(out, bad)); err == nil {
			t.Errorf("handleBuild(%q) succeeded", bad)
		}
	}
}

func TestHandleBuildModelWithoutSrc(t *testing.T) {
	buildHermeticHome(t)
	t.Chdir(t.TempDir())
	if err := handleBuild(filepath.Join(t.TempDir(), "m.rbxmx")); err == nil {
		t.Error("expected error without src/")
	}
}

// --- benchmarks ---

// buildBenchProject lays out a project with many scripts and one large prefab.
func buildBenchProject(tb testing.TB) string {
	tb.Helper()
	root := tb.TempDir()
	buildWriteFile(tb, root, "chain.toml", "DisableBootstrap = false\n")
	body := strings.Repeat("local x = 1\n\nprint(x)\n", 40)
	for i := 0; i < 50; i++ {
		for j := 0; j < 10; j++ {
			buildWriteFile(tb, root, fmt.Sprintf("shared/mod%d/File%d.luau", i, j), "local v = use(\"x\")\n"+body)
		}
		buildWriteFile(tb, root, fmt.Sprintf("server/S%dService.luau", i), body)
		buildWriteFile(tb, root, fmt.Sprintf("shared/data%d.json", i), `{"a":1,"b":[1,2,3],"c":{"d":"e"}}`)
	}
	buildWriteFile(tb, root, "prefab/Big.rbxmx", buildRbxmx(3000))
	return root
}

func BenchmarkBuilderBuild(b *testing.B) {
	bl := NewBuilder(defaultConfig(), "")
	src := strings.Repeat("print(\"hello <world> & ]]> friends\")\n", 60)
	for i := 0; i < 2000; i++ {
		inst := bl.place("ReplicatedStorage.shared", []string{fmt.Sprintf("dir%d", i/20), fmt.Sprintf("Mod%d", i)}, "ModuleScript")
		setSource(inst, src)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = bl.build()
	}
}

// BenchmarkExtractRbxmx measures the old two-pass prefab import (both wrappers).
func BenchmarkExtractRbxmx(b *testing.B) {
	data := []byte(buildRbxmx(3000))
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		shared := map[string]string{}
		if err := extractSharedStrings(data, shared); err != nil {
			b.Fatal(err)
		}
		if _, err := extractItems(data); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScanModel measures the single-pass prefab import used by the builder.
func BenchmarkScanModel(b *testing.B) {
	data := []byte(buildRbxmx(3000))
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := scanModel(data, map[string]string{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseScript(b *testing.B) {
	cfg := defaultConfig()
	src := "local a = use(\"x\")\nlocal p = new(\"Part\")\n" + strings.Repeat("local x = 1\n\nprint(x)\n", 200)
	b.ReportAllocs()
	for b.Loop() {
		_ = parseScript(cfg, src, "shared/FooService.luau")
	}
}

func BenchmarkHandleBuild(b *testing.B) {
	root := buildBenchProject(b)
	buildHermeticHome(b)
	b.Chdir(root)
	out := filepath.Join(b.TempDir(), "out.rbxlx")
	b.ReportAllocs()
	for b.Loop() {
		if err := handleBuild(out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHandleSourcemap(b *testing.B) {
	root := buildBenchProject(b)
	buildHermeticHome(b)
	b.Chdir(root)
	out := filepath.Join(b.TempDir(), "sourcemap.json")
	b.ReportAllocs()
	for b.Loop() {
		if err := handleSourcemap(out); err != nil {
			b.Fatal(err)
		}
	}
}
