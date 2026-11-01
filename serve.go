package main

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
)

//go:embed bootstrap/*.luau
var bootstrapFS embed.FS

const (
	longPollTimeout = 20 * time.Second
	debounceWindow  = 75 * time.Millisecond
	reloadDebounce  = 500 * time.Millisecond
	maxQueueLen     = 10000
	configFileName  = "chainlink.json"
	chainTomlName   = "chain.toml"
	packageSrcDir   = "src"
	pkgPrefix       = "~pkg/" // namespace for package rel paths
)

// ---------- Config ----------

type RootMapping struct {
	Dir    string `json:"dir"`
	Target string `json:"target"`
}

type GlobalPackageMapping struct {
	PackageName string `json:"package"` // disk path under ~/.chainlink/packages
	Target      string `json:"target"`  // e.g. "ReplicatedStorage.packages.razorinput"
}

type ProjectConfig struct {
	Port             int                    `json:"port"`
	Token            string                 `json:"token"`
	Roots            []RootMapping          `json:"roots"`
	Packages         []GlobalPackageMapping `json:"packages"`
	DisableBootstrap bool                   `json:"-"` // from chain.toml only
}

// bootstrapScripts maps a root dir to its embedded init script.
var bootstrapScripts = map[string]struct {
	File  string
	Class string
}{
	"server": {"bootstrap/init.server.luau", "Script"},
	"client": {"bootstrap/init.client.luau", "LocalScript"},
}

type ChainToml struct {
	Packages         map[string]string `toml:"Packages"`
	DisableBootstrap bool              `toml:"DisableBootstrap"`
}

// isBootstrapped reports whether a root dir is parented under an embedded init script.
func isBootstrapped(cfg ProjectConfig, dir string) bool {
	if cfg.DisableBootstrap {
		return false
	}
	_, ok := bootstrapScripts[dir]
	return ok
}

func addLuauHeader(source string) string {
	if strings.HasPrefix(source, "--!strict") {
		return source
	}
	return "--!strict\n--!native\n--!optimize 2\n" + source
}

func packagesBase() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".chainlink", "packages")
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func isUnder(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// findPackageDir tries common on-disk layouts for "scope/name" + version.
func findPackageDir(name, version string) string {
	short := name[strings.LastIndex(name, "/")+1:]
	candidates := []string{
		strings.ReplaceAll(name, "/", "_") + "/" + version,
		name + "/" + version,
		short + "/" + version,
		strings.ReplaceAll(name, "/", "_") + "@" + version,
	}
	for _, c := range candidates {
		if dirExists(filepath.Join(packagesBase(), filepath.FromSlash(c))) {
			return c
		}
	}
	return candidates[0]
}

// withSrc returns "<p>/src" if it exists, otherwise p (copy the root).
func withSrc(p string) string {
	p = strings.Trim(filepath.ToSlash(p), "/")
	if strings.HasSuffix(p, "/"+packageSrcDir) {
		return p
	}
	if dirExists(filepath.Join(packagesBase(), filepath.FromSlash(p), packageSrcDir)) {
		return p + "/" + packageSrcDir
	}
	return p
}

// splitTarget: "ReplicatedStorage.packages.razorinput" -> ("ReplicatedStorage.packages", "razorinput")
func splitTarget(t string) (parent, name string) {
	i := strings.LastIndex(t, ".")
	if i < 0 {
		return "", t
	}
	return t[:i], t[i+1:]
}

func defaultConfig() ProjectConfig {
	return ProjectConfig{
		Port: 8013,
		Roots: []RootMapping{
			{Dir: "client", Target: "StarterPlayer.StarterPlayerScripts"},
			{Dir: "server", Target: "ServerScriptService"},
			{Dir: "shared", Target: "ReplicatedStorage.shared"},
			{Dir: "prefab", Target: "ServerStorage"},
		},
	}
}

func loadConfig() ProjectConfig {
	cfg := defaultConfig()

	if data, err := os.ReadFile(configFileName); err == nil {
		var file ProjectConfig
		if err := json.Unmarshal(data, &file); err != nil {
			log.Printf("Warning: %s is invalid: %v", configFileName, err)
		} else {
			if file.Port != 0 {
				cfg.Port = file.Port
			}
			if file.Token != "" {
				cfg.Token = file.Token
			}
			if len(file.Roots) > 0 {
				cfg.Roots = file.Roots
			}
			cfg.Packages = file.Packages
		}
	}

	var tomlCfg ChainToml
	if _, err := toml.DecodeFile(chainTomlName, &tomlCfg); err == nil {
		cfg.DisableBootstrap = tomlCfg.DisableBootstrap
		for _, spec := range tomlCfg.Packages {
			name, version := spec, "latest"
			if i := strings.LastIndex(spec, "@"); i > 0 {
				name, version = spec[:i], spec[i+1:]
			}
			short := name[strings.LastIndex(name, "/")+1:]
			cfg.Packages = append(cfg.Packages, GlobalPackageMapping{
				PackageName: findPackageDir(name, version),
				Target:      "ReplicatedStorage.packages." + short,
			})
		}
	} else if !os.IsNotExist(err) {
		log.Printf("Warning: %s could not be parsed: %v", chainTomlName, err)
	}

	// Resolve src/root and dedupe.
	seen := map[string]bool{}
	var pkgs []GlobalPackageMapping
	for _, p := range cfg.Packages {
		p.PackageName = withSrc(p.PackageName)
		if seen[p.PackageName] {
			continue
		}
		seen[p.PackageName] = true
		pkgs = append(pkgs, p)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].PackageName < pkgs[j].PackageName })
	cfg.Packages = pkgs
	return cfg
}

func pkgKey(p GlobalPackageMapping) string { return p.PackageName + "\x00" + p.Target }

// presentPackages filters to packages whose mapped folder exists on disk.
func presentPackages(cfg ProjectConfig) []GlobalPackageMapping {
	var out []GlobalPackageMapping
	for _, p := range cfg.Packages {
		if dirExists(filepath.Join(packagesBase(), filepath.FromSlash(p.PackageName))) {
			out = append(out, p)
		}
	}
	return out
}

// packageRootOf returns the package root rel path ("~pkg/...") that rel belongs to.
func packageRootOf(cfg ProjectConfig, rel string) (string, bool) {
	if !strings.HasPrefix(rel, pkgPrefix) {
		return "", false
	}
	inner := strings.TrimPrefix(rel, pkgPrefix)
	for _, p := range cfg.Packages {
		if inner == p.PackageName || strings.HasPrefix(inner, p.PackageName+"/") {
			return pkgPrefix + p.PackageName, true
		}
	}
	return "", false
}

// ---------- Events / Queue ----------

type Event struct {
	Seq          uint64   `json:"seq"`
	Op           string   `json:"op"`
	Path         string   `json:"path"`
	Target       string   `json:"target"`
	InstancePath []string `json:"instancePath"`
	ClassName    string   `json:"className,omitempty"`
	Source       string   `json:"source,omitempty"`
	ModelData    string   `json:"modelData,omitempty"` // base64-encoded model bytes
	ModelFormat  string   `json:"modelFormat,omitempty"`
	KeepChildren bool     `json:"keepChildren,omitempty"`
}

type ChangesResponse struct {
	SessionID string  `json:"sessionId"`
	Seq       uint64  `json:"seq"`
	Full      bool    `json:"full"`
	Events    []Event `json:"events"`
}

type Queue struct {
	mu        sync.Mutex
	sessionID string
	seq       uint64
	events    []Event
	notify    chan struct{}
}

func NewQueue() *Queue {
	return &Queue{
		sessionID: strconv.FormatInt(time.Now().UnixNano(), 36),
		notify:    make(chan struct{}),
	}
}

func (q *Queue) Push(evs ...Event) {
	if len(evs) == 0 {
		return
	}
	q.mu.Lock()
	for _, ev := range evs {
		q.seq++
		ev.Seq = q.seq
		q.events = append(q.events, ev)
	}
	if len(q.events) > maxQueueLen {
		q.events = q.events[len(q.events)-maxQueueLen:]
	}
	old := q.notify
	q.notify = make(chan struct{}) // fresh channel for future waiters
	q.mu.Unlock()
	close(old) // wake everyone waiting on the old one
}

func (q *Queue) Since(since uint64) (events []Event, latest uint64, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if since > q.seq {
		return nil, q.seq, false
	}
	if len(q.events) > 0 && since < q.events[0].Seq-1 {
		return nil, q.seq, false
	}
	for _, ev := range q.events {
		if ev.Seq > since {
			events = append(events, ev)
		}
	}
	return events, q.seq, true
}

func (q *Queue) Notify() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.notify
}

// ---------- Path classification ----------

type Classified struct {
	Target       string
	InstancePath []string
	ClassName    string
	IsInit       bool
	Syncable     bool
}

func mapPath(cfg ProjectConfig, rel string) (target string, base []string, rest string, bootstrapped bool, ok bool) {
	if strings.HasPrefix(rel, pkgPrefix) {
		inner := strings.TrimPrefix(rel, pkgPrefix)
		for _, p := range cfg.Packages {
			if inner == p.PackageName || strings.HasPrefix(inner, p.PackageName+"/") {
				parent, name := splitTarget(p.Target)
				return parent, []string{name}, strings.TrimPrefix(strings.TrimPrefix(inner, p.PackageName), "/"), false, true
			}
		}
		return "", nil, "", false, false
	}
	for _, r := range cfg.Roots {
		if rel == r.Dir || strings.HasPrefix(rel, r.Dir+"/") {
			rest := strings.TrimPrefix(strings.TrimPrefix(rel, r.Dir), "/")
			if isBootstrapped(cfg, r.Dir) {
				// Everything in server/ or client/ lives under the embedded init script.
				return r.Target, []string{"init"}, rest, true, true
			}
			return r.Target, nil, rest, false, true
		}
	}
	return "", nil, "", false, false
}

func classify(cfg ProjectConfig, rel string, isDir bool) Classified {
	target, base, rest, bootstrapped, ok := mapPath(cfg, rel)
	if !ok {
		return Classified{}
	}
	// Never treat a bootstrapped root dir itself as an instance (it would map to the init script).
	if bootstrapped && rest == "" {
		return Classified{}
	}
	segs := append([]string{}, base...)
	if rest != "" {
		segs = append(segs, strings.Split(rest, "/")...)
	}

	if isDir {
		return Classified{Target: target, InstancePath: segs, ClassName: "Folder", Syncable: true}
	}
	if rest == "" {
		return Classified{}
	}

	file := segs[len(segs)-1]
	parent := append([]string{}, segs[:len(segs)-1]...)
	ext := strings.ToLower(filepath.Ext(file))
	stem := strings.TrimSuffix(file, filepath.Ext(file))

	var class string
	switch ext {
	case ".luau", ".lua":
		switch {
		case strings.HasSuffix(stem, ".server"):
			class, stem = "Script", strings.TrimSuffix(stem, ".server")
		case strings.HasSuffix(stem, ".client"):
			class, stem = "LocalScript", strings.TrimSuffix(stem, ".client")
		default:
			class = "ModuleScript"
		}
	case ".json":
		class = "ModuleScript"
	case ".txt":
		class = "StringValue"
	case ".rbxmx", ".rbxm":
		class = "Model"
	default:
		return Classified{}
	}

	if strings.EqualFold(stem, "init") {
		if len(parent) == 0 {
			return Classified{}
		}
		// A root-level init in server/ or client/ would overwrite the embedded bootstrap.
		if bootstrapped && len(parent) == len(base) {
			return Classified{}
		}
		// An init file becomes its folder's instance; siblings are naturally its children.
		return Classified{Target: target, InstancePath: parent, ClassName: class, IsInit: true, Syncable: true}
	}

	return Classified{Target: target, InstancePath: append(parent, stem), ClassName: class, Syncable: true}
}

func instanceKey(target string, path []string) string {
	return target + "\x00" + strings.Join(path, "\x00")
}

// claim reports whether rel may own the instance; first owner wins.
func (s *Server) claim(ev Event) bool {
	key := instanceKey(ev.Target, ev.InstancePath)
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner, ok := s.owners[key]; ok && owner != ev.Path {
		if _, err := os.Stat(s.absPath(owner)); err == nil {
			log.Printf("Warning: %s and %s both map to %s.%s; ignoring %s",
				owner, ev.Path, ev.Target, strings.Join(ev.InstancePath, "."), ev.Path)
			return false
		}
	}
	s.owners[key] = ev.Path
	return true
}

func (s *Server) release(ev Event) bool {
	key := instanceKey(ev.Target, ev.InstancePath)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners[key] != ev.Path {
		return false // we never owned it; don't delete someone else's instance
	}
	delete(s.owners, key)
	return true
}

// ---------- Server ----------

type Server struct {
	root    string
	queue   *Queue
	watcher *fsnotify.Watcher

	cfgMu  sync.RWMutex
	cfg    ProjectConfig
	active []GlobalPackageMapping // packages currently synced (present on disk)

	reloadMu sync.Mutex // serializes reloadPackages

	mu          sync.Mutex
	known       map[string]bool
	pending     map[string]*time.Timer
	reloadTimer *time.Timer
	owners      map[string]string // instanceKey -> rel path that owns it
}

func (s *Server) conf() ProjectConfig {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) activePackages() []GlobalPackageMapping {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.active
}

func (s *Server) relPath(abs string) (string, bool) {
	base := packagesBase()
	if isUnder(base, abs) {
		rel, err := filepath.Rel(base, abs)
		if err != nil {
			return "", false
		}
		rel = filepath.ToSlash(rel)
		for _, p := range s.conf().Packages {
			if rel == p.PackageName || strings.HasPrefix(rel, p.PackageName+"/") {
				return pkgPrefix + rel, true
			}
		}
		return "", false
	}
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func (s *Server) absPath(rel string) string {
	if strings.HasPrefix(rel, pkgPrefix) {
		return filepath.Join(packagesBase(), filepath.FromSlash(strings.TrimPrefix(rel, pkgPrefix)))
	}
	return filepath.Join(s.root, filepath.FromSlash(rel))
}

func (s *Server) fileEvent(rel string, op string) (Event, bool) {
	cfg := s.conf()
	c := classify(cfg, rel, false)
	if !c.Syncable {
		if op != "DELETE" && isShadowedRootInit(cfg, rel) {
			log.Printf("Warning: %s is ignored because the embedded bootstrap is enabled (set disable_bootstrap = true in %s to use it)", rel, chainTomlName)
		}
		return Event{}, false
	}
	ev := Event{Op: op, Path: rel, Target: c.Target, InstancePath: c.InstancePath, ClassName: c.ClassName}
	if op == "DELETE" {
		ev.KeepChildren = c.IsInit
		if !c.IsInit {
			// File merged with a sibling folder of the same name: turn the
			// instance back into a Folder instead of destroying the folder's contents.
			sibling := filepath.Join(filepath.Dir(s.absPath(rel)), c.InstancePath[len(c.InstancePath)-1])
			if dirExists(sibling) {
				ev.KeepChildren = true
			}
		}
		return ev, true
	}
	data, err := os.ReadFile(s.absPath(rel))
	if err != nil {
		return Event{}, false
	}
	source := string(data)
	lower := strings.ToLower(rel)
	if strings.HasSuffix(lower, ".luau") || strings.HasSuffix(lower, ".lua") {
		source = parseScript(cfg, source, rel)
	}
	if strings.HasSuffix(lower, ".json") {
		var parsed any
		if err := json.Unmarshal(data, &parsed); err != nil {
			source = "return " + source
		} else {
			source = "return " + jsonToLuau(parsed)
		}
	}
	if strings.HasSuffix(lower, ".rbxmx") || strings.HasSuffix(lower, ".rbxm") {
		// Model bytes are not script text: base64 preserves binary .rbxm bytes
		// across the JSON/HTTP boundary without invalid-UTF8 replacement.
		ev.ModelData = base64.StdEncoding.EncodeToString(data)
		ev.ModelFormat = strings.TrimPrefix(strings.ToLower(filepath.Ext(rel)), ".")
		return ev, true
	}
	ev.Source = source
	return ev, true
}

// isShadowedRootInit reports whether rel is e.g. server/init.server.luau while bootstrap is on.
func isShadowedRootInit(cfg ProjectConfig, rel string) bool {
	dir, file := filepath.Split(filepath.FromSlash(rel))
	dir = strings.Trim(filepath.ToSlash(dir), "/")
	if !isBootstrapped(cfg, dir) {
		return false
	}
	stem := strings.TrimSuffix(file, filepath.Ext(file))
	stem = strings.TrimSuffix(strings.TrimSuffix(stem, ".server"), ".client")
	return strings.EqualFold(stem, "init")
}

// removeDir emits deletes for a removed directory. If a file still owns the
// same instance (file+folder merge), only the folder's contents are removed
// so the file's instance survives and "unmerges".
func (s *Server) removeDir(rel string, children []string, childIsDir map[string]bool) {
	cfg := s.conf()
	c := classify(cfg, rel, true)
	if !c.Syncable {
		return
	}

	s.mu.Lock()
	owner, merged := s.owners[instanceKey(c.Target, c.InstancePath)]
	s.mu.Unlock()

	if !merged {
		s.queue.Push(Event{Op: "DELETE", Path: rel, Target: c.Target, InstancePath: c.InstancePath})
		log.Printf("[DELETE] %s/", rel)
		return
	}

	sort.Strings(children)
	var evs []Event
	for _, child := range children {
		if childIsDir[child] {
			if cc := classify(cfg, child, true); cc.Syncable {
				evs = append(evs, Event{Op: "DELETE", Path: child, Target: cc.Target, InstancePath: cc.InstancePath})
			}
		} else if ev, ok := s.fileEvent(child, "DELETE"); ok {
			evs = append(evs, ev)
		}
	}
	s.queue.Push(evs...)
	log.Printf("[UNMERGE] %s/ removed from %s (%d children)", rel, owner, len(evs))
}

func jsonToLuau(v any) string {
	switch t := v.(type) {
	case map[string]any:
		var pairs []string
		for k, val := range t {
			pairs = append(pairs, fmt.Sprintf("[%q] = %s", k, jsonToLuau(val)))
		}
		return "{" + strings.Join(pairs, ", ") + "}"
	case []any:
		var items []string
		for _, val := range t {
			items = append(items, jsonToLuau(val))
		}
		return "{" + strings.Join(items, ", ") + "}"
	case string:
		return fmt.Sprintf("%q", t)
	case float64:
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	case nil:
		return "nil"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// walkFiles emits CREATE events for every syncable file under absDir.
// Reports whether an init file sits directly at the mapped root.
func (s *Server) walkFiles(absDir string, out *[]Event) (rootInit bool) {
	cfg := s.conf()
	filepath.Walk(absDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, ok := s.relPath(p)
		if !ok {
			return nil
		}
		if ev, ok := s.fileEvent(rel, "CREATE"); ok && s.claim(ev) {
			*out = append(*out, ev)
			if c := classify(cfg, rel, false); c.IsInit && len(c.InstancePath) == 1 && strings.HasPrefix(rel, pkgPrefix) {
				rootInit = true
			}
		}
		return nil
	})
	return rootInit
}

func sortByDepth(evs []Event) {
	sort.SliceStable(evs, func(i, j int) bool {
		return len(evs[i].InstancePath) < len(evs[j].InstancePath)
	})
}

// packageEvents builds an ordered CREATE batch for one package.
func (s *Server) packageEvents(p GlobalPackageMapping) []Event {
	rel := pkgPrefix + p.PackageName
	var evs []Event
	if !s.walkFiles(s.absPath(rel), &evs) {
		c := classify(s.conf(), rel, true)
		evs = append(evs, Event{Op: "CREATE", Path: rel, Target: c.Target, InstancePath: c.InstancePath, ClassName: "Folder"})
	}
	sortByDepth(evs)
	return evs
}

func (s *Server) snapshot() []Event {
	s.mu.Lock()
	s.owners = map[string]string{}
	s.mu.Unlock()

	cfg := s.conf()
	out := bootstrapEvents(cfg) // depth 1, so they sort before their children
	for _, r := range cfg.Roots {
		s.walkFiles(filepath.Join(s.root, r.Dir), &out)
	}
	for _, p := range s.activePackages() {
		out = append(out, s.packageEvents(p)...)
	}
	sortByDepth(out)
	return out
}

// bootstrapEvents returns CREATE events for the embedded init scripts of every bootstrapped root.
func bootstrapEvents(cfg ProjectConfig) []Event {
	var out []Event
	for _, r := range cfg.Roots {
		if !isBootstrapped(cfg, r.Dir) {
			continue
		}
		bs := bootstrapScripts[r.Dir]
		data, err := bootstrapFS.ReadFile(bs.File)
		if err != nil {
			log.Printf("Warning: could not read embedded bootstrap file %s: %v", bs.File, err)
			continue
		}
		out = append(out, Event{
			Op:           "CREATE",
			Path:         bs.File,
			Target:       r.Target,
			InstancePath: []string{"init"},
			ClassName:    bs.Class,
			Source:       addLuauHeader(string(data)),
		})
	}
	return out
}

func (s *Server) schedule(abs string) {
	rel, ok := s.relPath(abs)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, exists := s.pending[rel]; exists {
		t.Stop()
	}
	s.pending[rel] = time.AfterFunc(debounceWindow, func() { s.resolve(rel) })
}

func (s *Server) scheduleReload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reloadTimer != nil {
		s.reloadTimer.Stop()
	}
	s.reloadTimer = time.AfterFunc(reloadDebounce, s.reloadPackages)
}

func (s *Server) resolve(rel string) {
	s.mu.Lock()
	delete(s.pending, rel)
	wasDir, wasKnown := s.known[rel]
	s.mu.Unlock()

	// Package roots appearing/disappearing are handled by reloadPackages,
	// so we don't double-emit CREATE/DELETE for whole packages here.
	if root, isPkg := packageRootOf(s.conf(), rel); isPkg {
		if rel == root || !dirExists(s.absPath(root)) {
			return
		}
	}

	abs := s.absPath(rel)
	info, err := os.Stat(abs)

	// --- removed ---
	if err != nil {
		if !wasKnown {
			return
		}
		s.mu.Lock()
		delete(s.known, rel)
		if wasDir {
			for k := range s.known {
				if strings.HasPrefix(k, rel+"/") {
					delete(s.known, k)
				}
			}
		}
		s.mu.Unlock()

		if wasDir {
			if c := classify(s.conf(), rel, true); c.Syncable {
				s.queue.Push(Event{Op: "DELETE", Path: rel, Target: c.Target, InstancePath: c.InstancePath})
				log.Printf("[DELETE] %s/", rel)
			}
			return
		}
		if ev, ok := s.fileEvent(rel, "DELETE"); ok && s.release(ev) {
			s.queue.Push(ev)
			log.Printf("[DELETE] %s", rel)
		}
		return
	}

	// --- directory created/moved in ---
	if info.IsDir() {
		s.mu.Lock()
		s.known[rel] = true
		s.mu.Unlock()
		if wasKnown {
			return
		}
		s.watcher.Add(abs)
		filepath.Walk(abs, func(p string, fi os.FileInfo, err error) error {
			if err != nil || p == abs {
				return nil
			}
			r, ok := s.relPath(p)
			if !ok {
				return nil
			}
			if fi.IsDir() {
				s.watcher.Add(p)
				s.mu.Lock()
				s.known[r] = true
				s.mu.Unlock()
				return nil
			}
			s.resolve(r)
			return nil
		})
		return
	}

	// --- file created/updated ---
	op := "UPDATE"
	if !wasKnown {
		op = "CREATE"
	}
	ev, ok := s.fileEvent(rel, op)
	if !ok || !s.claim(ev) {
		return
	}
	s.mu.Lock()
	s.known[rel] = false
	s.mu.Unlock()
	s.queue.Push(ev)
	log.Printf("[%s] %s -> %s.%s (%s)", op, rel, ev.Target, strings.Join(ev.InstancePath, "."), ev.ClassName)
}

// forget drops known entries and watches for a package root.
func (s *Server) forget(rootRel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, isDir := range s.known {
		if k == rootRel || strings.HasPrefix(k, rootRel+"/") {
			if isDir {
				s.watcher.Remove(s.absPath(k))
			}
			delete(s.known, k)
		}
	}
	for k, t := range s.pending {
		if k == rootRel || strings.HasPrefix(k, rootRel+"/") {
			t.Stop()
			delete(s.pending, k)
		}
	}
	for k, owner := range s.owners {
		if owner == rootRel || strings.HasPrefix(owner, rootRel+"/") {
			delete(s.owners, k)
		}
	}
}

// watchPackageAncestors watches the packages base and every existing folder on
// the way to each declared package, so installs/uninstalls/src changes are seen.
func (s *Server) watchPackageAncestors(cfg ProjectConfig) {
	base := packagesBase()
	os.MkdirAll(base, 0o755)
	s.watcher.Add(base)
	for _, p := range cfg.Packages {
		cur := base
		for _, part := range strings.Split(p.PackageName, "/") {
			cur = filepath.Join(cur, part)
			if !dirExists(cur) {
				break
			}
			s.watcher.Add(cur)
		}
	}
}

// reloadPackages re-reads package config and diffs against what's synced.
func (s *Server) reloadPackages() {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	fresh := loadConfig()

	s.cfgMu.Lock()
	oldCfg := s.cfg
	oldActive := s.active
	newCfg := oldCfg // roots/port/token require a restart
	newCfg.Packages = fresh.Packages
	newCfg.DisableBootstrap = fresh.DisableBootstrap
	newActive := presentPackages(newCfg)
	s.cfg = newCfg
	s.active = newActive
	s.cfgMu.Unlock()

	s.watchPackageAncestors(newCfg)

	oldSet := map[string]bool{}
	for _, p := range oldActive {
		oldSet[pkgKey(p)] = true
	}
	newSet := map[string]bool{}
	for _, p := range newActive {
		newSet[pkgKey(p)] = true
	}

	var events []Event

	// Removals first so a version bump on the same target deletes before creating.
	for _, p := range oldActive {
		if newSet[pkgKey(p)] {
			continue
		}
		rel := pkgPrefix + p.PackageName
		c := classify(oldCfg, rel, true)
		events = append(events, Event{Op: "DELETE", Path: rel, Target: c.Target, InstancePath: c.InstancePath})
		s.forget(rel)
		log.Printf("[PKG REMOVE] %s -> %s", p.PackageName, p.Target)
	}

	for _, p := range newActive {
		if oldSet[pkgKey(p)] {
			continue
		}
		s.primeDir(s.absPath(pkgPrefix + p.PackageName))
		evs := s.packageEvents(p)
		events = append(events, evs...)
		log.Printf("[PKG ADD] %s -> %s (%d events)", p.PackageName, p.Target, len(evs))
	}

	if oldCfg.DisableBootstrap != newCfg.DisableBootstrap {
		events = append(events, s.bootstrapToggleEvents(oldCfg)...)
	}

	s.queue.Push(events...)
}

// bootstrapToggleEvents re-parents server/ and client/ after disable_bootstrap changes.
// Must be called after s.cfg has been updated.
func (s *Server) bootstrapToggleEvents(oldCfg ProjectConfig) []Event {
	newCfg := s.conf()
	var dels, creates []Event
	for _, r := range newCfg.Roots {
		if _, ok := bootstrapScripts[r.Dir]; !ok {
			continue
		}
		if isBootstrapped(oldCfg, r.Dir) {
			// Deleting the bootstrap script removes everything parented under it.
			dels = append(dels, Event{Op: "DELETE", Path: bootstrapScripts[r.Dir].File, Target: r.Target, InstancePath: []string{"init"}})
		} else if entries, err := os.ReadDir(filepath.Join(s.root, r.Dir)); err == nil {
			for _, e := range entries {
				rel := r.Dir + "/" + e.Name()
				if c := classify(oldCfg, rel, e.IsDir()); c.Syncable {
					dels = append(dels, Event{Op: "DELETE", Path: rel, Target: c.Target, InstancePath: c.InstancePath})
				}
			}
		}
		s.walkFiles(filepath.Join(s.root, r.Dir), &creates)
	}
	creates = append(creates, bootstrapEvents(newCfg)...)
	sortByDepth(creates)
	state := "enabled"
	if newCfg.DisableBootstrap {
		state = "disabled"
	}
	log.Printf("[BOOTSTRAP] %s, re-synced server/ and client/", state)
	return append(dels, creates...)
}

func (s *Server) isConfigFile(abs string) bool {
	if filepath.Dir(abs) != s.root {
		return false
	}
	b := filepath.Base(abs)
	return b == chainTomlName || b == configFileName
}

func (s *Server) watchLoop() {
	base := packagesBase()
	for {
		select {
		case event, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if s.isConfigFile(event.Name) {
				s.scheduleReload()
				continue
			}
			if isUnder(base, event.Name) {
				s.scheduleReload() // no-op if the package set didn't change
			}
			s.schedule(event.Name)
		case err, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Watcher error: %v", err)
		}
	}
}

func (s *Server) primeDir(absDir string) {
	cfg := s.conf()
	filepath.Walk(absDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, ok := s.relPath(p)
		if !ok {
			return nil
		}
		if info.IsDir() {
			s.watcher.Add(p)
			s.mu.Lock()
			s.known[rel] = true
			s.mu.Unlock()
		} else if c := classify(cfg, rel, false); c.Syncable {
			s.mu.Lock()
			s.known[rel] = false
			s.mu.Unlock()
		}
		return nil
	})
}

func (s *Server) primeKnown() {
	for _, r := range s.conf().Roots {
		s.primeDir(filepath.Join(s.root, r.Dir))
	}
	for _, p := range s.activePackages() {
		s.primeDir(s.absPath(pkgPrefix + p.PackageName))
	}
}

// ---------- HTTP ----------

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok := s.conf().Token; tok != "" && r.Header.Get("X-ChainLink-Token") != tok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "sessionId": s.queue.sessionID})
}

// handleChanges long-polls for events after ?since=<seq>.
// A missing/mismatched session or a gap in the queue returns a full snapshot.
// An empty poll blocks until an event arrives or longPollTimeout elapses.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	session := q.Get("session")

	sendFull := func() {
		// Read seq before snapshotting so nothing pushed during the walk is skipped.
		s.queue.mu.Lock()
		seq := s.queue.seq
		s.queue.mu.Unlock()
		writeJSON(w, ChangesResponse{
			SessionID: s.queue.sessionID,
			Seq:       seq,
			Full:      true,
			Events:    s.snapshot(),
		})
	}

	// Only a missing or different session means "I have nothing, give me everything".
	// since==0 with a matching session is a normal long poll on a fresh queue.
	if session != s.queue.sessionID {
		sendFull()
		return
	}

	deadline := time.NewTimer(longPollTimeout)
	defer deadline.Stop()

	for {
		// Grab the notify channel BEFORE checking the queue so a push between
		// the check and the wait can't be missed.
		notify := s.queue.Notify()

		events, latest, ok := s.queue.Since(since)
		if !ok {
			sendFull()
			return
		}
		if len(events) > 0 {
			writeJSON(w, ChangesResponse{SessionID: s.queue.sessionID, Seq: latest, Events: events})
			return
		}

		select {
		case <-notify:
			// Woken by a push; loop and re-check. If it was a spurious wake with
			// nothing new for us, we simply wait again on a fresh channel.
			continue
		case <-deadline.C:
			writeJSON(w, ChangesResponse{SessionID: s.queue.sessionID, Seq: latest, Events: []Event{}})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write error: %v", err)
	}
}

// ---------- Entry ----------

func serve() error {
	cfg := loadConfig()

	root, err := os.Getwd()
	if err != nil {
		return err
	}

	for _, r := range cfg.Roots {
		if !dirExists(filepath.Join(root, r.Dir)) {
			log.Printf("Warning: directory %s/ not found. It will be ignored until created.", r.Dir)
		}
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create watcher: %w", err)
	}
	defer watcher.Close()

	s := &Server{
		cfg:     cfg,
		active:  presentPackages(cfg),
		root:    root,
		queue:   NewQueue(),
		watcher: watcher,
		known:   map[string]bool{},
		pending: map[string]*time.Timer{},
		owners:  map[string]string{},
	}

	watcher.Add(root)            // catches chain.toml / chainlink.json edits
	s.watchPackageAncestors(cfg) // catches installs/uninstalls under ~/.chainlink/packages
	s.primeKnown()
	go s.watchLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.auth(s.handleHealth))
	mux.HandleFunc("/changes", s.auth(s.handleChanges))

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	log.Printf("Listening on http://%s  (session %s)", addr, s.queue.sessionID)
	for _, r := range cfg.Roots {
		log.Printf("  %-8s -> %s", r.Dir+"/", r.Target)
	}
	for _, p := range cfg.Packages {
		abs := s.absPath(pkgPrefix + p.PackageName)
		status := "NOT INSTALLED (will sync when it appears)"
		if dirExists(abs) {
			var tmp []Event
			if s.walkFiles(abs, &tmp) {
				status = fmt.Sprintf("ModuleScript, %d files", len(tmp))
			} else {
				status = fmt.Sprintf("Folder (no init), %d files", len(tmp))
			}
		}
		log.Printf("  pkg %s -> %s  [%s]", abs, p.Target, status)
	}
	log.Println("Watching chain.toml and ~/.chainlink/packages for package changes.")
	log.Println("Press Ctrl+C to stop.")

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}
