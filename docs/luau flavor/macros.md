# Macros

The build system provides a set of global "macros" (injected helper functions) that are prepended to Luau scripts when the source assigns from them (`x = use ...`, `part.Texture = asset ...`). A bare call whose result is not assigned does not inject the macro.

## Injected Globals

### `use "module"`
A versatile utility for loading modules or retrieving services.

**Behavior:**
- If `module` is a string:
    - If it starts with `.` or `@`, it is passed directly to `require(module)`.
    - If it starts with `/`, it is a path under `ReplicatedStorage`: each segment is found with `WaitForChild` and the result is required.
    - If it is lowercase, it is treated as a package name and resolved via `game.ReplicatedStorage.packages[module]`.
    - Otherwise, it is treated as a Roblox Service name and resolved via `game:GetService(module)`.
- Otherwise, `module` is passed directly to `require(module)`.

**Example:**
```lua
local TweenService = use "TweenService" -- Retrieves game:GetService("TweenService")
local MyPackage = use "my_package"      -- Retrieves require(game.ReplicatedStorage.packages.my_package)
local Config = use "/shared/Config"     -- Retrieves require(game.ReplicatedStorage.shared.Config)
local MyModule = use path.to.module     -- Retrieves require(path.to.module)
```

### `new "instance"`
A shorthand for creating new Roblox instances.

**Behavior:**
- Calls `Instance.new(instance)` and returns the result.

**Example:**
```lua
local part = new "Part" -- Equivalent to Instance.new("Part")
```

### `asset "path"`
Returns the content ID of a file in the project's `assets/` directory that `chainlink assets` has uploaded.

**Behavior:**
- `path` is the file's path under `assets/`, with forward slashes and its extension.
- Looks the path up in `ReplicatedStorage.assets`, a ModuleScript that `serve` and `build` generate from `assets.lock.json`.
- Errors if the file has not been uploaded yet.

**Example:**
```lua
local icon = asset "ui/icon.png" -- "rbxassetid://123456789"
sound.SoundId = asset "sfx/hit.ogg"
```
