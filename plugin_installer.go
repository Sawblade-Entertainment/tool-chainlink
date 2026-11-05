package main

import (
	_ "embed"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"runtime"
)

//go:embed plugin/ChainLink.plugin.luau
var pluginSource string

const rbxmxTemplate = `<roblox xmlns:xmime="http://www.w3.org/2005/05/xmlmime" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:noNamespaceSchemaLocation="http://www.roblox.com/roblox.xsd" version="4">
	<Meta name="ExplicitAutoJoints">true</Meta>
	<External>null</External>
	<External>nil</External>
	<Item class="Folder" referent="RBXc55df6218100459aacc58270c98d724a">
		<Properties>
			<Color3 name="IconTint">
				<R>0</R>
				<G>0</G>
				<B>0</B>
			</Color3>
			<BinaryString name="AttributesSerialize"></BinaryString>
			<SecurityCapabilities name="Capabilities">0</SecurityCapabilities>
			<bool name="DefinesCapabilities">false</bool>
			<string name="Name">Folder</string>
			<BinaryString name="SerializedOverrides"></BinaryString>
			<int64 name="SourceAssetId">-1</int64>
			<SharedString name="Tags">yuZpQdnvvUBOTYh1jqZ2cA==</SharedString>
		</Properties>
		<Item class="Script" referent="RBXa19c4d002b5e4c6899e7991b1a5c4bb4">
			<Properties>
				<ProtectedString name="Source">%s</ProtectedString>
				<bool name="Disabled">false</bool>
				<Content name="LinkedSource"><null></null></Content>
				<token name="RunContext">3</token>
				<string name="ScriptGuid">{d9ee86fa-ce8e-4bf5-b903-eec0b41c8991}</string>
				<BinaryString name="AttributesSerialize"></BinaryString>
				<SecurityCapabilities name="Capabilities">0</SecurityCapabilities>
				<bool name="DefinesCapabilities">false</bool>
				<string name="Name">Plugin</string>
				<BinaryString name="SerializedOverrides"></BinaryString>
				<int64 name="SourceAssetId">-1</int64>
				<SharedString name="Tags">yuZpQdnvvUBOTYh1jqZ2cA==</SharedString>
			</Properties>
		</Item>
	</Item>
	<SharedStrings>
		<SharedString md5="yuZpQdnvvUBOTYh1jqZ2cA=="></SharedString>
	</SharedStrings>
</roblox>`

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
	content := fmt.Sprintf(rbxmxTemplate, escapedSource)
	destPath := filepath.Join(pluginsDir, "ChainLink.rbxmx")

	err = os.WriteFile(destPath, []byte(content), 0644)
	if err != nil {
		return fmt.Errorf("failed to write rbxmx file: %w", err)
	}

	// If an old binary version exists, remove it to avoid confusion
	_ = os.Remove(filepath.Join(pluginsDir, "ChainLink.rbxm"))

	fmt.Printf("Successfully installed ChainLink plugin to %s\n", destPath)
	return nil
}

// Removed main to avoid conflict with main.go
