package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// robloxAPI is the Open Cloud base URL. API keys saved with
// `chainlink login-cloud` are stored under it.
const robloxAPI = "https://apis.roblox.com"

// robloxCredentialsURL is where Open Cloud API keys are created.
const robloxCredentialsURL = "https://create.roblox.com/dashboard/credentials"

// robloxClient allows for large place uploads, which can outlast registryClient's timeout.
var robloxClient = &http.Client{Timeout: 10 * time.Minute}

// RobloxConfig is the [Roblox] table of chain.toml.
type RobloxConfig struct {
	PlaceId int64 // place that publish-place uploads to
	UserId  int64 // creator of uploaded assets...
	GroupId int64 // ...or the group that owns them (wins over UserId)
}

func loadRobloxConfig() (RobloxConfig, error) {
	var m struct {
		Roblox RobloxConfig `toml:"Roblox"`
	}
	if _, err := toml.DecodeFile(chainTomlName, &m); err != nil && !errors.Is(err, os.ErrNotExist) {
		return RobloxConfig{}, fmt.Errorf("could not read %s: %w", chainTomlName, err)
	}
	return m.Roblox, nil
}

// robloxAPIKey picks the Open Cloud key: --api-key, then ROBLOX_API_KEY, then auth.toml.
func robloxAPIKey(flagKey string) (string, error) {
	if flagKey != "" {
		return flagKey, nil
	}
	if k := strings.TrimSpace(os.Getenv("ROBLOX_API_KEY")); k != "" {
		return k, nil
	}
	k, err := getToken(robloxAPI)
	if err != nil {
		return "", errors.New("no Roblox API key: run 'chainlink login-cloud', set ROBLOX_API_KEY, or pass --api-key")
	}
	return k, nil
}

// cloudKeyPermissions lists what an Open Cloud API key needs for
// `chainlink assets` and `chainlink publish-place`, as {setting, value}.
var cloudKeyPermissions = [][2]string{
	{"Access permissions", "Assets API with Read and Write (asset:read, asset:write)"},
	{"Experiences", "add the experience publish-place uploads to ([Roblox] PlaceId)"},
	{"Owner", "create the key under the group if assets use [Roblox] GroupId"},
	{"Accepted IP addresses", "your IP, or 0.0.0.0/0 to allow any (e.g. for CI)"},
}

func printCloudKeyPermissions() {
	fmt.Printf("Create an Open Cloud API key at %s with:\n", bold(robloxCredentialsURL))
	width := 0
	for _, p := range cloudKeyPermissions {
		width = max(width, len(p[0]))
	}
	for _, p := range cloudKeyPermissions {
		fmt.Printf("  %s  %s\n", cyan(fmt.Sprintf("%-*s", width, p[0])), p[1])
	}
	fmt.Println()
}

// handleLoginCloud saves an Open Cloud API key to auth.toml, prompting for it
// unless --api-key is given. The permissions the key needs are printed first.
func handleLoginCloud(args []string) error {
	fs := flag.NewFlagSet("login-cloud", flag.ContinueOnError)
	keyFlag := fs.String("api-key", "", "API key to save (skips the prompt)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	printCloudKeyPermissions()
	key := strings.TrimSpace(*keyFlag)
	if key == "" {
		var err error
		if key, err = readAPIKey("Enter your Open Cloud API key:"); err != nil {
			return err
		}
	}
	if err := setToken(robloxAPI, key); err != nil {
		return err
	}
	status("Saved", sgrGreen, "Open Cloud API key")
	return nil
}

// openCloud talks to the Open Cloud Assets API. base and sleep are fields so
// tests can point it at a local server and skip the polling delay.
type openCloud struct {
	client *http.Client
	base   string
	key    string
	sleep  func(time.Duration)
}

func newOpenCloud(key string) openCloud {
	return openCloud{client: robloxClient, base: robloxAPI, key: key, sleep: time.Sleep}
}

// cloudOperation is the long-running Operation the Assets API returns for uploads.
type cloudOperation struct {
	Path     string          `json:"path"`
	Done     bool            `json:"done"`
	Response json.RawMessage `json:"response"`
	Error    *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// cloudAsset holds the fields of a finished operation's response that we use.
type cloudAsset struct {
	AssetID    cloudID `json:"assetId"`
	RevisionID cloudID `json:"revisionId"`
}

// cloudID accepts an ID sent either as a JSON string or a number.
type cloudID string

func (id *cloudID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*id = cloudID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("invalid ID %s", b)
	}
	*id = cloudID(n.String())
	return nil
}

// operationTimeout bounds how long wait polls an upload before giving up.
// It is a var so tests can shorten it.
var operationTimeout = 5 * time.Minute

// upload sends file as the fileContent of a multipart Assets API request and
// waits for the resulting operation to finish.
func (c openCloud) upload(method, endpoint string, request any, filename, contentType string, file []byte) (cloudAsset, error) {
	meta, err := json.Marshal(request)
	if err != nil {
		return cloudAsset{}, err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("request", string(meta)); err != nil {
		return cloudAsset{}, err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="fileContent"; filename=%q`, filename))
	h.Set("Content-Type", contentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		return cloudAsset{}, err
	}
	if _, err := part.Write(file); err != nil {
		return cloudAsset{}, err
	}
	if err := mw.Close(); err != nil {
		return cloudAsset{}, err
	}

	req, err := http.NewRequest(method, c.base+endpoint, &body)
	if err != nil {
		return cloudAsset{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var op cloudOperation
	if err := c.do(req, &op); err != nil {
		return cloudAsset{}, err
	}
	return c.wait(op)
}

// wait polls op until it is done and decodes its response.
func (c openCloud) wait(op cloudOperation) (cloudAsset, error) {
	deadline := time.Now().Add(operationTimeout)
	interval := time.Second
	for !op.Done {
		if op.Path == "" {
			return cloudAsset{}, errors.New("Open Cloud returned no operation to wait for")
		}
		if time.Now().After(deadline) {
			return cloudAsset{}, fmt.Errorf("timed out waiting for %s", op.Path)
		}
		c.sleep(interval)
		interval = min(interval*2, 8*time.Second)
		req, err := http.NewRequest("GET", c.base+"/assets/v1/"+op.Path, nil)
		if err != nil {
			return cloudAsset{}, err
		}
		if err := c.do(req, &op); err != nil {
			return cloudAsset{}, err
		}
	}
	if op.Error != nil {
		return cloudAsset{}, fmt.Errorf("Open Cloud: %s", op.Error.Message)
	}
	var asset cloudAsset
	if err := json.Unmarshal(op.Response, &asset); err != nil {
		return cloudAsset{}, fmt.Errorf("malformed Open Cloud response: %w", err)
	}
	return asset, nil
}

// do sends req with the API key and decodes a JSON reply into out.
func (c openCloud) do(req *http.Request, out any) error {
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("%s %s: %s\n%s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(data)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- Place publishing ---

// placeContentType maps a place file extension to its Open Cloud upload type.
func placeContentType(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".rbxl":
		return "application/octet-stream", nil
	case ".rbxlx":
		return "application/xml", nil
	}
	return "", fmt.Errorf("%s is not a place file (expected .rbxl or .rbxlx)", path)
}

// buildPlace builds the current project as a binary place, like `chainlink build x.rbxl`.
func buildPlace() ([]byte, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	b := NewBuilder(loadConfig(), root)
	b.populatePlace()
	if b.readErrs > 0 {
		return nil, fmt.Errorf("build: %d file(s) could not be read; nothing was published", b.readErrs)
	}
	return b.encode(".rbxl"), nil
}

// publishPlace uploads a place file as a new version of placeID.
func (c openCloud) publishPlace(placeID int64, filename, contentType string, data []byte, publish bool) (cloudAsset, error) {
	request := map[string]any{"assetId": placeID, "assetType": "Place", "published": publish}
	return c.upload("PATCH", "/assets/v1/assets/"+strconv.FormatInt(placeID, 10), request, filename, contentType, data)
}

func handlePublishPlace(args []string) error {
	fs := flag.NewFlagSet("publish-place", flag.ContinueOnError)
	placeFlag := fs.Int64("place", 0, "place ID to publish to (overrides [Roblox] PlaceId)")
	keyFlag := fs.String("api-key", "", "Open Cloud API key (overrides ROBLOX_API_KEY and auth.toml)")
	draft := fs.Bool("draft", false, "save the version without publishing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("expected at most one place file, got %d", fs.NArg())
	}

	rcfg, err := loadRobloxConfig()
	if err != nil {
		return err
	}
	placeID := rcfg.PlaceId
	if *placeFlag != 0 {
		placeID = *placeFlag
	}
	if placeID <= 0 {
		return errors.New("no place ID: set PlaceId under [Roblox] in chain.toml or pass --place")
	}
	key, err := robloxAPIKey(*keyFlag)
	if err != nil {
		return err
	}

	var data []byte
	filename, contentType := "place.rbxl", "application/octet-stream"
	if file := fs.Arg(0); file != "" {
		if contentType, err = placeContentType(file); err != nil {
			return err
		}
		if data, err = os.ReadFile(file); err != nil {
			return err
		}
		filename = filepath.Base(file)
	} else {
		status("Building", sgrCyan, "place")
		if data, err = buildPlace(); err != nil {
			return err
		}
	}

	status("Uploading", sgrCyan, "%s to place %d %s", filename, placeID, dim(fmt.Sprintf("(%d KiB)", len(data)/1024)))
	asset, err := newOpenCloud(key).publishPlace(placeID, filename, contentType, data, !*draft)
	if err != nil {
		return err
	}
	verb := "Published"
	if *draft {
		verb = "Saved"
	}
	status(verb, sgrGreen, "place %d version %s", placeID, asset.RevisionID)
	return nil
}
