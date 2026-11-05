package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
)

// rbxDump renders a tree with Refs as instance paths, so trees from different
// decodes can be compared.
func rbxDump(roots []*Instance) string {
	paths := map[*Instance]string{}
	for _, r := range roots {
		var visit func(inst *Instance, prefix string)
		visit = func(inst *Instance, prefix string) {
			paths[inst] = prefix + inst.Name
			for _, c := range inst.Children {
				visit(c, paths[inst]+"/")
			}
		}
		visit(r, "")
	}
	var b strings.Builder
	var visit func(inst *Instance, depth int)
	visit = func(inst *Instance, depth int) {
		fmt.Fprintf(&b, "%s%s %q\n", strings.Repeat("  ", depth), inst.Class, inst.Name)
		props := append([]Property{}, inst.Props...)
		sort.Slice(props, func(i, j int) bool { return props[i].Name < props[j].Name })
		for _, p := range props {
			var v string
			switch x := p.Value.(type) {
			case Ref:
				v = "Ref(" + paths[x.Target] + ")"
			case Content:
				v = fmt.Sprintf("Content(%d %q %s)", x.Kind, x.Uri, paths[x.Object])
			default:
				v = fmt.Sprintf("%T %#v", x, x)
			}
			fmt.Fprintf(&b, "%s  .%s = %s\n", strings.Repeat("  ", depth), p.Name, v)
		}
		for _, c := range inst.Children {
			visit(c, depth+1)
		}
	}
	for _, r := range roots {
		visit(r, 0)
	}
	return b.String()
}

func rbxFind(tb testing.TB, roots []*Instance, names ...string) *Instance {
	tb.Helper()
	return buildFind(tb, roots, names...)
}

// rbxEveryType builds a tree holding one property of every value type.
func rbxEveryType() []*Instance {
	target := &Instance{Class: "Part", Name: "Target"}
	all := &Instance{Class: "Model", Name: "All", Props: []Property{
		{"Str", String("héllo <&> world")},
		{"Source", ProtectedString("print(']]>')\n")},
		{"Blob", BinaryString("\x00\x01\xffbytes")},
		{"TextureID", ContentId("rbxassetid://1")},
		{"Tex", Content{Kind: ContentUri, Uri: "rbxassetid://2"}},
		{"TextureContent", Content{}},
		{"ObjContent", Content{Kind: ContentObject, Object: target}},
		{"Shared", SharedString("shared\x00data")},
		{"Net", NetAssetRef("net")},
		{"B", Bool(true)},
		{"I", Int32(-123456)},
		{"L", Int64(-9007199254740991)},
		{"F", Float32(-0.15625)},
		{"Inf", Float32(float32(math.Inf(1)))},
		{"D", Float64(math.Pi)},
		{"E", Enum(272)},
		{"TeamColor", BrickColor(23)},
		{"Fa", Faces(0b100100)},
		{"Ax", Axes(0b101)},
		{"Caps", SecurityCapabilities(1<<40 | 3)},
		{"U", UDim{0.5, -20}},
		{"U2", UDim2{UDim{0.25, 10}, UDim{1, -5}}},
		{"V2", Vector2{1.5, -2}},
		{"V3", Vector3{1e6, -0.001, 3}},
		{"C3", Color3{0.1, 0.5, 1}},
		{"C3u8", Color3uint8{10, 20, 30}},
		{"R", Ray{Vector3{1, 2, 3}, Vector3{0, -1, 0}}},
		{"Re", Rect{Vector2{1, 2}, Vector2{3, 4}}},
		{"NR", NumberRange{1, 3}},
		{"CF", CFrame{Vector3{1, 2, 3}, [9]float32{0.5, -0.6, 0.5, 0.8, 0.4, -0.2, -0.1, 0.6, 0.7}}},
		{"OCF", OptionalCFrame{CFrame{Vector3{9, 8, 7}, identityRotation}, true}},
		{"NoCF", OptionalCFrame{}},
		{"NS", NumberSequence{{0, 1, 0.5}, {1, 2, 0}}},
		{"CS", ColorSequence{{0, Color3{1, 0, 0}, 0}, {1, Color3{0, 0, 1}, 0}}},
		{"PP", PhysicalProperties{true, 0.5, 0.3, 0.2, 1, 1, 0.75}},
		{"NoPP", PhysicalProperties{}},
		{"Id", UniqueId{Random: -0x1234567890abcde, Time: 12345, Index: 7}},
		{"Font", Font{"rbxasset://fonts/families/GothamSSm.json", 700, FontItalic, "rbxasset://fonts/x.ttf"}},
		{"V3i16", Vector3int16{-1, 2, 32767}},
		{"Primary", Ref{target}},
		{"Nil", Ref{}},
	}}
	all.Children = []*Instance{target, {Class: "Folder", Name: "Empty"}}
	return []*Instance{all}
}

func TestDecodeStudioFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/studio_fixture.rbxm")
	if err != nil {
		t.Fatal(err)
	}
	f, err := decodeRobloxFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.Meta["ExplicitAutoJoints"] != "true" || len(f.Roots) != 1 {
		t.Fatalf("meta = %v, roots = %d", f.Meta, len(f.Roots))
	}
	root := f.Roots[0]
	if root.Class != "Model" || root.Name != "Fixture" || len(root.Children) != 9 {
		t.Fatalf("root = %s %q with %d children", root.Class, root.Name, len(root.Children))
	}
	value := func(name string) Value {
		t.Helper()
		return rbxFind(t, f.Roots, "Fixture", name).prop("Value")
	}

	if got := root.prop("Tags"); got != SharedString("Alpha\x00Beta") {
		t.Errorf("Tags = %#v", got)
	}
	if got := root.prop("WorldPivotData"); got != (OptionalCFrame{CFrame{Vector3{9, 8, 7}, identityRotation}, true}) {
		t.Errorf("WorldPivotData = %#v", got)
	}
	if got := root.prop("SourceAssetId"); got != Int64(-1) {
		t.Errorf("SourceAssetId = %#v", got)
	}
	if got := root.prop("PrimaryPart"); got != (Ref{}) {
		t.Errorf("PrimaryPart = %#v", got)
	}

	basic := value("Basic").(CFrame)
	if basic.Position != (Vector3{1, 2, 3}) || basic.Rotation[5] != -1 || basic.Rotation[7] != 1 || math.Abs(float64(basic.Rotation[4])) > 1e-6 {
		t.Errorf("Basic = %#v", basic)
	}
	skew := value("Skew").(CFrame)
	want := [9]float32{0.513036847, -0.646507621, 0.564642489, 0.852064669, 0.463138223, -0.243903369, -0.103822142, 0.606243312, 0.788473248}
	for i := range want {
		if math.Abs(float64(skew.Rotation[i]-want[i])) > 1e-6 {
			t.Errorf("Skew rotation = %v, want %v", skew.Rotation, want)
			break
		}
	}
	if skew.Position != (Vector3{-5, 0, 7}) {
		t.Errorf("Skew position = %v", skew.Position)
	}
	if got := value("Int"); got != Int64(-9007199254740991) {
		t.Errorf("Int = %#v", got)
	}
	if got := value("Num"); got != Float64(-0.15625) {
		t.Errorf("Num = %#v", got)
	}
	if got := value("Str"); got != String("héllo") {
		t.Errorf("Str = %#v", got)
	}
	if got := value("Obj"); got != (Ref{rbxFind(t, f.Roots, "Fixture", "Str")}) {
		t.Errorf("Obj = %#v", got)
	}
	if got := value("C3"); got != (Color3{0.1, 0.5, 1}) {
		t.Errorf("C3 = %#v", got)
	}
	if got := value("Brick"); got != BrickColor(23) {
		t.Errorf("Brick = %#v", got)
	}
	sa := rbxFind(t, f.Roots, "Fixture", "SA")
	if got := sa.prop("EmissiveMaskContent"); got != (Content{Kind: ContentUri, Uri: "rbxassetid://4242"}) {
		t.Errorf("EmissiveMaskContent = %#v", got)
	}
	if got := sa.prop("ColorMap"); got != ContentId("") {
		t.Errorf("ColorMap = %#v, want ContentId (hidden property hint)", got)
	}
	if got := sa.prop("AttributesSerialize"); got != BinaryString("") {
		t.Errorf("AttributesSerialize = %#v", got)
	}
}

func TestBinaryRoundTrip(t *testing.T) {
	roots := rbxEveryType()
	want := rbxDump(roots)
	data := encodeBinary(&rbxFile{Roots: roots, Meta: map[string]string{"ExplicitAutoJoints": "true"}}, false)
	f, err := decodeBinary(data)
	if err != nil {
		t.Fatal(err)
	}
	// The binary format has one string type and one shared string type;
	// reflection hints and the bytes themselves pick the kind back. "Net" has
	// no NetAssetRef hint.
	got := rbxDump(f.Roots)
	want = strings.Replace(want, `.Net = main.NetAssetRef "net"`, `.Net = main.SharedString "net"`, 1)
	if got != want {
		t.Errorf("round trip mismatch:\n%s\nwant:\n%s", got, want)
	}
	if f.Meta["ExplicitAutoJoints"] != "true" {
		t.Errorf("meta = %v", f.Meta)
	}
	if !bytes.Equal(encodeBinary(&rbxFile{Roots: f.Roots, Meta: f.Meta}, false), encodeBinary(&rbxFile{Roots: f.Roots, Meta: f.Meta}, false)) {
		t.Error("binary output is not deterministic")
	}
}

func TestXMLRoundTrip(t *testing.T) {
	roots := rbxEveryType()
	want := rbxDump(roots)
	data := encodeXML(&rbxFile{Roots: roots})
	f, err := decodeXML(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if got := rbxDump(f.Roots); got != want {
		t.Errorf("round trip mismatch:\n%s\nwant:\n%s\nxml:\n%s", got, want, data)
	}
}

// Every value survives XML -> binary -> XML, the path a .rbxmx prefab takes
// into a .rbxl build.
func TestCrossFormatRoundTrip(t *testing.T) {
	roots := rbxEveryType()
	want := rbxDump(roots)
	f, err := decodeBinary(encodeBinary(&rbxFile{Roots: roots}, false))
	if err != nil {
		t.Fatal(err)
	}
	f, err = decodeXML(encodeXML(f))
	if err != nil {
		t.Fatal(err)
	}
	// NetAssetRef needs its reflection hint to survive binary; "Net" has none.
	want = strings.Replace(want, `.Net = main.NetAssetRef "net"`, `.Net = main.SharedString "net"`, 1)
	if got := rbxDump(f.Roots); got != want {
		t.Errorf("mismatch:\n%s\nwant:\n%s", got, want)
	}
}

func TestXMLDecodeStudioForms(t *testing.T) {
	doc := `<roblox version="4">
	<Meta name="ExplicitAutoJoints">true</Meta>
	<External>null</External>
	<Item class="SpawnLocation" referent="RBX1">
		<Properties>
			<string name="Name">Spawn</string>
			<int name="TeamColor">194</int>
			<int name="Other">5</int>
			<string name="Texture">rbxassetid://5</string>
			<Content name="MeshId"><null></null></Content>
			<Content name="TextureContent"><null></null></Content>
			<Content name="Legacy">rbxasset://bare</Content>
			<Color3 name="Packed">4294901760</Color3>
			<CoordinateFrame name="CFrame"><X>1</X><Y>2</Y><Z>3</Z></CoordinateFrame>
			<BinaryString name="Tags">SGVsbG8A
V29ybGQ=</BinaryString>
			<SharedString name="Data">k</SharedString>
			<Ref name="Self">RBX1</Ref>
			<Ref name="Gone">RBXmissing</Ref>
			<Mystery name="X">?</Mystery>
		</Properties>
	</Item>
	<SharedStrings><SharedString md5="k">ZGF0YQ==</SharedString></SharedStrings>
</roblox>`
	f, err := decodeXML([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	inst := f.Roots[0]
	checks := map[string]Value{
		"TeamColor":      BrickColor(194),
		"Other":          Int32(5),
		"Texture":        ContentId("rbxassetid://5"),
		"MeshId":         ContentId(""),
		"TextureContent": Content{},
		"Legacy":         ContentId("rbxasset://bare"),
		"Packed":         Color3{1, 0, 0},
		"CFrame":         CFrame{Vector3{1, 2, 3}, identityRotation},
		"Tags":           BinaryString("Hello\x00World"),
		"Data":           SharedString("data"),
		"Self":           Ref{inst},
		"Gone":           Ref{},
	}
	for name, want := range checks {
		if got := inst.prop(name); got != want {
			t.Errorf("%s = %#v, want %#v", name, got, want)
		}
	}
	if inst.prop("X") != nil || inst.Name != "Spawn" || f.Meta["ExplicitAutoJoints"] != "true" {
		t.Errorf("inst = %+v, meta %v", inst, f.Meta)
	}

	for _, bad := range []string{``, `<notroblox/>`, `<roblox><Item><Properties/></Item></roblox>`, `<roblox><Item class="A"><Properties><float name="F">abc</float></Properties></Item></roblox>`, `<roblox><Item class="A">`} {
		if _, err := decodeXML([]byte(bad)); err == nil {
			t.Errorf("decodeXML(%q) succeeded", bad)
		}
	}
}

// Studio only accepts SharedString keys that are base64 MD5 digests.
func TestXMLSharedStringKeys(t *testing.T) {
	inst := &Instance{Class: "Part", Name: "P", Props: []Property{{"Tags", SharedString("a")}, {"Other", SharedString("a")}}}
	out := string(encodeXML(&rbxFile{Roots: []*Instance{inst}}))
	const key = "DMF1ucDxtqgxw5niaXcmYQ==" // base64(md5("a"))
	if strings.Count(out, ">"+key+"<") != 2 || strings.Count(out, `md5="`+key+`"`) != 1 {
		t.Errorf("shared string keys:\n%s", out)
	}
	raw, _ := base64.StdEncoding.DecodeString(key)
	if len(raw) != 16 {
		t.Fatal("key is not a 16-byte digest")
	}
}

func TestBinaryStringKinds(t *testing.T) {
	inst := &Instance{Class: "Part", Name: "P", Props: []Property{
		{"Source", String("print(1)")},
		{"TextureID", String("rbxassetid://1")},
		{"AttributesSerialize", String("")},
		{"Odd", String("a\x00b")},
		{"Plain", BinaryString("text")},
		{"HiddenMap", String("rbxassetid://9")},
		{"Text", String("rbxassetid://9")},
	}}
	f, err := decodeBinary(encodeBinary(&rbxFile{Roots: []*Instance{inst}}, false))
	if err != nil {
		t.Fatal(err)
	}
	got := f.Roots[0]
	for name, want := range map[string]Value{
		"Source":              ProtectedString("print(1)"),
		"TextureID":           ContentId("rbxassetid://1"),
		"AttributesSerialize": BinaryString(""),
		"Odd":                 BinaryString("a\x00b"),
		"Plain":               String("text"),
		"HiddenMap":           ContentId("rbxassetid://9"), // unknown to reflection: judged by value
		"Text":                String("rbxassetid://9"),    // known string property
	} {
		if v := got.prop(name); v != want {
			t.Errorf("%s = %#v, want %#v", name, v, want)
		}
	}
}

// Instances of one class with different properties go in separate INST
// chunks, so no instance is given values it never had.
func TestBinaryGroupsByProperties(t *testing.T) {
	a := &Instance{Class: "Part", Name: "A", Props: []Property{{"Anchored", Bool(true)}}}
	b := &Instance{Class: "Part", Name: "B", Props: []Property{{"Size", Vector3{1, 2, 3}}}}
	c := &Instance{Class: "Part", Name: "C", Props: []Property{{"Anchored", Bool(false)}}}
	data := encodeBinary(&rbxFile{Roots: []*Instance{a, b, c}}, false)
	if n := bytes.Count(data, []byte("INST")); n != 2 {
		t.Errorf("%d INST chunks, want 2", n)
	}
	f, err := decodeBinary(data)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rbxDump(f.Roots), rbxDump([]*Instance{a, b, c}); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBinaryPlaceServices(t *testing.T) {
	ws := &Instance{Class: "Workspace", Name: "Workspace", Children: []*Instance{{Class: "Folder", Name: "F"}}}
	rs := &Instance{Class: "ReplicatedStorage", Name: "ReplicatedStorage"}
	place := encodeBinary(&rbxFile{Roots: []*Instance{ws, rs}}, true)
	model := encodeBinary(&rbxFile{Roots: []*Instance{ws, rs}}, false)
	f, err := decodeBinary(place)
	if err != nil {
		t.Fatal(err)
	}
	if got := rbxDump(f.Roots); got != rbxDump([]*Instance{ws, rs}) {
		t.Errorf("place tree:\n%s", got)
	}
	// Service INST chunks carry a marker byte per instance.
	if len(place) <= len(model) {
		t.Errorf("place (%d bytes) has no service markers over model (%d bytes)", len(place), len(model))
	}
}

func TestBasicRotation(t *testing.T) {
	// From rbx-dom: ID 0x03 is a 90 degree rotation about X.
	if rot, ok := basicRotation(0x03); !ok || rot != [9]float32{1, 0, 0, 0, 0, -1, 0, 1, 0} {
		t.Errorf("0x03 = %v", rot)
	}
	if rot, _ := basicRotation(0x02); rot != identityRotation {
		t.Errorf("0x02 = %v", rot)
	}
	valid := 0
	for id := 0; id < 256; id++ {
		rot, ok := basicRotation(byte(id))
		if !ok {
			continue
		}
		valid++
		r := rot
		det := r[0]*(r[4]*r[8]-r[5]*r[7]) - r[1]*(r[3]*r[8]-r[5]*r[6]) + r[2]*(r[3]*r[7]-r[4]*r[6])
		if det != 1 {
			t.Errorf("rotation %#x has determinant %v", id, det)
		}
	}
	if valid != 24 {
		t.Errorf("%d valid rotation IDs, want 24", valid)
	}
}

func TestLZ4Decompress(t *testing.T) {
	// "abcabcabcabc": 3 literals, then a 9-byte match at offset 3 (overlapping).
	src := []byte{0x35, 'a', 'b', 'c', 3, 0, 0x10, 'x'}
	got, err := lz4Decompress(src, 13)
	if err != nil || string(got) != "abcabcabcabcx" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range [][]byte{{0x35, 'a'}, {0x35, 'a', 'b', 'c', 9, 0}, {0xf0}} {
		if _, err := lz4Decompress(bad, 13); err == nil {
			t.Errorf("lz4Decompress(%v) succeeded", bad)
		}
	}
}

// Truncated or corrupted files must fail cleanly, never panic or hang.
func TestBinaryDecodeCorrupt(t *testing.T) {
	data := encodeBinary(&rbxFile{Roots: rbxEveryType()}, false)
	fixture, err := os.ReadFile("testdata/studio_fixture.rbxm")
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range [][]byte{data, fixture} {
		for n := 0; n < len(src); n += 7 {
			if _, err := decodeBinary(src[:n]); err == nil {
				t.Fatalf("decoding %d of %d bytes succeeded", n, len(src))
			}
		}
		for i := 32; i < len(src); i += 13 {
			bad := bytes.Clone(src)
			bad[i] ^= 0xa5
			decodeBinary(bad) // must not panic
		}
	}
	if _, err := decodeRobloxFile([]byte("<roblox!garbage")); err == nil {
		t.Error("bad header accepted")
	}
}

func TestModelRoot(t *testing.T) {
	if _, err := modelRoot(&rbxFile{}, "x"); err == nil {
		t.Error("empty model accepted")
	}
	one := &Instance{Class: "Part", Name: "Inner"}
	if r, _ := modelRoot(&rbxFile{Roots: []*Instance{one}}, "File"); r != one || r.Name != "File" {
		t.Errorf("single root = %+v", r)
	}
	two := []*Instance{{Class: "Part"}, {Class: "Part"}}
	if r, _ := modelRoot(&rbxFile{Roots: two}, "File"); r.Class != "Folder" || r.Name != "File" || len(r.Children) != 2 {
		t.Errorf("wrapped root = %+v", r)
	}
}

func TestRetarget(t *testing.T) {
	old, new := &Instance{Name: "old"}, &Instance{Name: "new"}
	child := &Instance{Props: []Property{{"A", Ref{old}}, {"B", Content{Kind: ContentObject, Object: old}}, {"C", Ref{}}}}
	root := &Instance{Children: []*Instance{child}, Props: []Property{{"Self", Ref{old}}}}
	retarget(root, old, new)
	if child.prop("A") != (Ref{new}) || child.prop("B").(Content).Object != new || child.prop("C") != (Ref{}) || root.prop("Self") != (Ref{new}) {
		t.Errorf("retarget left %+v / %+v", child.Props, root.Props)
	}
}

func BenchmarkEncodeBinary(b *testing.B) {
	f := &rbxFile{Roots: rbxBenchTree()}
	b.ReportAllocs()
	for b.Loop() {
		_ = encodeBinary(f, false)
	}
}

func BenchmarkDecodeBinary(b *testing.B) {
	data := encodeBinary(&rbxFile{Roots: rbxBenchTree()}, false)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeBinary(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeXML(b *testing.B) {
	data := encodeXML(&rbxFile{Roots: rbxBenchTree()})
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeXML(data); err != nil {
			b.Fatal(err)
		}
	}
}

// rbxBenchTree is a model of 3000 parts, each with an attachment.
func rbxBenchTree() []*Instance {
	root := &Instance{Class: "Model", Name: "Big"}
	for i := 0; i < 3000; i++ {
		p := &Instance{Class: "Part", Name: fmt.Sprintf("Part%d", i), Props: []Property{
			{"CFrame", CFrame{Vector3{float32(i), 0, 0}, identityRotation}},
			{"Size", Vector3{4, 1, 2}},
			{"Anchored", Bool(true)},
			{"PhysicalConfigData", SharedString(strings.Repeat("ABC", 16))},
		}}
		p.Children = []*Instance{{Class: "Attachment", Name: "Att"}}
		root.Children = append(root.Children, p)
	}
	return []*Instance{root}
}
