package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	assetsDir     = "assets"
	assetLockName = "assets.lock.json"
	// assetModuleName is the ModuleScript under ReplicatedStorage that maps
	// asset paths to content IDs; the asset macro requires it.
	assetModuleName = "assets"
	// maxAssetName is the longest displayName Open Cloud accepts.
	maxAssetName = 50
)

// assetKind is how one file extension uploads to Open Cloud.
type assetKind struct {
	Type        string
	ContentType string
}

// assetKinds lists the file types Open Cloud accepts, by extension. Images
// upload as Image (not Decal) so the returned ID works in Image properties.
var assetKinds = map[string]assetKind{
	".png":   {"Image", "image/png"},
	".jpg":   {"Image", "image/jpeg"},
	".jpeg":  {"Image", "image/jpeg"},
	".bmp":   {"Image", "image/bmp"},
	".tga":   {"Image", "image/tga"},
	".mp3":   {"Audio", "audio/mpeg"},
	".ogg":   {"Audio", "audio/ogg"},
	".wav":   {"Audio", "audio/wav"},
	".flac":  {"Audio", "audio/flac"},
	".fbx":   {"Model", "model/fbx"},
	".gltf":  {"Model", "model/gltf+json"},
	".glb":   {"Model", "model/gltf-binary"},
	".rbxm":  {"Model", "model/x-rbxm"},
	".rbxmx": {"Model", "model/x-rbxm"},
	".mp4":   {"Video", "video/mp4"},
	".mov":   {"Video", "video/mov"},
}

// lockedAsset is one uploaded file in assets.lock.json.
type lockedAsset struct {
	ID   string `json:"id"`
	Hash string `json:"hash"` // sha256 of the uploaded content
}

// assetLock maps slash-separated paths under assets/ to their uploads.
type assetLock map[string]lockedAsset

func readAssetLock(path string) (assetLock, error) {
	lock := assetLock{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return lock, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("malformed %s: %w", path, err)
	}
	return lock, nil
}

func (l assetLock) save(path string) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return replaceFileAtomic(path, append(data, '\n'), 0644)
}

// assetModuleSource renders assets.lock.json as the ModuleScript the asset
// macro reads: a table of asset path -> "rbxassetid://<id>".
func assetModuleSource(data []byte) (string, error) {
	var lock assetLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return "", fmt.Errorf("malformed %s: %w", assetLockName, err)
	}
	ids := make(map[string]any, len(lock))
	for p, a := range lock {
		ids[p] = "rbxassetid://" + a.ID
	}
	return "return " + jsonToLuau(ids), nil
}

// localAsset is an uploadable file found under assets/.
type localAsset struct {
	Rel  string // slash-separated, relative to assets/
	Path string
	Kind assetKind
	Hash string
}

// scanAssets finds every uploadable file under dir. Hidden files are skipped
// quietly; other unsupported files get a warning.
func scanAssets(dir string) ([]localAsset, error) {
	var found []localAsset
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != dir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		kind, ok := assetKinds[strings.ToLower(filepath.Ext(p))]
		if !ok {
			warnf("skipping %s/%s (not a type Roblox accepts)", assetsDir, rel)
			return nil
		}
		hash, err := hashFile(p)
		if err != nil {
			return err
		}
		found = append(found, localAsset{Rel: rel, Path: p, Kind: kind, Hash: hash})
		return nil
	})
	return found, err
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// assetCreator is the creationContext.creator an upload is owned by.
func assetCreator(cfg RobloxConfig) (map[string]string, error) {
	switch {
	case cfg.GroupId > 0:
		return map[string]string{"groupId": strconv.FormatInt(cfg.GroupId, 10)}, nil
	case cfg.UserId > 0:
		return map[string]string{"userId": strconv.FormatInt(cfg.UserId, 10)}, nil
	}
	return nil, errors.New("no asset creator: set UserId or GroupId under [Roblox] in chain.toml")
}

// assetDisplayName is the file name without its extension, cut to maxAssetName runes.
func assetDisplayName(rel string) string {
	name := strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	if r := []rune(name); len(r) > maxAssetName {
		name = string(r[:maxAssetName])
	}
	return name
}

// uploadAsset creates a new Roblox asset from a and returns its ID.
func (c openCloud) uploadAsset(a localAsset, creator map[string]string) (string, error) {
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", err
	}
	request := map[string]any{
		"assetType":       a.Kind.Type,
		"displayName":     assetDisplayName(a.Rel),
		"description":     assetsDir + "/" + a.Rel,
		"creationContext": map[string]any{"creator": creator},
	}
	asset, err := c.upload("POST", "/assets/v1/assets", request, path.Base(a.Rel), a.Kind.ContentType, data)
	if err != nil {
		return "", err
	}
	if asset.AssetID == "" {
		return "", errors.New("Open Cloud returned no asset ID")
	}
	return string(asset.AssetID), nil
}

// syncAssets uploads every file in assets/ that is new or changed since it was
// last uploaded, and drops lock entries for files that are gone. The lock is
// saved after each upload so a failure part way keeps what already succeeded.
func syncAssets(c openCloud, creator map[string]string, dryRun bool) error {
	if !dirExists(assetsDir) {
		return fmt.Errorf("no %s/ directory found", assetsDir)
	}
	found, err := scanAssets(assetsDir)
	if err != nil {
		return err
	}
	lock, err := readAssetLock(assetLockName)
	if err != nil {
		return err
	}

	present := map[string]bool{}
	var pending []localAsset
	for _, a := range found {
		present[a.Rel] = true
		if lock[a.Rel].Hash != a.Hash {
			pending = append(pending, a)
		}
	}
	var stale []string
	for _, rel := range slices.Sorted(maps.Keys(lock)) {
		if !present[rel] {
			stale = append(stale, rel)
		}
	}

	if dryRun {
		for _, a := range pending {
			status("Would upload", sgrCyan, "%s/%s %s", assetsDir, a.Rel, dim("("+a.Kind.Type+")"))
		}
		for _, rel := range stale {
			status("Would forget", sgrYellow, "%s/%s", assetsDir, rel)
		}
		status("Finished", sgrGreen, "%d to upload, %d up to date", len(pending), len(found)-len(pending))
		return nil
	}

	for _, rel := range stale {
		delete(lock, rel)
		status("Forgot", sgrYellow, "%s/%s %s", assetsDir, rel, dim("(file removed)"))
	}
	if len(stale) > 0 {
		if err := lock.save(assetLockName); err != nil {
			return err
		}
	}

	failed := 0
	for _, a := range pending {
		status("Uploading", sgrCyan, "%s/%s %s", assetsDir, a.Rel, dim("("+a.Kind.Type+")"))
		id, err := c.uploadAsset(a, creator)
		if err != nil {
			warnf("%s/%s: %v", assetsDir, a.Rel, err)
			failed++
			continue
		}
		lock[a.Rel] = lockedAsset{ID: id, Hash: a.Hash}
		if err := lock.save(assetLockName); err != nil {
			return err
		}
		status("Uploaded", sgrGreen, "%s/%s %s", assetsDir, a.Rel, dim("rbxassetid://"+id))
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d upload(s) failed", failed, len(pending))
	}
	status("Finished", sgrGreen, "%d uploaded, %d up to date", len(pending), len(found)-len(pending))
	return nil
}

func handleAssets(args []string) error {
	fs := flag.NewFlagSet("assets", flag.ContinueOnError)
	keyFlag := fs.String("api-key", "", "Open Cloud API key (overrides ROBLOX_API_KEY and auth.toml)")
	dryRun := fs.Bool("dry-run", false, "list what would be uploaded without uploading")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dryRun {
		return syncAssets(openCloud{}, nil, true)
	}

	rcfg, err := loadRobloxConfig()
	if err != nil {
		return err
	}
	creator, err := assetCreator(rcfg)
	if err != nil {
		return err
	}
	key, err := robloxAPIKey(*keyFlag)
	if err != nil {
		return err
	}
	return syncAssets(newOpenCloud(key), creator, false)
}
