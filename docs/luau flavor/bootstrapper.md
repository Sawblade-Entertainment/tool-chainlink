# Bootstrapper

The bootstrapper is the entry point for both Client and Server environments in the Luau flavor. It automatically discovers and manages the lifecycle of `ModuleScript` children, allowing you to organize your game logic into modular "Services" (server-side) and "Controllers" (client-side).

## Core Lifecycle

Every module loaded by the bootstrapper can implement a set of lifecycle methods. The bootstrapper calls these methods in a specific order and context.

### Common Lifecycle (Client & Server)

| Method | Timing | Description |
| :--- | :--- | :--- |
| `:init()` | **Synchronous** | Called once on every module before any `:start()` calls. If `:init()` errors, the module is excluded from all subsequent lifecycle events. |
| `:start()` | **Asynchronous** | Called once after every module has successfully completed `:init()`. Each module's `:start()` runs in its own thread. |
| `:step(time, dt)` | **Per-Frame** | Called every `RunService.Stepped` (before physics). |
| `:beat(dt)` | **Per-Frame** | Called every `RunService.Heartbeat` (after physics). |

### Client-Only Lifecycle

| Method | Timing | Description |
| :--- | :--- | :--- |
| `:draw(dt)` | **Per-Frame** | Called every `RunService.RenderStepped` (before each frame renders). |
| `:input(input, gameProcessed, phase)` | **Event-Driven** | Called on `UserInputService.InputBegan`, `InputEnded` and `InputChanged`. `phase` is `"began"`, `"ended"` or `"changed"`. |

### Server-Only Lifecycle

| Method | Timing | Description |
| :--- | :--- | :--- |
| `:playerAdded(player)` | **Event-Driven** | Called when a player joins, including players already present when the bootstrapper starts. |
| `:playerRemoving(player)` | **Event-Driven** | Called when a player leaves the game. |
| `:characterAdded(player, character)` | **Event-Driven** | Called when a player's character spawns. |
| `:characterRemoving(player, character)` | **Event-Driven** | Called when a player's character is removed. |
| `:close()` | **Shutdown** | Called during `game:BindToClose`. Used for saving data or cleanup. The server will wait for these to complete (up to 25s). |

## Module Priority

Modules can define a numeric `priority` field to control the order in which `:init()` is called.

- **Lower numbers run first** (Default is `0`).
- If priorities are equal, modules are loaded alphabetically by name.

## Examples

### Example Controller (Client)

```luau
-- client/PlayerController.luau

-- set attributes here
mod.priority = 1 -- If you wish to specify run order

function mod:init()
    print("PlayerController initializing...")
end

function mod:start()
    print("PlayerController started!")
end

function mod:input(input: InputObject, gameProcessed: boolean, phase: string)
    if input.KeyCode == Enum.KeyCode.E and phase == "began" and not gameProcessed then
        print("Interact key pressed!")
    end
end
```

### Example Service (Server)

```luau
-- server/DataService.luau

-- set attributes here

function mod:init()
    -- Setup database connections etc.
    print("DataService initializing...")
end

function mod:playerAdded(player: Player)
    print(`Loading data for {player.Name}...`)
    -- Yielding is fine here; the bootstrapper spawns this in a thread
end

function mod:playerRemoving(player: Player)
    print(`Saving data for {player.Name}...`)
end

function mod:close()
    print("Shutting down DataService...")
end
```
