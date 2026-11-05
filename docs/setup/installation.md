# Installation

ChainLink can be installed either from pre-built releases or by building from source.

## From Releases

1. Navigate to the [Releases](https://github.com/Sawblade-Entertainment/tool-chainlink/releases) page.
2. Download the binary corresponding to your operating system and architecture (e.g., `chainlink-darwin-arm64` for Apple Silicon Macs).
3. Run the binary directly from your downloads folder. As long as the binary name retains its release prefix (e.g., `chainlink-darwin-arm64`), it will automatically trigger a global installation.

```bash
# Example for macOS arm64
./chainlink-darwin-arm64
```

This will install the binary to `~/.chainlink/bin/chainlink` and attempt to update your system `PATH`.

## From Source

If you have Go installed on your system, you can build ChainLink directly from the repository:

```bash
git clone https://github.com/Sawblade-Entertainment/tool-chainlink.git
cd tool-chainlink
make        # runs tests, then builds all platforms into bin/
```

## Global Installation

Once you have the `chainlink` binary, you can use the built-in `self-install` command to set up the tool permanently on your system:

```bash
./chainlink self-install
```

This command will:
1. Create a directory at `~/.chainlink/bin`.
2. Copy the current binary to that directory.
3. Attempt to add `~/.chainlink/bin` to your shell configuration file (e.g., `.zshrc` or `.bashrc`).

After running this, restart your terminal or run `source ~/.zshrc` (or the equivalent for your shell) to start using `chainlink` from anywhere.
