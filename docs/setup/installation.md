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
4. Install the ChainLink plugin into Roblox Studio's plugins folder.

After running this, restart your terminal or run `source ~/.zshrc` (or the equivalent for your shell) to start using `chainlink` from anywhere.

## Updating

ChainLink checks GitHub for a newer release at most once a day and, when one is out, prints a notice after the command you ran. To update:

```bash
chainlink update
```

This downloads the latest release for your platform into `~/.chainlink/bin` and refreshes the Studio plugin. Use `chainlink update --check` to only see whether an update is available, and `chainlink version` to see what you're running.

The check is skipped in CI, when output isn't a terminal, and for builds made from source. Set `CHAINLINK_NO_UPDATE_CHECK=1` to turn it off.
