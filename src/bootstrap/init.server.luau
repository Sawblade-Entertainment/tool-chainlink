--!strict
--!native
--!optimize 2
-- Server Bootstrapper
-- Requires every child ModuleScript and drives its lifecycle:
--   :init()                              -- called once on every module before any :start()
--   :start()                             -- called once after every module has finished :init()
--   :step(time, dt)                      -- RunService.Stepped (before physics)
--   :beat(dt)                            -- RunService.Heartbeat (after physics)
--   :playerAdded(player)                 -- Players.PlayerAdded (+ players already in game)
--   :playerRemoving(player)              -- Players.PlayerRemoving
--   :characterAdded(player, character)   -- Player.CharacterAdded
--   :characterRemoving(player, character)-- Player.CharacterRemoving
--   :close()                             -- game:BindToClose (server shutdown, may yield)
--
-- Optional: a module can define a numeric `priority` field.
-- Lower numbers run first (default 0).

local Players = game:GetService("Players")
local RunService = game:GetService("RunService")

local LOG_PREFIX = "[ SERVER ]"
local CLOSE_TIMEOUT = 25 -- seconds; Roblox kills the server after ~30s

type Module = { [any]: any }
type Entry = { name: string, module: Module, priority: number }

local entries: { Entry } = {}

-- Require every child ModuleScript
for _, child in script:GetChildren() do
    if not child:IsA("ModuleScript") then
        continue
    end

    local ok, result = pcall(require, child)
    if not ok then
        warn(`{LOG_PREFIX} Failed to require {child.Name}: {result}`)
        continue
    end

    if type(result) ~= "table" then
        warn(`{LOG_PREFIX} {child.Name} did not return a table; skipping`)
        continue
    end

    table.insert(entries, {
        name = child.Name,
        module = result,
        priority = tonumber(result.priority) or 0,
    })
end

-- Sort by priority, then by name so the order is deterministic
table.sort(entries, function(a, b)
    if a.priority ~= b.priority then
        return a.priority < b.priority
    end
    return a.name < b.name
end)

-- Safely call a method on a module if it exists
local function call(entry: Entry, method: string, ...: any): boolean
    local fn = entry.module[method]
    if type(fn) ~= "function" then
        return true
    end

    local ok, err = pcall(fn, entry.module, ...)
    if not ok then
        warn(`{LOG_PREFIX} {entry.name}:{method} errored: {err}`)
    end
    return ok
end

-- Build a list of modules that implement a given method
local function collect(method: string): { Entry }
    local list = {}
    for _, entry in entries do
        if type(entry.module[method]) == "function" then
            table.insert(list, entry)
        end
    end
    return list
end

-- INIT: synchronous, in order. A failed init excludes that module from the rest.
local initialized: { Entry } = {}
for _, entry in entries do
    if call(entry, "init") then
        table.insert(initialized, entry)
    end
end
entries = initialized

-- START: each in its own thread so a yielding start doesn't block the others
for _, entry in entries do
    if type(entry.module.start) == "function" then
        task.spawn(call, entry, "start")
    end
end

-- Per-frame hooks
local stepList = collect("step")
local beatList = collect("beat")

if #stepList > 0 then
    RunService.Stepped:Connect(function(time: number, dt: number)
        for _, entry in stepList do
            call(entry, "step", time, dt)
        end
    end)
end

if #beatList > 0 then
    RunService.Heartbeat:Connect(function(dt: number)
        for _, entry in beatList do
            call(entry, "beat", dt)
        end
    end)
end

-- Players hooks
local playerAddedList = collect("playerAdded")
local playerRemovingList = collect("playerRemoving")
local characterAddedList = collect("characterAdded")
local characterRemovingList = collect("characterRemoving")
local closeList = collect("close")

-- Each handler runs in its own thread, so a yielding module (e.g. a DataStore load)
-- doesn't delay the other modules.
local function dispatch(list: { Entry }, method: string, ...: any)
    for _, entry in list do
        task.spawn(call, entry, method, ...)
    end
end

-- Tracks playerRemoving handlers still running, so shutdown can wait for them
local pendingRemovals = 0

local characterConnections: { [Player]: { RBXScriptConnection } } = {}

local function onPlayerAdded(player: Player)
    if characterConnections[player] then
        return -- already handled (guards against the existing-players loop overlapping)
    end
    characterConnections[player] = {}

    dispatch(playerAddedList, "playerAdded", player)

    if #characterAddedList > 0 then
        table.insert(characterConnections[player], player.CharacterAdded:Connect(function(character)
            dispatch(characterAddedList, "characterAdded", player, character)
        end))

        -- The character may have spawned before we connected
        if player.Character then
            dispatch(characterAddedList, "characterAdded", player, player.Character)
        end
    end

    if #characterRemovingList > 0 then
        table.insert(characterConnections[player], player.CharacterRemoving:Connect(function(character)
            dispatch(characterRemovingList, "characterRemoving", player, character)
        end))
    end
end

local function onPlayerRemoving(player: Player)
    for _, entry in playerRemovingList do
        pendingRemovals += 1
        task.spawn(function()
            call(entry, "playerRemoving", player)
            pendingRemovals -= 1
        end)
    end

    local connections = characterConnections[player]
    if connections then
        for _, connection in connections do
            connection:Disconnect()
        end
        characterConnections[player] = nil
    end
end

Players.PlayerAdded:Connect(onPlayerAdded)
Players.PlayerRemoving:Connect(onPlayerRemoving)

-- Catch players who joined before this script finished loading (common in Studio)
for _, player in Players:GetPlayers() do
    task.spawn(onPlayerAdded, player)
end

-- CLOSE: run every module's close in parallel, then wait for them
-- (and any in-flight playerRemoving handlers) to finish, up to a timeout.
game:BindToClose(function()
    local unfinished: { [string]: boolean } = {}

    for _, entry in closeList do
        unfinished[entry.name] = true
        task.spawn(function()
            call(entry, "close")
            unfinished[entry.name] = nil
        end)
    end

    local deadline = os.clock() + CLOSE_TIMEOUT
    while (next(unfinished) ~= nil or pendingRemovals > 0) and os.clock() < deadline do
        task.wait()
    end

    if next(unfinished) ~= nil then
        local names = {}
        for name in unfinished do
            table.insert(names, name)
        end
        warn(`{LOG_PREFIX} close timed out for: {table.concat(names, ", ")}`)
    end

    if pendingRemovals > 0 then
        warn(`{LOG_PREFIX} {pendingRemovals} playerRemoving handler(s) did not finish before shutdown`)
    end
end)

print(`{LOG_PREFIX} Loaded {#entries} module(s)`)