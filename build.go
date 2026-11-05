package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// --- Tree model ---

type Property struct {
	Name, Type, Value string
}

type Instance struct {
	Referent string
	Class    string
	Name     string
	Props    []Property
	Children []*Instance
	RawXML   string // pre-serialized <Item>…</Item> subtree (from .rbxmx prefabs)
	FilePath string // Source file path, relative to the project root when inside it
}

type Builder struct {
	cfg           ProjectConfig
	root          string
	byPath        map[string]*Instance
	top           []*Instance
	nextRef       int
	sharedStrings map[string]string // md5 -> value
	prefabRefs    map[string]string // referent -> .rbxmx file that first used it

	// namesOnly builds just the instance names/classes/file paths (for the
	// sourcemap): script contents are never read or transformed.
	namesOnly bool
	readErrs  int // files that could not be read
}

// injectedGlobal is a helper function prepended to scripts that assign from
// a call to Name (e.g. `local x = use(...)`).
type injectedGlobal struct {
	Name string
	Body string
}

// InjectGlobals is ordered so the generated prelude is deterministic.
var InjectGlobals = []injectedGlobal{
	{"use", `function use(m)
	if type(m) == "string" and m:sub(1,1) ~= "." and m:sub(1,1) ~= "@" then
		if m:sub(1,1) == "/" then
			local curr = game.ReplicatedStorage
			for _, child in pairs(m:sub(2,#m):split("/")) do
				curr = curr:WaitForChild(child)
			end
			m = curr
		elseif string.lower(m) == m then
			m = game.ReplicatedStorage:WaitForChild("packages"):WaitForChild(m)
		else
			return game:GetService(m)
		end
	end
	return require(m)
end
`},
	{"new", "function new(c) return Instance.new(c) end\n"},
}

func NewBuilder(cfg ProjectConfig, root string) *Builder {
	return &Builder{
		cfg:           cfg,
		root:          root,
		byPath:        map[string]*Instance{},
		sharedStrings: map[string]string{},
		prefabRefs:    map[string]string{},
		nextRef:       1,
	}
}

func (b *Builder) newReferent() string {
	r := fmt.Sprintf("RBX%032d", b.nextRef)
	b.nextRef++
	return r
}

func (b *Builder) getOrCreate(path, class, name string, parent *Instance) *Instance {
	if inst, ok := b.byPath[path]; ok {
		return inst
	}
	inst := &Instance{Referent: b.newReferent(), Class: class, Name: name}
	b.byPath[path] = inst
	if parent == nil {
		b.top = append(b.top, inst)
	} else {
		parent.Children = append(parent.Children, inst)
	}
	return inst
}

func (b *Builder) service(name string) *Instance {
	return b.getOrCreate(name, name, name, nil)
}

func (b *Builder) resolveTarget(target string) *Instance {
	parts := strings.Split(target, ".")
	cur := b.service(parts[0])
	for i := 1; i < len(parts); i++ {
		path := strings.Join(parts[:i+1], ".")
		class := "Folder"
		switch path {
		case "StarterPlayer.StarterPlayerScripts":
			class = "StarterPlayerScripts"
		case "StarterPlayer.StarterCharacterScripts":
			class = "StarterCharacterScripts"
		}
		cur = b.getOrCreate(path, class, parts[i], cur)
	}
	return cur
}

func (b *Builder) place(target string, instancePath []string, leafClass string) *Instance {
	cur := b.resolveTarget(target)
	last := len(instancePath) - 1
	for i, seg := range instancePath {
		fullPath := target + "." + strings.Join(instancePath[:i+1], ".")
		class := "Folder"
		if i == last && leafClass != "" {
			class = leafClass
		}
		cur = b.getOrCreate(fullPath, class, seg, cur)
		// Walk visits a directory before its files (and "Foo" before
		// "Foo.server.luau"), so the instance may already exist as a plain
		// Folder; an init/sibling script then gives it its real class.
		if i == last && leafClass != "" && cur.Class == "Folder" {
			cur.Class = leafClass
		}
	}
	return cur
}

func setSource(inst *Instance, src string) {
	if !isScriptClass(inst.Class) {
		inst.Class = "ModuleScript"
	}
	setProp(inst, Property{Name: "Source", Type: "ProtectedString", Value: src})
}

// setProp sets p on inst, replacing any existing property with the same name.
func setProp(inst *Instance, p Property) {
	for i := range inst.Props {
		if inst.Props[i].Name == p.Name {
			inst.Props[i] = p
			return
		}
	}
	inst.Props = append(inst.Props, p)
}

// --- Serialization ---

// sanitize drops characters that are illegal in XML 1.0 and invalid UTF-8 bytes.
func sanitize(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, isIllegalXML) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		// size==1 RuneError is an invalid byte; a literal U+FFFD is kept.
		if !(r == utf8.RuneError && size == 1) && !isIllegalXML(r) {
			sb.WriteString(s[i : i+size])
		}
		i += size
	}
	return sb.String()
}

func isIllegalXML(r rune) bool {
	switch {
	case r == 0x9 || r == 0xA || r == 0xD:
		return false
	case r >= 0x20 && r <= 0xD7FF, r >= 0xE000 && r <= 0xFFFD, r >= 0x10000 && r <= 0x10FFFF:
		return false
	}
	return true
}

// escText escapes s for XML character data (and attribute values: quotes are
// escaped too). Strings with nothing to escape are returned as-is.
func escText(s string) string {
	s = sanitize(s)
	if !strings.ContainsAny(s, "&<>\"'\t\n\r") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func escAttr(s string) string {
	return escText(s) // xml.EscapeText already escapes '"' as &#34;
}

// cdata wraps s in a CDATA section, splitting on any "]]>" so it can't terminate early.
func cdata(s string) string {
	var b bytes.Buffer
	writeCDATA(&b, s)
	return b.String()
}

func writeCDATA(w *bytes.Buffer, s string) {
	s = sanitize(s)
	w.WriteString("<![CDATA[")
	for {
		i := strings.Index(s, "]]>")
		if i < 0 {
			break
		}
		w.WriteString(s[:i])
		w.WriteString("]]]]><![CDATA[>")
		s = s[i+3:]
	}
	w.WriteString(s)
	w.WriteString("]]>")
}

const tabs = "\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t"

func indent(depth int) string {
	if depth <= len(tabs) {
		return tabs[:depth]
	}
	return strings.Repeat("\t", depth)
}

func writeItem(w *bytes.Buffer, inst *Instance, depth int) {
	if inst.RawXML != "" {
		w.WriteString(inst.RawXML)
		w.WriteByte('\n')
		return
	}
	ind := indent(depth)
	w.WriteString(ind)
	w.WriteString(`<Item class="`)
	w.WriteString(escAttr(inst.Class))
	w.WriteString(`" referent="`)
	w.WriteString(escAttr(inst.Referent))
	w.WriteString("\">\n")
	w.WriteString(ind)
	w.WriteString("\t<Properties>\n")
	w.WriteString(ind)
	w.WriteString("\t\t<string name=\"Name\">")
	w.WriteString(escText(inst.Name))
	w.WriteString("</string>\n")
	for _, p := range inst.Props {
		w.WriteString(ind)
		w.WriteString("\t\t<")
		w.WriteString(p.Type)
		w.WriteString(` name="`)
		w.WriteString(escAttr(p.Name))
		w.WriteString(`">`)
		if p.Type == "ProtectedString" {
			writeCDATA(w, p.Value)
		} else {
			w.WriteString(escText(p.Value))
		}
		w.WriteString("</")
		w.WriteString(p.Type)
		w.WriteString(">\n")
	}
	w.WriteString(ind)
	w.WriteString("\t</Properties>\n")
	for _, c := range inst.Children {
		writeItem(w, c, depth+1)
	}
	w.WriteString(ind)
	w.WriteString("</Item>\n")
}

// sizeHint roughly estimates the serialized size of the tree under inst.
func sizeHint(inst *Instance) int {
	n := 160 + len(inst.Name) + len(inst.RawXML)
	for _, p := range inst.Props {
		n += 64 + len(p.Value)
	}
	for _, c := range inst.Children {
		n += sizeHint(c)
	}
	return n
}

func (b *Builder) build() []byte {
	var w bytes.Buffer
	hint := 512
	for _, it := range b.top {
		hint += sizeHint(it)
	}
	for _, v := range b.sharedStrings {
		hint += 96 + len(v)
	}
	w.Grow(hint)

	w.WriteString(`<roblox xmlns:xmime="http://www.w3.org/2005/05/xmlmime" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:noNamespaceSchemaLocation="http://www.roblox.com/roblox.xsd" version="4">` + "\n")
	for _, it := range b.top {
		writeItem(&w, it, 1)
	}

	if len(b.sharedStrings) > 0 {
		keys := make([]string, 0, len(b.sharedStrings))
		for md5 := range b.sharedStrings {
			keys = append(keys, md5)
		}
		sort.Strings(keys) // reproducible output
		w.WriteString("\t<SharedStrings>\n")
		for _, md5 := range keys {
			w.WriteString("\t\t<SharedString md5=\"")
			w.WriteString(escAttr(md5))
			w.WriteString("\">")
			w.WriteString(escText(b.sharedStrings[md5]))
			w.WriteString("</SharedString>\n")
		}
		w.WriteString("\t</SharedStrings>\n")
	}

	w.WriteString("</roblox>\n")
	return w.Bytes()
}

// --- Prefab (.rbxmx) support ---

// modelItem is a top-level <Item> of an .rbxmx file: its class, Name
// property and byte range within the file.
type modelItem struct {
	Class, Name string
	start, end  int64
}

func xmlAttr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// scanModel tokenizes an .rbxmx file once, returning its top-level items and
// every Item referent, and storing <SharedStrings> entries into shared (if non-nil).
func scanModel(data []byte, shared map[string]string) (items []modelItem, refs []string, err error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var depth, itemDepth, topDepth int // element depth, open Items, depth of current top-level Item
	var named, inSharedStrings bool
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return items, refs, nil
			}
			return nil, nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case t.Name.Local == "Item":
				if itemDepth == 0 {
					end := dec.InputOffset()
					items = append(items, modelItem{Class: xmlAttr(t, "class"), start: end - int64(len(rawStartTag(data, end)))})
					topDepth, named = depth, false
				}
				itemDepth++
				if ref := xmlAttr(t, "referent"); ref != "" {
					refs = append(refs, ref)
				}

			case itemDepth == 1 && !named && depth == topDepth+2 && t.Name.Local == "string" && xmlAttr(t, "name") == "Name":
				// <Item><Properties><string name="Name">
				var val string
				if err := dec.DecodeElement(&val, &t); err != nil {
					return nil, nil, err
				}
				depth--
				items[len(items)-1].Name, named = val, true

			case t.Name.Local == "SharedStrings":
				inSharedStrings = true

			case inSharedStrings && t.Name.Local == "SharedString":
				var val string
				if err := dec.DecodeElement(&val, &t); err != nil {
					return nil, nil, err
				}
				depth--
				if md5 := xmlAttr(t, "md5"); md5 != "" && shared != nil {
					shared[md5] = val
				}
			}
		case xml.EndElement:
			depth--
			switch t.Name.Local {
			case "Item":
				itemDepth--
				if itemDepth == 0 {
					items[len(items)-1].end = dec.InputOffset()
				}
			case "SharedStrings":
				inSharedStrings = false
			}
		}
	}
}

// extractItems pulls the raw <Item>…</Item> subtrees out of an .rbxmx model file.
func extractItems(data []byte) ([]string, error) {
	items, _, err := scanModel(data, nil)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no <Item> elements found")
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = string(data[it.start:it.end])
	}
	return out, nil
}

// extractSharedStrings pulls the <SharedStrings> section from an .rbxmx file.
func extractSharedStrings(data []byte, shared map[string]string) error {
	_, _, err := scanModel(data, shared)
	return err
}

// rawStartTag returns the bytes of the start tag that ends at offset `end`.
func rawStartTag(data []byte, end int64) []byte {
	i := int(end) - 1
	for i >= 0 && data[i] != '<' {
		i--
	}
	if i < 0 {
		return nil
	}
	return data[i:int(end)]
}

// readModel imports an .rbxmx file as RawXML instances (one per top-level
// Item) carrying the item's name/class and the file path for the sourcemap.
func (b *Builder) readModel(path, display string) ([]*Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		b.readFailed(path, err)
		return nil, err
	}
	shared := b.sharedStrings
	if b.namesOnly {
		shared = nil
	}
	items, refs, err := scanModel(data, shared)
	if err == nil && len(items) == 0 {
		err = fmt.Errorf("no <Item> elements found")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "build: skipping %s: %v\n", display, err)
		return nil, err
	}
	if !b.namesOnly {
		b.checkReferents(path, display, refs)
	}

	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	filePath := b.sourcePath(path)
	out := make([]*Instance, len(items))
	for i, it := range items {
		inst := &Instance{Name: it.Name, Class: it.Class, FilePath: filePath}
		if inst.Name == "" {
			inst.Name = stem
		}
		if inst.Class == "" {
			inst.Class = "Model"
		}
		if !b.namesOnly {
			inst.RawXML = string(data[it.start:it.end])
		}
		out[i] = inst
	}
	return out, nil
}

// checkReferents warns when a prefab reuses referents already embedded from
// another prefab: Studio would resolve Ref properties to the wrong instance.
func (b *Builder) checkReferents(path, display string, refs []string) {
	warned := false
	for _, ref := range refs {
		prev, ok := b.prefabRefs[ref]
		if !ok {
			b.prefabRefs[ref] = path
		} else if prev != path && !warned {
			fmt.Fprintf(os.Stderr, "build: warning: %s reuses referent %s from %s; Ref properties may resolve to the wrong instance\n", display, ref, prev)
			warned = true
		}
	}
}

// --- Build command ---

// readFailed reports a file that could not be read and counts it.
func (b *Builder) readFailed(path string, err error) {
	fmt.Fprintf(os.Stderr, "build: cannot read %s: %v\n", path, err)
	b.readErrs++
}

// sourcePath returns p relative to the project root (slash-separated) when it
// lies inside it, otherwise p unchanged (e.g. global packages).
func (b *Builder) sourcePath(p string) string {
	if b.root == "" {
		return p
	}
	rel, err := filepath.Rel(b.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.ToSlash(rel)
}

func (b *Builder) syncBootstrap() {
	if b.cfg.DisableBootstrap {
		return
	}
	for _, ev := range bootstrapEvents(b.cfg) {
		if ev.Op != "CREATE" {
			continue
		}
		inst := b.place(ev.Target, ev.InstancePath, ev.ClassName)
		setSource(inst, ev.Source)
	}
}

// populatePlace builds the full place tree: services, bootstrap, roots and packages.
func (b *Builder) populatePlace() {
	for _, s := range []string{"Workspace", "ReplicatedStorage", "ServerScriptService", "ServerStorage", "StarterPlayer"} {
		b.service(s)
	}
	b.resolveTarget("StarterPlayer.StarterPlayerScripts")

	b.syncBootstrap()

	for _, r := range b.cfg.Roots {
		absDir := filepath.Join(b.root, r.Dir)
		if dirExists(absDir) {
			b.syncDir(absDir, r.Target, "")
		}
	}
	for _, p := range b.cfg.Packages {
		absDir := filepath.Join(packagesBase(), filepath.FromSlash(p.PackageName))
		if !dirExists(absDir) {
			continue
		}
		srcDir := filepath.Join(absDir, "src")
		if dirExists(srcDir) {
			b.syncDir(srcDir, p.Target, "")
		} else {
			b.syncDir(absDir, p.Target, "")
		}
	}
}

func isScriptClass(c string) bool {
	switch c {
	case "Script", "LocalScript", "ModuleScript":
		return true
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// assignsFrom reports whether source contains `= name` (whitespace allowed
// around '=') with name as a whole word, e.g. `local x = use(...)` but not `= user`.
func assignsFrom(source, name string) bool {
	for i := 0; ; {
		j := strings.Index(source[i:], name)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(name)
		if (end == len(source) || !isIdentByte(source[end])) && (j == 0 || !isIdentByte(source[j-1])) {
			k := j - 1
			for k >= 0 && (source[k] == ' ' || source[k] == '\t' || source[k] == '\n' || source[k] == '\r') {
				k--
			}
			if k >= 0 && source[k] == '=' {
				return true
			}
		}
		i = j + 1
	}
}

func parseScript(cfg ProjectConfig, source string, fullRel string) string {
	if !cfg.DisableBootstrap && (strings.Contains(fullRel, "Controller.lua") || strings.Contains(fullRel, "Service.lua")) {
		// Blank lines are kept so multi-line strings stay intact.
		source = "local mod = {}\n" + source + "\nreturn mod"
	}

	var prelude string
	for _, g := range InjectGlobals {
		if assignsFrom(source, g.Name) {
			prelude += g.Body
		}
	}

	return addLuauHeader(prelude + source)
}

// jsonSource converts a .json file to ModuleScript source.
func jsonSource(data []byte) string {
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "return " + string(data)
	}
	return "return " + jsonToLuau(parsed)
}

func (b *Builder) syncDir(absDir, target, relPrefix string) {
	b.resolveTarget(target)

	// The classify prefix depends only on absDir, so resolve it once.
	var rootDir string
	for _, r := range b.cfg.Roots {
		if filepath.Join(b.root, r.Dir) == absDir {
			rootDir = r.Dir
			break
		}
	}
	prefix := relPrefix
	isPackage := strings.Contains(absDir, filepath.Join(packagesBase(), ""))
	if rootDir != "" {
		prefix = rootDir + "/"
	} else if isPackage {
		// Package files classify under pkgPrefix + the package path relative to the packages base.
		pkgRel, _ := filepath.Rel(packagesBase(), absDir)
		prefix = pkgPrefix + filepath.ToSlash(pkgRel) + "/"
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	err := filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			b.readFailed(path, err)
			return nil
		}
		if path == absDir {
			return nil
		}
		rel, _ := filepath.Rel(absDir, path)
		fullRel := prefix + filepath.ToSlash(rel)

		c := classify(b.cfg, fullRel, info.IsDir())
		if !c.Syncable {
			return nil
		}

		if info.IsDir() {
			b.place(c.Target, c.InstancePath, "")
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		switch {
		case isScriptClass(c.ClassName) || c.ClassName == "StringValue":
			var data []byte
			if !b.namesOnly {
				if data, err = os.ReadFile(path); err != nil {
					b.readFailed(path, err)
					return nil
				}
			}
			inst := b.place(c.Target, c.InstancePath, c.ClassName)
			inst.FilePath = b.sourcePath(path)
			if b.namesOnly {
				return nil
			}
			switch {
			case c.ClassName == "StringValue":
				setProp(inst, Property{Name: "Value", Type: "string", Value: string(data)})
			case ext == ".json":
				setSource(inst, jsonSource(data))
			case isPackage:
				setSource(inst, string(data))
			default:
				setSource(inst, parseScript(b.cfg, string(data), fullRel))
			}

		case ext == ".rbxmx":
			// Import the model tree directly into the folder that would have
			// held the file: the .rbxmx defines its own root item(s).
			parent := b.resolveTarget(c.Target)
			for i := 0; i < len(c.InstancePath)-1; i++ {
				seg := c.InstancePath[i]
				p := c.Target + "." + strings.Join(c.InstancePath[:i+1], ".")
				parent = b.getOrCreate(p, "Folder", seg, parent)
			}
			items, err := b.readModel(path, rel)
			if err != nil {
				return nil
			}
			parent.Children = append(parent.Children, items...)

		case ext == ".rbxm":
			fmt.Fprintf(os.Stderr, "build: skipping %s: binary .rbxm cannot be embedded in XML .rbxlx; convert it to .rbxmx for build output\n", rel)

		default:
			fmt.Fprintf(os.Stderr, "build: skipping %s (unsupported class %q)\n", rel, c.ClassName)
		}
		return nil
	})
	if err != nil {
		b.readFailed(absDir, err)
	}
}

// scriptClassForFile maps a Rojo-style filename to a Roblox script class.
// Returns the class and the instance name (with the suffix stripped).
func scriptClassForFile(name string) (class, instName string, ok bool) {
	lower := strings.ToLower(name)
	var ext string
	switch {
	case strings.HasSuffix(lower, ".luau"):
		ext = name[len(name)-5:]
	case strings.HasSuffix(lower, ".lua"):
		ext = name[len(name)-4:]
	default:
		return "", "", false
	}
	base := name[:len(name)-len(ext)]
	switch {
	case strings.HasSuffix(strings.ToLower(base), ".server"):
		return "Script", base[:len(base)-len(".server")], true
	case strings.HasSuffix(strings.ToLower(base), ".client"):
		return "LocalScript", base[:len(base)-len(".client")], true
	default:
		return "ModuleScript", base, true
	}
}

// syncModelDir walks absDir and builds an instance tree beneath parent.
// Directories become Folders (or scripts, if they contain an init.* file),
// scripts/json become the appropriate script class, .txt files become
// StringValues, and .rbxmx files are embedded verbatim.
func (b *Builder) syncModelDir(absDir string, parent *Instance) {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		b.readFailed(absDir, err)
		return
	}

	// First pass: an init.lua / init.server.lua / init.client.lua turns the
	// parent folder itself into a script.
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		class, name, ok := scriptClassForFile(e.Name())
		if !ok || !strings.EqualFold(name, "init") {
			continue
		}
		path := filepath.Join(absDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			b.readFailed(path, err)
			continue
		}
		parent.Class = class
		setSource(parent, string(data))
		parent.FilePath = b.sourcePath(path)
	}

	for _, e := range entries {
		path := filepath.Join(absDir, e.Name())
		name := e.Name()

		if e.IsDir() {
			child := &Instance{Referent: b.newReferent(), Class: "Folder", Name: name}
			parent.Children = append(parent.Children, child)
			b.syncModelDir(path, child)
			continue
		}

		lower := strings.ToLower(name)
		switch {
		case strings.HasSuffix(lower, ".lua"), strings.HasSuffix(lower, ".luau"):
			class, instName, _ := scriptClassForFile(name)
			if strings.EqualFold(instName, "init") {
				continue // already applied to parent
			}
			data, err := os.ReadFile(path)
			if err != nil {
				b.readFailed(path, err)
				continue
			}
			child := &Instance{Referent: b.newReferent(), Class: class, Name: instName, FilePath: b.sourcePath(path)}
			setSource(child, string(data))
			parent.Children = append(parent.Children, child)

		case strings.HasSuffix(lower, ".json"), strings.HasSuffix(lower, ".txt"):
			data, err := os.ReadFile(path)
			if err != nil {
				b.readFailed(path, err)
				continue
			}
			child := &Instance{Referent: b.newReferent(), Name: strings.TrimSuffix(name, filepath.Ext(name)), FilePath: b.sourcePath(path)}
			if strings.HasSuffix(lower, ".txt") {
				child.Class = "StringValue"
				setProp(child, Property{Name: "Value", Type: "string", Value: string(data)})
			} else {
				child.Class = "ModuleScript"
				setSource(child, jsonSource(data))
			}
			parent.Children = append(parent.Children, child)

		case strings.HasSuffix(lower, ".rbxmx"):
			// Import the model items directly into the parent.
			items, err := b.readModel(path, name)
			if err != nil {
				continue
			}
			parent.Children = append(parent.Children, items...)

		case strings.HasSuffix(lower, ".rbxm"):
			fmt.Fprintf(os.Stderr, "build: skipping %s: binary .rbxm cannot be embedded in XML output; convert it to .rbxmx\n", name)

		default:
			fmt.Fprintf(os.Stderr, "build: skipping %s (unsupported file type)\n", name)
		}
	}
}

// buildModel packages the src/ directory into a single top-level Folder.
func (b *Builder) buildModel(modelName string) error {
	srcDir := filepath.Join(b.root, "src")
	if !dirExists(srcDir) {
		return fmt.Errorf("build: no src/ directory found in %s", b.root)
	}
	rootInst := &Instance{Referent: b.newReferent(), Class: "Folder", Name: modelName}
	b.top = append(b.top, rootInst)
	b.syncModelDir(srcDir, rootInst)
	return nil
}

func handleBuild(outputPath string) error {
	cfg := loadConfig()
	root, err := os.Getwd()
	if err != nil {
		return err
	}

	b := NewBuilder(cfg, root)

	switch strings.ToLower(filepath.Ext(outputPath)) {
	case ".rbxmx":
		// Model build: package src/ as a standalone model, no services/bootstrap.
		name := strings.TrimSuffix(filepath.Base(outputPath), filepath.Ext(outputPath))
		if err := b.buildModel(name); err != nil {
			return err
		}

	case ".rbxlx":
		b.populatePlace()

	case ".rbxl", ".rbxm":
		return fmt.Errorf("build: binary formats are not supported; use .rbxlx or .rbxmx")

	default:
		return fmt.Errorf("build: unknown output format %q (expected .rbxlx or .rbxmx)", filepath.Ext(outputPath))
	}

	if b.readErrs > 0 {
		return fmt.Errorf("build: %d file(s) could not be read; %s was not written", b.readErrs, outputPath)
	}
	return os.WriteFile(outputPath, b.build(), 0644)
}
