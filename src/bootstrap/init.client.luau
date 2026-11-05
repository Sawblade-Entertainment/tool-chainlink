--!strict
--!native
--!optimize 2
-- Client Bootstrapper
-- Requires every child ModuleScript and drives its lifecycle:
--   :init()                          -- called once on every module before any :start()
--   :start()                         -- called once after every module has finished :init()
--   :step(time, dt)                  -- RunService.Stepped (before physics)
--   :beat(dt)                        -- RunService.Heartbeat (after physics)
--   :draw(dt)                        -- RunService.RenderStepped (before each frame renders)
--   :input(input, gameProcessed, phase)  -- UserInputService Began / Ended / Changed
--       phase is "began" | "ended" | "changed"
--
-- Optional: a module can define a numeric `priority` field.
-- Lower numbers run first (default 0).

local RunService = game:GetService("RunService")
local UserInputService = game:GetService("UserInputService")

local LOG_PREFIX = "[ CLIENT ]"

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

-- Build a list of modules that implement a given method (avoids per-frame lookups)
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

-- Per-frame / event hooks
local stepList = collect("step")
local beatList = collect("beat")
local drawList = collect("draw")
local inputList = collect("input")

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

if #drawList > 0 then
    RunService.RenderStepped:Connect(function(dt: number)
        for _, entry in drawList do
            call(entry, "draw", dt)
        end
    end)
end

if #inputList > 0 then
    local function dispatch(phase: string)
        return function(input: InputObject, gameProcessed: boolean)
            for _, entry in inputList do
                call(entry, "input", input, gameProcessed, phase)
            end
        end
    end

    UserInputService.InputBegan:Connect(dispatch("began"))
    UserInputService.InputEnded:Connect(dispatch("ended"))
    UserInputService.InputChanged:Connect(dispatch("changed"))
end

print(`{LOG_PREFIX} Loaded {#entries} module(s)`)