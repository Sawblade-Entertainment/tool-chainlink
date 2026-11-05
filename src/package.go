package main

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// packageRegistryAPI is a variable so tests can point it at an httptest server.
var packageRegistryAPI = "https://api.wally.run/v1/package-contents"

// InstallPackageList installs a list of packages from a map where keys are aliases and values are package names (e.g., "vibeonline/razorinput@0.1.0")
func InstallPackageList(packages map[string]string) error {
	for _, alias := range slices.Sorted(maps.Keys(packages)) {
		packageName := packages[alias]
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

// packageInstallDir is the global dir for a package version:
// ~/.chainlink/packages/<scope>_<name>/<version>.
func packageInstallDir(home, scope, name, version string) string {
	return filepath.Join(home, ".chainlink", "packages", scope+"_"+name, version)
}

// InstallPackage installs a package by its name (e.g., "vibeonline/razorinput@0.1.0")
// It downloads the package zip directly from the package registry and extracts it.
func InstallPackage(alias, packageName string) error {
	scope, name, version, err := parsePackageSpec(packageName)
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	// Checked before any network I/O so re-installs cost nothing.
	globalPkgDir := packageInstallDir(home, scope, name, version)
	if _, err := os.Stat(globalPkgDir); err == nil {
		status("Cached", sgrBlue, "%s", packageName)
		return nil
	}

	// Construct the registry URL
	url := fmt.Sprintf("%s/%s/%s/%s", packageRegistryAPI, scope, name, version)

	status("Downloading", sgrCyan, "%s", packageName)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Use the official registry CLI User-Agent to avoid 426 Upgrade Required errors
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

	status("Installed", sgrGreen, "%s %s", packageName, dim(globalPkgDir))
	return nil
}
