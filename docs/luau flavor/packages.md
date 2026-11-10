# Packages

ChainLink provides a custom `use` implementation for the Luau flavor to simplify dependency management and package resolution.

## Import Syntax

All imports should be placed at the top of your script. The custom require uses a string-based pathing system:

```lua
-- Correct import format
RazorInput = use "razorinput"
CharacterController = use "./CharacterController"
```

## Path Resolution

The behavior of `use` depends on the prefix of the path provided:

### 1. Package Resolution (Lowercase)
If a path is all lowercase and does **not** start with `./`, `../`, `@` or `/`, `use` looks for it in `ReplicatedStorage.packages`. A name with any uppercase letter is treated as a service instead (see Best Practices).

- **Example**: `use "razorinput"` > Looks for `game.ReplicatedStorage.packages.razorinput`.
- This allows you to reference shared libraries and third-party packages without knowing their exact location in the DataModel.

### 2. ReplicatedStorage Paths
Paths starting with `/` resolve from `ReplicatedStorage`, waiting for each segment with `WaitForChild`.

- **Example**: `use "/shared/Config"` > Requires `game.ReplicatedStorage.shared.Config`.

### 3. Relative Resolution
Paths starting with `./` or `../` resolve relative to the current script's location in the hierarchy, similar to standard file system paths.

### 4. Absolute/Special Resolution
Paths starting with `@` are handled as special absolute paths (depending on project configuration).

## Best Practices

- **Top-of-File**: Always place your `use` calls at the very beginning of the script to ensure dependencies are loaded before any logic executes.
- **Naming**: Use lowercase for package names in `ReplicatedStorage.packages` to maintain consistency across the project.
- **Service**: Use uppercase for service names. Acts as a shortcut to game:GetService(). use "Players" or game:GetService("Players") whichever you prefer.
