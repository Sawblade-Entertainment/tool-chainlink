package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// wallyRegistryAPI is a variable so tests can point it at an httptest server.
var wallyRegistryAPI = "https://api.wally.run/v1/package-contents"

// InstallPackageList installs a list of wally packages from a map where keys are aliases and values are package names (e.g., "vibeonline/razorinput@0.1.0")
func InstallPackageList(packages map[string]string) error {
	for alias, packageName := range packages {
		fmt.Printf("Installing %s...\n", packageName)
		if err := InstallPackage(alias, packageName); err != nil {
			return fmt.Errorf("failed to install package %s: %w", packageName, err)
		}
	}
	return nil
}

// parsePackageSpec parses "scope/name@version".
func parsePackageSpec(packageName string) (scope, name, version string, err error) {
	parts := strings.Split(packageName, "@")
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid package name format: %s. Expected scope/name@version", packageName)
	}
	scopeParts := strings.Split(parts[0], "/")
	if len(scopeParts) != 2 || !validPathComponent(scopeParts[0]) || !validPathComponent(scopeParts[1]) {
		return "", "", "", fmt.Errorf("invalid scope/name format: %s. Expected scope/name", parts[0])
	}
	if !validPathComponent(parts[1]) {
		return "", "", "", fmt.Errorf("invalid package version in %s", packageName)
	}
	return scopeParts[0], scopeParts[1], parts[1], nil
}

// InstallPackage installs a wally package by its name (e.g., "vibeonline/razorinput@0.1.0")
// It downloads the package zip directly from the Wally registry and extracts it.
func InstallPackage(alias, packageName string) error {
	scope, name, version, err := parsePackageSpec(packageName)
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	// Global package directory: ~/.chainlink/packages/scope_name/version
	// Checked before any network I/O so re-installs cost nothing.
	globalPkgDir := filepath.Join(home, ".chainlink", "packages", scope+"_"+name, version)
	if _, err := os.Stat(globalPkgDir); err == nil {
		fmt.Printf("Package %s/%s version %s is already installed globally. Skipping.\n", scope, name, version)
		return nil
	}

	// Construct the registry URL
	url := fmt.Sprintf("%s/%s/%s/%s", wallyRegistryAPI, scope, name, version)

	fmt.Printf("Downloading package %s from %s...\n", packageName, url)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Use the official Wally CLI User-Agent to avoid 426 Upgrade Required errors
	req.Header.Set("User-Agent", "wally/0.3.2")
	req.Header.Set("Wally-Version", "0.3.2")

	// Download and extract into a staging dir, and move it into place only on
	// success, so a failed install is retried rather than skipped next time.
	staging, err := newStagingDir(home)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	zipPath := filepath.Join(staging, "package.zip")
	if err := downloadRequest(req, zipPath); err != nil {
		return fmt.Errorf("failed to download package: %v", err)
	}

	contents := filepath.Join(staging, "contents")
	if err := os.MkdirAll(contents, 0755); err != nil {
		return err
	}
	if err := extractZip(zipPath, contents); err != nil {
		return fmt.Errorf("failed to extract package globally: %v", err)
	}
	if err := moveIntoPlace(contents, globalPkgDir); err != nil {
		return err
	}

	fmt.Printf("Successfully installed %s globally to %s\n", packageName, globalPkgDir)
	return nil
}
