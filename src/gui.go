package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

// `chainlink ui` serves a dashboard on 127.0.0.1 and opens it in the browser.
// The page is static; it reads everything from /api/status and runs the
// commands in guiActions through /api/jobs, all of which need the per-run
// token the CLI puts in the URL fragment. Requests whose Host
// isn't loopback are refused (DNS rebinding), so other sites can neither load
// the page nor call the API.

//go:embed gui
var guiFiles embed.FS

const guiTokenHeader = "X-ChainLink-UI-Token"

// guiStatus is the JSON /api/status returns.
type guiStatus struct {
	Version string       `json:"version"`
	Latest  string       `json:"latest,omitempty"` // newer release, when one is known
	Dir     string       `json:"dir"`
	Setup   guiSetup     `json:"setup"`
	Project *guiProject  `json:"project,omitempty"` // nil without a chain.toml
	Error   string       `json:"error,omitempty"`   // chain.toml couldn't be read
	Jobs    []guiJobView `json:"jobs"`              // commands run from the page, newest first
}

// guiSetup is the machine-wide install: what self-install and login-cloud do.
type guiSetup struct {
	BinDir          string `json:"binDir"`
	BinInstalled    bool   `json:"binInstalled"`
	OnPath          bool   `json:"onPath"`
	PathPending     bool   `json:"pathPending"` // in the rc file, not in this shell yet
	PluginPath      string `json:"pluginPath"`
	PluginInstalled bool   `json:"pluginInstalled"`
	CloudKey        bool   `json:"cloudKey"`
}

type guiProject struct {
	Name        string       `json:"name"`
	Version     string       `json:"version"`
	Description string       `json:"description,omitempty"`
	Tools       []guiDep     `json:"tools"`
	Packages    []guiDep     `json:"packages"`
	Sync        guiSync      `json:"sync"`
	Roblox      RobloxConfig `json:"roblox"`
	Assets      *guiAssets   `json:"assets,omitempty"` // nil without assets/
}

type guiDep struct {
	Alias     string `json:"alias"`
	Spec      string `json:"spec"`
	Installed bool   `json:"installed"`
	Error     string `json:"error,omitempty"`
}

type guiSync struct {
	Port    int  `json:"port"`
	Running bool `json:"running"`
}

type guiAssets struct {
	Total    int `json:"total"`
	Uploaded int `json:"uploaded"` // in assets.lock.json with the same hash
	Pending  int `json:"pending"`  // new or changed since the last upload
	Skipped  int `json:"skipped"`  // not a type Roblox accepts
}

type guiServer struct {
	token string
	home  string
	goos  string

	// health probes `chainlink serve`; replaced in tests.
	health func(port int, token string) bool

	jobs *guiJobs

	mu     sync.Mutex
	hashes map[string]guiHash // asset path -> hash, reused while size and mtime match
}

type guiHash struct {
	size int64
	mod  time.Time
	hash string
}

func newGUIServer(home string) (*guiServer, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &guiServer{
		token:  hex.EncodeToString(b),
		home:   home,
		goos:   runtime.GOOS,
		health: probeServe,
		jobs:   newGUIJobs(),
		hashes: map[string]guiHash{},
	}, nil
}

func (g *guiServer) handler() http.Handler {
	static, _ := fs.Sub(guiFiles, "gui")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/status", g.requireToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, g.status())
	}))
	mux.HandleFunc("POST /api/jobs", g.requireToken(g.handleStartJob))
	mux.HandleFunc("GET /api/jobs/{id}", g.requireToken(g.withJob(func(w http.ResponseWriter, r *http.Request, j *guiJob) {
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		writeJSON(w, j.view(from, true))
	})))
	mux.HandleFunc("POST /api/jobs/{id}/stop", g.requireToken(g.withJob(func(w http.ResponseWriter, r *http.Request, j *guiJob) {
		j.stop()
		writeJSON(w, j.view(0, false))
	})))
	return guardLoopback(mux)
}

// guardLoopback refuses non-loopback Host headers and keeps the page out of
// frames and away from anything but its own scripts.
func guardLoopback(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		// Browsers send Origin on cross-site POSTs; refuse anything not from here.
		if o := r.Header.Get("Origin"); o != "" && r.Method != http.MethodGet {
			if u, err := url.Parse(o); err != nil || !isLoopbackHost(u.Host) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// Embedded files have no modtime, so make browsers revalidate; a
		// cached app.js would outlive a chainlink update.
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func (g *guiServer) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(guiTokenHeader)), []byte(g.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (g *guiServer) handleStartJob(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "expected JSON", http.StatusUnsupportedMediaType)
		return
	}
	var req struct {
		Action string `json:"action"`
		Input  string `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Action == "serve" {
		sc := loadConfig()
		if g.health(sc.Port, sc.Token) {
			http.Error(w, fmt.Sprintf("chainlink serve is already running on port %d", sc.Port), http.StatusConflict)
			return
		}
	}
	j, err := g.jobs.start(req.Action, req.Input)
	switch {
	case errors.Is(err, errGUIUnknownAction):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case err != nil:
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, j.view(0, false))
	}
}

func (g *guiServer) withJob(next func(http.ResponseWriter, *http.Request, *guiJob)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		j := g.jobs.find(id)
		if err != nil || j == nil {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		next(w, r, j)
	}
}

// status reads chain.toml and the install state fresh on every call, so the
// page reflects changes made from the terminal.
func (g *guiServer) status() guiStatus {
	st := guiStatus{Version: chainlinkVersion, Setup: g.setup(), Jobs: g.jobs.views()}
	if latest := readUpdateCache(g.home).Latest; versionNewer(latest, chainlinkVersion) {
		st.Latest = latest
	}
	st.Dir, _ = os.Getwd()

	var cfg ChainLinkConfig
	if _, err := toml.DecodeFile(chainTomlName, &cfg); errors.Is(err, fs.ErrNotExist) {
		return st
	} else if err != nil {
		st.Error = fmt.Sprintf("could not read %s: %v", chainTomlName, err)
		return st
	}
	p := &guiProject{
		Name:        cfg.Package.Name,
		Version:     cfg.Package.Version,
		Description: cfg.Package.Description,
		Tools:       []guiDep{},
		Packages:    []guiDep{},
	}
	for _, alias := range slices.Sorted(maps.Keys(cfg.Tools)) {
		d := guiDep{Alias: alias, Spec: cfg.Tools[alias]}
		if s, err := parseToolSpec(d.Spec); err != nil {
			d.Error = err.Error()
		} else {
			_, d.Installed = installedToolDir(g.home, s, alias)
		}
		p.Tools = append(p.Tools, d)
	}
	for _, alias := range slices.Sorted(maps.Keys(cfg.Packages)) {
		d := guiDep{Alias: alias, Spec: cfg.Packages[alias]}
		if scope, name, version, err := parsePackageSpec(d.Spec); err != nil {
			d.Error = err.Error()
		} else {
			d.Installed = dirExists(packageInstallDir(g.home, scope, name, version))
		}
		p.Packages = append(p.Packages, d)
	}

	sc := loadConfig()
	p.Sync = guiSync{Port: sc.Port, Running: g.health(sc.Port, sc.Token)}

	if rc, err := loadRobloxConfig(); err == nil {
		p.Roblox = rc
	}
	if dirExists(assetsDir) {
		p.Assets = g.assets()
	}
	st.Project = p
	return st
}

func (g *guiServer) setup() guiSetup {
	s := guiSetup{BinDir: filepath.Join(g.home, ".chainlink", "bin")}
	s.BinInstalled = fileExists(filepath.Join(s.BinDir, chainlinkBinaryName(g.goos)))
	s.OnPath = pathListContains(os.Getenv("PATH"), s.BinDir)
	if !s.OnPath {
		if rc, err := shellConfigFor(g.goos, os.Getenv("SHELL"), g.home); err == nil && rc != "" {
			if data, err := os.ReadFile(rc); err == nil && mentionsDir(string(data), s.BinDir) {
				s.PathPending = true
			}
		}
	}
	if dir, err := pluginsDirFor(g.goos, g.home); err == nil {
		s.PluginPath = filepath.Join(dir, "ChainLink.rbxmx")
		s.PluginInstalled = fileExists(s.PluginPath)
	}
	if strings.TrimSpace(os.Getenv("ROBLOX_API_KEY")) != "" {
		s.CloudKey = true
	} else if _, err := getToken(robloxAPI); err == nil {
		s.CloudKey = true
	}
	return s
}

// assets compares assets/ with assets.lock.json the way `chainlink assets`
// does, without printing warnings or rehashing files that haven't changed.
func (g *guiServer) assets() *guiAssets {
	a := &guiAssets{}
	found, err := walkAssets(assetsDir, func(string) { a.Skipped++ }, g.hashCached)
	if err != nil {
		return a
	}
	lock, _ := readAssetLock(assetLockName)
	for _, f := range found {
		a.Total++
		if l, ok := lock[f.Rel]; ok && l.Hash == f.Hash {
			a.Uploaded++
		} else {
			a.Pending++
		}
	}
	return a
}

func (g *guiServer) hashCached(p string, d fs.DirEntry) (string, error) {
	info, err := d.Info()
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	c, ok := g.hashes[p]
	g.mu.Unlock()
	if ok && c.size == info.Size() && c.mod.Equal(info.ModTime()) {
		return c.hash, nil
	}
	h, err := hashFile(p)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.hashes[p] = guiHash{info.Size(), info.ModTime(), h}
	g.mu.Unlock()
	return h, nil
}

// probeServe reports whether `chainlink serve` answers on port.
func probeServe(port int, token string) bool {
	req, err := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(port)+"/health", nil)
	if err != nil {
		return false
	}
	if token != "" {
		req.Header.Set("X-ChainLink-Token", token)
	}
	resp, err := (&http.Client{Timeout: 500 * time.Millisecond}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// openBrowserCommand returns the command that opens url in the default browser.
func openBrowserCommand(goos, url string) *exec.Cmd {
	switch goos {
	case "darwin":
		return exec.Command("open", url)
	case "windows":
		// start would treat & and # specially; the URL handler takes it as is.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return exec.Command("xdg-open", url)
	}
}

func handleUI(args []string) error {
	fset := flag.NewFlagSet("ui", flag.ContinueOnError)
	port := fset.Int("port", 0, "port to listen on (default: any free port)")
	noOpen := fset.Bool("no-open", false, "print the URL without opening a browser")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fset.Arg(0))
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	g, err := newGUIServer(home)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return err
	}
	// The token rides in the fragment, which browsers never send to the server.
	link := fmt.Sprintf("http://%s/#token=%s", ln.Addr(), g.token)

	status("Serving", sgrGreen, "ChainLink UI on %s", cyan("http://"+ln.Addr().String()))
	if *noOpen {
		hintf("Open %s", link)
	} else if err := openBrowserCommand(runtime.GOOS, link).Start(); err != nil {
		warnf("could not open a browser: %v", err)
		hintf("Open %s", link)
	}
	hintf("Press Ctrl+C to stop.")

	srv := &http.Server{Handler: g.handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	// Stop anything started from the page (serve, a download) before exiting.
	g.jobs.stopAll(guiStopTimeout + time.Second)
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return nil
}
