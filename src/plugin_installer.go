package main

import (
	_ "embed"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed plugin/ChainLink.plugin.luau
var pluginSource string

// rbxmxTemplate wraps the plugin source in a model file; {{Source}} is
// replaced with the XML-escaped source.
//
//go:embed templates/plugin.rbxmx
var rbxmxTemplate string

func getRobloxPluginsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return pluginsDirFor(runtime.GOOS, home)
}

// pluginsDirFor returns Roblox Studio's local plugins folder for goos.
func pluginsDirFor(goos, home string) (string, error) {
	switch goos {
	case "windows":
		return filepath.Join(home, "AppData", "Local", "Roblox", "Plugins"), nil
	case "darwin":
		return filepath.Join(home, "Documents", "Roblox", "Plugins"), nil
	case "linux":
		return filepath.Join(home, ".local", "share", "roblox", "Plugins"), nil
	default:
		return "", fmt.Errorf("unsupported platform: %s", goos)
	}
}

func installPlugin() error {
	pluginsDir, err := getRobloxPluginsDir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		return fmt.Errorf("failed to create plugins directory: %w", err)
	}

	// Escape the source code to prevent XML corruption
	escapedSource := html.EscapeString(pluginSource)
	content := strings.Replace(rbxmxTemplate, "{{Source}}", escapedSource, 1)
	destPath := filepath.Join(pluginsDir, "ChainLink.rbxmx")

	err = os.WriteFile(destPath, []byte(content), 0644)
	if err != nil {
		return fmt.Errorf("failed to write rbxmx file: %w", err)
	}

	// If an old binary version exists, remove it to avoid confusion
	_ = os.Remove(filepath.Join(pluginsDir, "ChainLink.rbxm"))

	status("Installed", sgrGreen, "Studio plugin %s", dim(destPath))
	return nil
}

// Removed main to avoid conflict with main.go
