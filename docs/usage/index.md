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
- `prefab/`: Models (`.rbxm`/`.rbxmx`) synced to `ServerStorage`. Starts with `Office.rbxmx`, a Sawblade Entertainment office in dark glass, charcoal and wood: a three-floor building with a lobby, reception and lounge, stairs, office floors with desks and a glass meeting room, a branded entrance canopy and roof logo, and a front plaza with lamps, trees and the spawn.
- `assets/`: Images, audio, models and videos for `chainlink assets` to upload. Starts with `spawn.png`, the ChainLink logo.

The starter `server/ExampleService.luau` puts a copy of the Office in `Workspace`. After `chainlink assets` has uploaded `spawn.png`, it shows the logo on every Decal named `Logo` in the office with `asset "spawn.png"`; until then the spawn keeps Roblox's default decal and the other logo panels stay plain black.
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
The ChainLink plugin for Roblox Studio is installed by `chainlink self-install`.

### Syncing Assets
Put images, audio, models and videos in an `assets/` directory, then upload them to Roblox:
```bash
chainlink assets
```

Supported files: `.png`, `.jpg`/`.jpeg`, `.bmp`, `.tga` (uploaded as Images), `.mp3`, `.ogg`, `.wav`, `.flac` (Audio), `.fbx`, `.gltf`, `.glb`, `.rbxm`, `.rbxmx` (Models) and `.mp4`, `.mov` (Videos). Other files are skipped with a warning, and hidden files are ignored.

Only new or changed files are uploaded; IDs are kept in `assets.lock.json`. A changed file becomes a new asset with a new ID. Use `--dry-run` to see what would be uploaded. Use the [`asset` macro](../luau%20flavor/macros.md) to reference uploads from code.

Assets are owned by the `UserId` or `GroupId` in the `[Roblox]` section of `chain.toml`. Save an Open Cloud API key first with `chainlink login-cloud`; see [Configuration](../setup/configuration.md) for the permissions it needs.

### Publishing a Place
Publish a place file to the place set by `PlaceId` under `[Roblox]` in `chain.toml`:
```bash
chainlink publish-place game.rbxl      # or game.rbxlx
chainlink publish-place                # build this project as .rbxl and publish it
chainlink publish-place --place 123    # override PlaceId
chainlink publish-place --draft        # save a new version without publishing it
```

### Serving
Start the ChainLink server for integrated development:
```bash
chainlink serve
```
