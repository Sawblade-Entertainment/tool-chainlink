# Configuration

ChainLink's behavior is driven by the `chain.toml` file and a hidden authentication store.

## The `chain.toml` File

This file tracks all project-level dependencies and metadata.

### `[Package]` Section
Contains metadata about the current project. This is used when publishing the project as a package.

| Field | Type | Description |
| :--- | :--- | :--- |
| `Name` | String | The name of the package. |
| `Version` | String | The semver version of the package. |
| `License` | String | The license (e.g., "MIT"). |
| `Description` | String | A short description of the project. |
| `Website` | String | The project's homepage URL. |

When `Name` is set and `src/init.luau` exists, `chainlink serve` (and `build`/`sourcemap`) map the project's `src/` directory to `ReplicatedStorage.packages.<Name>`, the same place installed packages go. `src/init.luau` becomes the package's ModuleScript. A scoped name such as `scope/mylib` uses only the last part (`mylib`). `src/` is not mapped when a root in `chainlink.json` already covers it (for example `src` or `src/lib`), when the name contains a `.`, or when a dependency in `[Packages]` maps to the same name. Changing `Name` or adding `src/init.luau` requires restarting `serve`.

The same mapping is available for any directory through `chainlink.json`: a root with `"package": true` makes the directory itself the target instance (its `init.luau` becomes that instance) instead of putting its contents inside the target. The target needs a parent, so `"target": "ReplicatedStorage.lib"` works but `"target": "Workspace"` is mapped as a plain root.

```json
{ "roots": [{ "dir": "lib", "target": "ReplicatedStorage.lib", "package": true }] }
```

### `[Tools]` Section
A map of tool names to their specifications.
**Format:** `tool_name = "owner/repo@tag"`
Example: `selene = "Kampfkarren/selene@0.32.0"`

### `[Packages]` Section
A map of package names to their specifications.
**Format:** `package_name = "scope/name@version"`
Example: `razorinput = "vibeonline/razorinput@0.1.0"`

Each package syncs to `ReplicatedStorage.packages.<name>`, using the last part of `scope/name`. A package whose name contains a `.` is skipped with a warning, because Roblox paths use `.` as the separator.

### `[Roblox]` Section
IDs used by the commands that talk to Roblox Open Cloud.

| Field | Type | Description |
| :--- | :--- | :--- |
| `PlaceId` | Integer | The place `chainlink publish-place` uploads to. `--place` overrides it. |
| `UserId` | Integer | The user who owns assets uploaded by `chainlink assets`. |
| `GroupId` | Integer | The group that owns uploaded assets. Takes precedence over `UserId`. |

```toml
[Roblox]
  PlaceId = 1234567890
  GroupId = 987654
```

## Asset Lock File

`chainlink assets` records every uploaded file in `assets.lock.json`, mapping its path under `assets/` to its asset ID and a hash of its content. A file is uploaded again only when its content changes, and entries for deleted files are dropped. Commit this file so everyone on the project shares the same IDs.

`serve`, `build` and `sourcemap` turn it into `ReplicatedStorage.assets`, a ModuleScript mapping each path to `"rbxassetid://<id>"`, which the `asset` macro reads.

## Authentication Store

ChainLink stores API tokens for various registries in a file located at `~/.chainlink/auth.toml`.

### How it Works
When you run `chainlink login`, ChainLink retrieves the registry's API endpoint from its index configuration. The token is then stored mapped to that API URL.

### Manual Management
The `auth.toml` file is a simple TOML file with a `[tokens]` map:
```toml
[tokens]
"https://api.wally.run" = "your-api-token-here"
"https://apis.roblox.com/" = "your-open-cloud-api-key"
```

### Roblox Open Cloud
`chainlink assets` and `chainlink publish-place` need an [Open Cloud API key](https://create.roblox.com/dashboard/credentials) set up with:

| Setting | Value |
| :--- | :--- |
| Access permissions | **Assets** API with Read and Write (`asset:read`, `asset:write`) |
| Experiences | The experience `publish-place` uploads to (`[Roblox] PlaceId`) |
| Owner | Create the key under the group if assets use `[Roblox] GroupId` |
| Accepted IP addresses | Your IP, or `0.0.0.0/0` to allow any (e.g. for CI) |

`chainlink login-cloud` prints this list too. Save the key once with:
```bash
chainlink login-cloud                  # prompts for the key
chainlink login-cloud --api-key <key>  # non-interactive
```

The key is taken from, in order:

1. the `--api-key` flag,
2. the `ROBLOX_API_KEY` environment variable (handy in CI),
3. `auth.toml`, saved by `chainlink login-cloud`.
