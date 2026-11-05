package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// InstallChainlink sets up the Chainlink environment.
// It creates ~/.chainlink/bin and provides instructions to add it to the PATH.
func InstallChainlink() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	binDir := filepath.Join(home, ".chainlink", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("failed to create bin directory: %w", err)
	}

	fmt.Println("Chainlink environment initialized.")
	fmt.Printf("Bin directory created at: %s\n", binDir)

	shellConfig, err := shellConfigFor(runtime.GOOS, os.Getenv("SHELL"), home)
	if err != nil {
		return err
	}
	if shellConfig == "" {
		fmt.Println("\nTo add Chainlink to your PATH on Windows:")
		fmt.Printf("1. Search for 'Edit the system environment variables'\n")
		fmt.Printf("2. Click 'Environment Variables'\n")
		fmt.Printf("3. Under 'User variables', find PATH and click 'Edit'\n")
		fmt.Printf("4. Add a new entry: %s\n", binDir)
		return nil
	}

	// Check if already in PATH
	if pathListContains(os.Getenv("PATH"), binDir) {
		fmt.Printf("\n%s is already in your PATH. Skipping PATH update.\n", binDir)
		return nil
	}

	// The PATH of this process is stale until the shell restarts, so also check
	// the rc file to avoid appending the same line on every run.
	if data, err := os.ReadFile(shellConfig); err == nil && mentionsDir(string(data), binDir) {
		fmt.Printf("\n%s already adds %s to your PATH.\n", shellConfig, binDir)
		fmt.Println("Please restart your terminal or run: source " + shellConfig)
		return nil
	}

	pathLine := fmt.Sprintf("\nexport PATH=\"%s:$PATH\"\n", binDir)

	f, err := os.OpenFile(shellConfig, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("failed to open shell config %s: %w", shellConfig, err)
	}

	if _, err := f.WriteString(pathLine); err != nil {
		f.Close()
		return fmt.Errorf("failed to write to shell config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to write to shell config: %w", err)
	}

	fmt.Printf("\nAdded %s to %s\n", binDir, shellConfig)
	fmt.Println("Please restart your terminal or run: source " + shellConfig)

	return nil
}

// shellConfigFor returns the rc file that should get the PATH line, or "" on
// Windows where the user is told how to edit PATH instead.
func shellConfigFor(goos, shell, home string) (string, error) {
	switch goos {
	case "darwin", "linux":
		name := filepath.Base(shell)
		switch {
		case strings.Contains(name, "zsh"):
			return filepath.Join(home, ".zshrc"), nil
		case goos == "darwin" && strings.Contains(name, "bash"):
			// Terminal.app starts login shells, which read .bash_profile.
			return filepath.Join(home, ".bash_profile"), nil
		default:
			return filepath.Join(home, ".bashrc"), nil
		}
	case "windows":
		return "", nil
	default:
		return "", fmt.Errorf("unsupported operating system: %s", goos)
	}
}

// pathListContains reports whether dir is exactly one of the entries in a
// PATH-style list.
func pathListContains(pathList, dir string) bool {
	dir = filepath.Clean(dir)
	for _, entry := range filepath.SplitList(pathList) {
		if entry != "" && filepath.Clean(entry) == dir {
			return true
		}
	}
	return false
}

// mentionsDir reports whether text contains dir as a whole path, so that
// "/x/.chainlink/bin-old" does not count as "/x/.chainlink/bin".
func mentionsDir(text, dir string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], dir)
		if j < 0 {
			return false
		}
		end := i + j + len(dir)
		if end == len(text) || strings.ContainsRune(":\"'/ \t\r\n;", rune(text[end])) {
			return true
		}
		i = i + j + 1
	}
}
