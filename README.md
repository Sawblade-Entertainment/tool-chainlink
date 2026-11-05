# ChainLink

ChainLink is a tool manager for Roblox development, allowing you to easily manage tools and packages for your project.

## Installation

### From Releases
Download the binary for your platform from the [Releases](https://github.com/Sawblade-Entertainment/ChainLink/releases) page.

### From Source
```bash
make        # runs tests, then builds all platforms into bin/
```

## Usage

ChainLink uses a `chain.toml` file in your project root to track dependencies.

### Basic Commands

```bash
# Initialize a new ChainLink project
# Creates chain.toml and a basic project structure (client/, server/, shared/)
chainlink init

# Install all tools and packages defined in chain.toml
# (asks before downloading anything new; -y skips the prompt)
chainlink install
chainlink install -y

# Install the ChainLink plugin for Roblox Studio
chainlink plugin

# Add a tool to your project
# Format: owner/repo@tag (e.g., BurntSushi/toml@v1.6.0)
chainlink add owner/repo@tag --tool

# Add a package to your project
# Format: scope/name@version (e.g., my-scope/my-package@1.0.0)
chainlink add scope/name@version --package

# Remove a tool or package
chainlink remove name

# Start the ChainLink server
chainlink serve

# Install ChainLink globally to your system
chainlink self-install

# Authenticate with a registry to publish packages
chainlink login

# Publish the current project as a package to a registry
chainlink publish

# Show help
chainlink help
```

## Configuration

The `chain.toml` file is structured as follows:

```toml
[Package]
Name = "MyProject"
Version = "1.0.0"
License = "MIT"
Description = "A great project"
Website = "https://example.com"

[Tools]
selene = "Kampfkarren/selene@0.32.0"

[Packages]
razorinput = "vibeonline/razorinput@0.1.0"
```

## Tool Proxying

ChainLink can act as a proxy for the tools it manages. If you create a symlink to the `chainlink` binary named after a tool in your `chain.toml` (e.g., `selene` -> `chainlink`), running that symlink will automatically execute the corresponding tool with the correct version.
