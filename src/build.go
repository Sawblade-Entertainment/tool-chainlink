package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// --- Tree model ---

type Builder struct {
	cfg    ProjectConfig
	root   string
	byPath map[string]*Instance
	top    []*Instance

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

var (
	//go:embed templates/globals/use.luau
	useGlobal string
	//go:embed templates/globals/new.luau
	newGlobal string
	//go:embed templates/globals/asset.luau
	assetGlobal string
)

// InjectGlobals is ordered so the generated prelude is deterministic.
var InjectGlobals = []injectedGlobal{
	{"use", useGlobal},
	{"new", newGlobal},
	{"asset", assetGlobal},
}

func NewBuilder(cfg ProjectConfig, root string) *Builder {
	return &Builder{
		cfg:    cfg,
		root:   root,
		byPath: map[string]*Instance{},
	}
}

func (b *Builder) getOrCreate(path, class, name string, parent *Instance) *Instance {
	if inst, ok := b.byPath[path]; ok {
		return inst
	}
	inst := &Instance{Class: class, Name: name}
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
	setProp(inst, "Source", ProtectedString(src))
}

// --- Model files (.rbxmx / .rbxm) ---

// readModel decodes a model file into the instance that stands for it, named
// after the file (see modelRoot).
func (b *Builder) readModel(path string) (*Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		b.readFailed(path, err)
		return nil, err
	}
	f, err := decodeRobloxFile(data)
	var root *Instance
	if err == nil {
		root, err = modelRoot(f, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	}
	if err != nil {
		warnf("skipping %s: %v", b.sourcePath(path), err)
		return nil, err
	}
	root.FilePath = b.sourcePath(path)
	if b.namesOnly {
		walk(root, func(inst *Instance) { inst.Props = nil })
	}
	return root, nil
}

// mergeModel makes inst the model's root instance. Children inst already has
// (from a folder of the same name, or an init model's siblings) are kept after
// the model's own.
func mergeModel(inst, model *Instance) {
	inst.Class = model.Class
	inst.Props = model.Props
	inst.Children = append(model.Children, inst.Children...)
	inst.FilePath = model.FilePath
	retarget(inst, model, inst)
}

// --- Build command ---

// readFailed reports a file that could not be read and counts it.
// Unreadable files in global packages only warn, so one broken package
// cannot block every project build (serve skips them the same way).
func (b *Builder) readFailed(path string, err error) {
	warnf("cannot read %s: %v", path, err)
	if !isUnder(packagesBase(), path) {
		b.readErrs++
	}
}

// sourcePath returns p relative to the project root (slash-separated) when it
// lies inside it, otherwise p unchanged (e.g. global packages).
func (b *Builder) sourcePath(p string) string {
	if b.root == "" {
		return p
	}
	if !isUnder(b.root, p) {
		return p
	}
	rel, _ := filepath.Rel(b.root, p)
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

// syncAssetLock adds the ReplicatedStorage.assets module generated from
// assets.lock.json, when `chainlink assets` has written one.
func (b *Builder) syncAssetLock() {
	path := filepath.Join(b.root, assetLockName)
	if !fileExists(path) {
		return
	}
	var source string
	if !b.namesOnly {
		data, err := os.ReadFile(path)
		if err != nil {
			b.readFailed(path, err)
			return
		}
		if source, err = assetModuleSource(data); err != nil {
			b.readFailed(path, err)
			return
		}
	}
	inst := b.place("ReplicatedStorage", []string{assetModuleName}, "ModuleScript")
	inst.FilePath = b.sourcePath(path)
	if !b.namesOnly {
		setSource(inst, source)
	}
}

// populatePlace builds the full place tree: services, bootstrap, roots and packages.
func (b *Builder) populatePlace() {
	for _, s := range []string{"Workspace", "ReplicatedStorage", "ServerScriptService", "ServerStorage", "StarterPlayer"} {
		b.service(s)
	}
	b.resolveTarget("StarterPlayer.StarterPlayerScripts")

	b.syncBootstrap()
	b.syncAssetLock()

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
	isPackage := isUnder(packagesBase(), absDir)
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
				setProp(inst, "Value", String(data))
			case ext == ".json":
				setSource(inst, jsonSource(data))
			case isPackage:
				setSource(inst, string(data))
			default:
				setSource(inst, parseScript(b.cfg, string(data), fullRel))
			}

		case ext == ".rbxmx" || ext == ".rbxm":
			if model, err := b.readModel(path); err == nil {
				mergeModel(b.place(c.Target, c.InstancePath, ""), model)
			}

		default:
			warnf("skipping %s (unsupported class %q)", rel, c.ClassName)
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
// StringValues, and .rbxmx/.rbxm files become the instances they contain
// (an init model becomes the folder itself).
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
			child := &Instance{Class: "Folder", Name: name}
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
			child := &Instance{Class: class, Name: instName, FilePath: b.sourcePath(path)}
			setSource(child, string(data))
			parent.Children = append(parent.Children, child)

		case strings.HasSuffix(lower, ".json"), strings.HasSuffix(lower, ".txt"):
			data, err := os.ReadFile(path)
			if err != nil {
				b.readFailed(path, err)
				continue
			}
			child := &Instance{Name: strings.TrimSuffix(name, filepath.Ext(name)), FilePath: b.sourcePath(path)}
			if strings.HasSuffix(lower, ".txt") {
				child.Class = "StringValue"
				setProp(child, "Value", String(data))
			} else {
				child.Class = "ModuleScript"
				setSource(child, jsonSource(data))
			}
			parent.Children = append(parent.Children, child)

		case strings.HasSuffix(lower, ".rbxmx"), strings.HasSuffix(lower, ".rbxm"):
			model, err := b.readModel(path)
			if err != nil {
				continue
			}
			if strings.EqualFold(model.Name, "init") {
				mergeModel(parent, model)
			} else {
				parent.Children = append(parent.Children, model)
			}

		default:
			warnf("skipping %s (unsupported file type)", name)
		}
	}
}

// buildModel packages the src/ directory into a single top-level Folder.
func (b *Builder) buildModel(modelName string) error {
	srcDir := filepath.Join(b.root, "src")
	if !dirExists(srcDir) {
		return fmt.Errorf("build: no src/ directory found in %s", b.root)
	}
	rootInst := &Instance{Class: "Folder", Name: modelName}
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

	ext := strings.ToLower(filepath.Ext(outputPath))
	switch ext {
	case ".rbxmx", ".rbxm":
		// Model build: package src/ as a standalone model, no services/bootstrap.
		name := strings.TrimSuffix(filepath.Base(outputPath), filepath.Ext(outputPath))
		if err := b.buildModel(name); err != nil {
			return err
		}

	case ".rbxlx", ".rbxl":
		b.populatePlace()

	default:
		return fmt.Errorf("build: unknown output format %q (expected .rbxl, .rbxlx, .rbxm or .rbxmx)", filepath.Ext(outputPath))
	}

	if b.readErrs > 0 {
		return fmt.Errorf("build: %d file(s) could not be read; %s was not written", b.readErrs, outputPath)
	}
	return os.WriteFile(outputPath, b.encode(ext), 0644)
}

// encode writes the built tree in the format of the output extension.
func (b *Builder) encode(ext string) []byte {
	f := &rbxFile{Roots: b.top}
	switch ext {
	case ".rbxl":
		return encodeBinary(f, true)
	case ".rbxm":
		return encodeBinary(f, false)
	}
	return encodeXML(f)
}
