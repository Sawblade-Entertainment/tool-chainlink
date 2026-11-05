# Usage Guide

ChainLink provides a suite of commands to manage your project's dependencies and environment.

## Project Lifecycle

### Initializing a Project
To start a new project with ChainLink:
```bash
chainlink init
```
This creates a `chain.toml` file and sets up a basic project structure:
- `client/`: Client-side scripts.
- `server/`: Server-side scripts.
- `shared/`: Shared modules.
It also initializes a Git repository if one doesn't already exist.

### Installing Dependencies
To install all tools and packages listed in your `chain.toml`:
```bash
chainlink install
```

Before downloading anything that isn't already in your global cache (`~/.chainlink`), ChainLink lists it and asks for confirmation. Pass `-y` (or `--yes`) to skip the prompt, e.g. in CI or scripts. Without a terminal to answer, `install` stops and asks you to re-run with `-y`:
```bash
chainlink install -y
```

Output is colored when writing to a terminal. Set `NO_COLOR=1` to turn colors off, or `FORCE_COLOR=1` to force them on.

## Managing Dependencies

### Adding Tools and Packages
You can add dependencies using the `add` command.

**Adding a Tool:**
```bash
chainlink add owner/repo@tag --tool
# Example: chainlink add Kampfkarren/selene@0.32.0 --tool
```

**Adding a Package:**
```bash
chainlink add scope/name@version --package
# Example: chainlink add vibeonline/razorinput@0.1.0 --package
```

### Removing Dependencies
To remove a tool or package:
```bash
chainlink remove name
```

## Advanced Commands

### The Roblox Studio Plugin
To install the ChainLink plugin for Roblox Studio:
```bash
chainlink plugin
```

### Serving
Start the ChainLink server for integrated development:
```bash
chainlink serve
```
