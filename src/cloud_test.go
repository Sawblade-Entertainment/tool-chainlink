package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// cloudUpload is one multipart upload the fake Open Cloud server received.
type cloudUpload struct {
	Method      string
	Path        string
	Request     map[string]any
	Filename    string
	ContentType string
	Data        []byte
}

// cloudFake is a fake Assets API: every upload returns an unfinished
// operation that completes on its first poll with the next asset ID.
type cloudFake struct {
	t       *testing.T
	mu      sync.Mutex
	uploads []cloudUpload
	polls   int
	nextID  int
	fail    map[string]string // displayName -> operation error message
	ops     map[string]string // operation ID -> finished response JSON
}

func cloudNewFake(t *testing.T) (*cloudFake, openCloud) {
	t.Helper()
	f := &cloudFake{t: t, nextID: 1000, fail: map[string]string{}, ops: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, openCloud{client: srv.Client(), base: srv.URL, key: "test-key", sleep: func(time.Duration) {}}
}

func (f *cloudFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-api-key") != "test-key" {
		http.Error(w, `{"message":"bad key"}`, http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if op, ok := strings.CutPrefix(r.URL.Path, "/assets/v1/operations/"); ok {
		f.polls++
		resp, ok := f.ops[op]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, resp)
		return
	}

	u := cloudUpload{Method: r.Method, Path: r.URL.Path}
	if err := json.Unmarshal([]byte(r.FormValue("request")), &u.Request); err != nil {
		f.t.Errorf("request field: %v", err)
	}
	file, hdr, err := r.FormFile("fileContent")
	if err != nil {
		f.t.Errorf("fileContent: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.Filename, u.ContentType = hdr.Filename, hdr.Header.Get("Content-Type")
	u.Data, _ = io.ReadAll(file)
	f.uploads = append(f.uploads, u)

	f.nextID++
	op := fmt.Sprintf("op-%d", f.nextID)
	if msg, bad := f.fail[fmt.Sprint(u.Request["displayName"])]; bad {
		f.ops[op] = fmt.Sprintf(`{"path":"operations/%s","done":true,"error":{"code":3,"message":%q}}`, op, msg)
	} else {
		// IDs come back as strings for assets and numbers for revisions.
		f.ops[op] = fmt.Sprintf(`{"path":"operations/%s","done":true,"response":{"assetId":"%d","revisionId":%d}}`, op, f.nextID, f.nextID%100)
	}
	fmt.Fprintf(w, `{"path":"operations/%s"}`, op)
}

func TestCloudIDUnmarshal(t *testing.T) {
	for in, want := range map[string]string{`"123"`: "123", `456`: "456", `78901234567890`: "78901234567890"} {
		var id cloudID
		if err := json.Unmarshal([]byte(in), &id); err != nil || string(id) != want {
			t.Errorf("Unmarshal(%s) = %q, %v; want %q", in, id, err, want)
		}
	}
	var id cloudID
	if err := json.Unmarshal([]byte(`{}`), &id); err == nil {
		t.Error("Unmarshal({}) succeeded")
	}
}

func TestOpenCloudUploadPollsOperation(t *testing.T) {
	f, c := cloudNewFake(t)
	asset, err := c.upload("POST", "/assets/v1/assets", map[string]any{"assetType": "Audio"}, "a.ogg", "audio/ogg", []byte("OggS"))
	if err != nil {
		t.Fatal(err)
	}
	if asset.AssetID != "1001" || asset.RevisionID != "1" {
		t.Errorf("asset = %+v", asset)
	}
	if f.polls != 1 {
		t.Errorf("polls = %d, want 1", f.polls)
	}
	u := f.uploads[0]
	if u.ContentType != "audio/ogg" || u.Filename != "a.ogg" || string(u.Data) != "OggS" || u.Request["assetType"] != "Audio" {
		t.Errorf("upload = %+v", u)
	}
}

func TestOpenCloudErrors(t *testing.T) {
	f, c := cloudNewFake(t)
	f.fail["bad"] = "moderated"
	_, err := c.upload("POST", "/assets/v1/assets", map[string]any{"displayName": "bad"}, "x.png", "image/png", nil)
	if err == nil || !strings.Contains(err.Error(), "moderated") {
		t.Errorf("operation error = %v", err)
	}

	c.key = "wrong"
	_, err = c.upload("POST", "/assets/v1/assets", map[string]any{}, "x.png", "image/png", nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("HTTP error = %v", err)
	}
}

func TestOpenCloudWaitTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"path":"operations/x","done":false}`)
	}))
	defer srv.Close()
	saved := operationTimeout
	operationTimeout = -time.Second
	defer func() { operationTimeout = saved }()
	c := openCloud{client: srv.Client(), base: srv.URL, sleep: func(time.Duration) {}}
	if _, err := c.wait(cloudOperation{Path: "operations/x"}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("wait = %v", err)
	}
}

func TestRobloxAPIKey(t *testing.T) {
	cliSetHome(t)
	t.Setenv("ROBLOX_API_KEY", "")
	if _, err := robloxAPIKey(""); err == nil {
		t.Error("expected an error with no key anywhere")
	}
	if err := setToken(robloxAPI, "stored"); err != nil {
		t.Fatal(err)
	}
	if k, _ := robloxAPIKey(""); k != "stored" {
		t.Errorf("auth.toml key = %q", k)
	}
	t.Setenv("ROBLOX_API_KEY", "env")
	if k, _ := robloxAPIKey(""); k != "env" {
		t.Errorf("env key = %q", k)
	}
	if k, _ := robloxAPIKey("flag"); k != "flag" {
		t.Errorf("flag key = %q", k)
	}
}

func TestLoadRobloxConfig(t *testing.T) {
	dir := cliChdirTemp(t)
	if cfg, err := loadRobloxConfig(); err != nil || cfg != (RobloxConfig{}) {
		t.Errorf("missing chain.toml = %+v, %v", cfg, err)
	}
	cliWriteFile(t, filepath.Join(dir, "chain.toml"), "[Roblox]\nPlaceId = 12\nUserId = 34\nGroupId = 56\n", 0644)
	cfg, err := loadRobloxConfig()
	if err != nil || cfg != (RobloxConfig{PlaceId: 12, UserId: 34, GroupId: 56}) {
		t.Errorf("loadRobloxConfig = %+v, %v", cfg, err)
	}
	cliWriteFile(t, filepath.Join(dir, "chain.toml"), "[Roblox\n", 0644)
	if _, err := loadRobloxConfig(); err == nil {
		t.Error("malformed chain.toml accepted")
	}
}

func TestPlaceContentType(t *testing.T) {
	for in, want := range map[string]string{"a.rbxl": "application/octet-stream", "B.RBXLX": "application/xml"} {
		if got, err := placeContentType(in); err != nil || got != want {
			t.Errorf("placeContentType(%s) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"a.rbxm", "a.rbxmx", "a"} {
		if _, err := placeContentType(in); err == nil {
			t.Errorf("placeContentType(%s) succeeded", in)
		}
	}
}

func TestPublishPlace(t *testing.T) {
	f, c := cloudNewFake(t)
	asset, err := c.publishPlace(4242, "game.rbxlx", "application/xml", []byte("<roblox/>"), true)
	if err != nil {
		t.Fatal(err)
	}
	if asset.RevisionID != "1" {
		t.Errorf("revision = %q", asset.RevisionID)
	}
	u := f.uploads[0]
	if u.Method != "PATCH" || u.Path != "/assets/v1/assets/4242" {
		t.Errorf("sent %s %s", u.Method, u.Path)
	}
	if u.Request["assetId"] != float64(4242) || u.Request["assetType"] != "Place" || u.Request["published"] != true {
		t.Errorf("request = %v", u.Request)
	}
	if u.ContentType != "application/xml" || string(u.Data) != "<roblox/>" {
		t.Errorf("file = %s %q", u.ContentType, u.Data)
	}

	if _, err := c.publishPlace(4242, "game.rbxl", "application/octet-stream", nil, false); err != nil {
		t.Fatal(err)
	}
	if f.uploads[1].Request["published"] != false {
		t.Error("draft upload was published")
	}
}

func TestBuildPlaceIsBinary(t *testing.T) {
	buildHermeticHome(t)
	root := t.TempDir()
	buildWriteFile(t, root, "shared/Util.luau", "return 1")
	t.Chdir(root)
	data, err := buildPlace()
	if err != nil {
		t.Fatal(err)
	}
	f, err := decodeRobloxFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rbxDump(f.Roots), "Util") {
		t.Error("built place is missing shared/Util")
	}
}

func TestHandlePublishPlaceValidation(t *testing.T) {
	cliSetHome(t)
	dir := cliChdirTemp(t)
	t.Setenv("ROBLOX_API_KEY", "k")
	if err := handlePublishPlace(nil); err == nil || !strings.Contains(err.Error(), "place ID") {
		t.Errorf("no place ID: %v", err)
	}
	if err := handlePublishPlace([]string{"--place", "1", "a.rbxl", "b.rbxl"}); err == nil {
		t.Error("two files accepted")
	}
	cliWriteFile(t, filepath.Join(dir, "x.rbxm"), "", 0644)
	if err := handlePublishPlace([]string{"--place", "1", "x.rbxm"}); err == nil || !strings.Contains(err.Error(), "not a place file") {
		t.Errorf("model file: %v", err)
	}
	if err := handlePublishPlace([]string{"--place", "1", "missing.rbxl"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

func TestHandleLoginCloud(t *testing.T) {
	cliSetHome(t)
	out := cloudCaptureStdout(t, func() {
		if err := handleLoginCloud([]string{"--api-key", " flag-key "}); err != nil {
			t.Fatal(err)
		}
	})
	for _, p := range cloudKeyPermissions {
		if !strings.Contains(out, p[0]) || !strings.Contains(out, p[1]) {
			t.Errorf("output is missing permission %q: %s", p[0], out)
		}
	}
	if k, _ := getToken(robloxAPI); k != "flag-key" {
		t.Errorf("saved key = %q", k)
	}
	if err := handleLoginCloud([]string{"extra"}); err == nil {
		t.Error("stray argument accepted")
	}
}

func TestHandleLoginCloudPrompt(t *testing.T) {
	cliSetHome(t)
	cloudStdin(t, "typed-key\n")
	if err := handleLoginCloud(nil); err != nil {
		t.Fatal(err)
	}
	if k, _ := getToken(robloxAPI); k != "typed-key" {
		t.Errorf("saved key = %q", k)
	}

	cloudStdin(t, "\n")
	if err := handleLoginCloud(nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty key: %v", err)
	}
}

// cloudStdin replaces os.Stdin with a pipe holding input for the rest of the test.
func cloudStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(input)
	w.Close()
	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved; r.Close() })
}

// cloudCaptureStdout returns what fn writes to os.Stdout.
func cloudCaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	return string(out)
}
