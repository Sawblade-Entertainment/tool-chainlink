--!strict
--!optimize 2
-- ChainLink Studio sync plugin

local HttpService = game:GetService("HttpService")
local RunService = game:GetService("RunService")
local ScriptEditorService = game:GetService("ScriptEditorService")
local SerializationService = game:GetService("SerializationService")
local ChangeHistoryService = game:GetService("ChangeHistoryService")

-- Plugins also load into play-test DataModels. Syncing only makes sense in the edit session.
if not RunService:IsEdit() then
    return
end

local ATTR = "ChainLinkPath"
local SETTING_URL = "ChainLink_Url"
local SETTING_TOKEN = "ChainLink_Token"
local SETTING_LOGS = "ChainLink_ShowLogs"
local DEFAULT_URL = "http://localhost:8013"
local MAX_LOG_LINES = 200

-- ---------- State ----------

local running = false
-- Bumped on every connect and disconnect so a poll loop from an earlier
-- connection exits instead of running alongside the new one.
local generation = 0
local probing = false
local lastSeq = 0
local sessionId = ""
local instanceCache: { [string]: Instance } = {}

-- ---------- Theme ----------

local Studio = settings().Studio
local SGC = Enum.StudioStyleGuideColor
local SGM = Enum.StudioStyleGuideModifier

local FONT = Font.fromEnum(Enum.Font.BuilderSans)
local FONT_MEDIUM = Font.new(FONT.Family, Enum.FontWeight.Medium)
local FONT_MONO = Font.fromEnum(Enum.Font.RobotoMono)
local SUCCESS_COLOR = Color3.fromRGB(58, 177, 94)

local themeBindings: { () -> () } = {}

local function themed(fn: () -> ())
    table.insert(themeBindings, fn)
    fn()
end

local function color(c: Enum.StudioStyleGuideColor, m: Enum.StudioStyleGuideModifier?): Color3
    return Studio.Theme:GetColor(c, m or SGM.Default)
end

local themeConnection = Studio.ThemeChanged:Connect(function()
    for _, fn in themeBindings do
        fn()
    end
end)

-- ---------- UI ----------

local toolbar = plugin:CreateToolbar("ChainLink")
local toggleButton = toolbar:CreateButton("ChainLink", "Show the ChainLink sync panel", "rbxassetid://121842935231412")
toggleButton.ClickableWhenViewportHidden = true

local widget = plugin:CreateDockWidgetPluginGuiAsync(
    "ChainLinkWidget",
    DockWidgetPluginGuiInfo.new(Enum.InitialDockState.Right, false, false, 280, 240, 220, 180)
)
widget.Title = "ChainLink"

local function make(class: string, props: { [string]: any }, parent: Instance?): any
    local inst = Instance.new(class)
    for k, v in props do
        (inst :: any)[k] = v
    end
    inst.Parent = parent
    return inst
end

local function textLabel(props: { [string]: any }, parent: Instance): TextLabel
    props.BackgroundTransparency = 1
    props.FontFace = props.FontFace or FONT
    props.TextSize = props.TextSize or 14
    props.TextXAlignment = props.TextXAlignment or Enum.TextXAlignment.Left
    return make("TextLabel", props, parent)
end

local function corner(inst: GuiObject)
    make("UICorner", { CornerRadius = UDim.new(0, 4) }, inst)
end

local function stroke(inst: GuiObject): UIStroke
    return make("UIStroke", { Thickness = 1, ApplyStrokeMode = Enum.ApplyStrokeMode.Border }, inst)
end

local PADDING_TOP = 12
local PADDING_BOTTOM = 10
local GAP = 12
local LOG_MIN_HEIGHT = 120

-- The panel scrolls when the widget is too short to fit the controls plus the
-- open log at its minimum height. updateLayout sizes root to fit.
local scroller = make("ScrollingFrame", {
    Size = UDim2.fromScale(1, 1),
    BorderSizePixel = 0,
    CanvasSize = UDim2.new(),
    ScrollingDirection = Enum.ScrollingDirection.Y,
    ScrollBarThickness = 6,
    VerticalScrollBarInset = Enum.ScrollBarInset.ScrollBar,
}, widget)
themed(function()
    scroller.BackgroundColor3 = color(SGC.MainBackground)
    scroller.ScrollBarImageColor3 = color(SGC.ScrollBar)
end)

local root = make("Frame", { Size = UDim2.fromScale(1, 1), BackgroundTransparency = 1 }, scroller)
make("UIPadding", {
    PaddingTop = UDim.new(0, PADDING_TOP),
    PaddingBottom = UDim.new(0, PADDING_BOTTOM),
    PaddingLeft = UDim.new(0, 12),
    PaddingRight = UDim.new(0, 12),
}, root)
make("UIListLayout", { Padding = UDim.new(0, GAP), SortOrder = Enum.SortOrder.LayoutOrder }, root)

-- Status: a colored dot, a short title, and a line of detail underneath.
local statusBlock = make("Frame", {
    Size = UDim2.fromScale(1, 0),
    AutomaticSize = Enum.AutomaticSize.Y,
    BackgroundTransparency = 1,
    LayoutOrder = 1,
}, root)
local statusDot = make(
    "Frame",
    { Size = UDim2.fromOffset(8, 8), Position = UDim2.fromOffset(0, 6), BorderSizePixel = 0 },
    statusBlock
)
make("UICorner", { CornerRadius = UDim.new(1, 0) }, statusDot)
local statusTitle = textLabel({
    Size = UDim2.new(1, -16, 0, 20),
    Position = UDim2.fromOffset(16, 0),
    FontFace = FONT_MEDIUM,
    TextTruncate = Enum.TextTruncate.AtEnd,
}, statusBlock)
local statusDetail = textLabel({
    Size = UDim2.new(1, -16, 0, 0),
    Position = UDim2.fromOffset(16, 20),
    AutomaticSize = Enum.AutomaticSize.Y,
    TextSize = 13,
    TextWrapped = true,
}, statusBlock)

-- Labeled text field, styled like Studio's own inputs.
local function field(labelText: string, placeholder: string, value: string, order: number): (Frame, TextBox)
    local row = make("Frame", { Size = UDim2.new(1, 0, 0, 48), BackgroundTransparency = 1, LayoutOrder = order }, root)
    local label = textLabel({ Size = UDim2.new(1, 0, 0, 18), Text = labelText }, row)
    local box = make("TextBox", {
        Size = UDim2.new(1, 0, 0, 28),
        Position = UDim2.fromOffset(0, 20),
        BorderSizePixel = 0,
        Text = value,
        PlaceholderText = placeholder,
        ClearTextOnFocus = false,
        FontFace = FONT,
        TextSize = 14,
        TextXAlignment = Enum.TextXAlignment.Left,
    }, row)
    corner(box)
    make("UIPadding", { PaddingLeft = UDim.new(0, 8), PaddingRight = UDim.new(0, 8) }, box)
    local border = stroke(box)
    local hovered = false

    local function restyle()
        local mod = if not box.TextEditable
            then SGM.Disabled
            elseif box:IsFocused() then SGM.Selected
            elseif hovered then SGM.Hover
            else SGM.Default
        label.TextColor3 = color(SGC.MainText)
        box.BackgroundColor3 = color(SGC.InputFieldBackground, mod)
        box.TextColor3 = color(if box.TextEditable then SGC.MainText else SGC.DimmedText)
        box.PlaceholderColor3 = color(SGC.DimmedText)
        border.Color = color(SGC.InputFieldBorder, mod)
    end
    themed(restyle)
    box.Focused:Connect(restyle)
    box.FocusLost:Connect(restyle)
    box:GetPropertyChangedSignal("TextEditable"):Connect(restyle)
    box.MouseEnter:Connect(function()
        hovered = true
        restyle()
    end)
    box.MouseLeave:Connect(function()
        hovered = false
        restyle()
    end)
    return row, box
end

local _, urlBox = field("Server URL", DEFAULT_URL, plugin:GetSetting(SETTING_URL) or DEFAULT_URL, 2)
-- The token is only needed when chainlink.json sets one, so the field stays
-- hidden until a token is saved or the server asks for one.
local tokenRow, tokenBox = field("Token", "From chainlink.json", plugin:GetSetting(SETTING_TOKEN) or "", 3)
tokenRow.Visible = tokenBox.Text ~= ""

local connectButton = make("TextButton", {
    Size = UDim2.new(1, 0, 0, 28),
    BorderSizePixel = 0,
    Text = "Connect",
    FontFace = FONT_MEDIUM,
    TextSize = 14,
    AutoButtonColor = false,
    LayoutOrder = 4,
}, root)
corner(connectButton)
local connectStroke = stroke(connectButton)
local connectHover = false
local connectPressed = false

-- Connect is the primary action; Disconnect uses the quieter secondary style.
local function styleConnect()
    local mod = if connectPressed then SGM.Pressed elseif connectHover then SGM.Hover else SGM.Default
    if running then
        connectButton.BackgroundColor3 = color(SGC.DialogButton, mod)
        connectButton.TextColor3 = color(SGC.DialogButtonText, mod)
        connectStroke.Color = color(SGC.DialogButtonBorder, mod)
    else
        connectButton.BackgroundColor3 = color(SGC.DialogMainButton, mod)
        connectButton.TextColor3 = color(SGC.DialogMainButtonText, mod)
        connectStroke.Color = color(SGC.DialogMainButton, mod)
    end
end
themed(styleConnect)
connectButton.MouseEnter:Connect(function()
    connectHover = true
    styleConnect()
end)
connectButton.MouseLeave:Connect(function()
    connectHover = false
    connectPressed = false
    styleConnect()
end)
connectButton.MouseButton1Down:Connect(function()
    connectPressed = true
    styleConnect()
end)
connectButton.MouseButton1Up:Connect(function()
    connectPressed = false
    styleConnect()
end)

-- Pushes the sync log to the bottom of the panel while it is collapsed.
local spacer = make("Frame", { Size = UDim2.fromScale(1, 0), BackgroundTransparency = 1, LayoutOrder = 10 }, root)
make("UIFlexItem", { FlexMode = Enum.UIFlexMode.Fill }, spacer)

-- Collapsed-by-default sync log. Warnings and errors also go to Studio's Output.
local showLogs = plugin:GetSetting(SETTING_LOGS) == true

local logHeader = make("TextButton", {
    Size = UDim2.new(1, 0, 0, 20),
    BackgroundTransparency = 1,
    Text = "",
    AutoButtonColor = false,
    LayoutOrder = 11,
}, root)
local logChevron = textLabel({
    Size = UDim2.fromOffset(12, 20),
    Text = ">",
    TextSize = 12,
    TextXAlignment = Enum.TextXAlignment.Center,
}, logHeader)
local logTitle = textLabel({
    Size = UDim2.new(1, -16, 1, 0),
    Position = UDim2.fromOffset(16, 0),
    Text = "Sync log",
    TextSize = 13,
}, logHeader)

local logFrame = make("ScrollingFrame", {
    Size = UDim2.fromScale(1, 0),
    BorderSizePixel = 0,
    CanvasSize = UDim2.new(),
    AutomaticCanvasSize = Enum.AutomaticSize.Y,
    ScrollingDirection = Enum.ScrollingDirection.Y,
    ScrollBarThickness = 6,
    LayoutOrder = 12,
}, root)
make("UIFlexItem", { FlexMode = Enum.UIFlexMode.Fill }, logFrame)
make("UIListLayout", { SortOrder = Enum.SortOrder.LayoutOrder }, logFrame)
make("UIPadding", {
    PaddingTop = UDim.new(0, 4),
    PaddingBottom = UDim.new(0, 4),
    PaddingLeft = UDim.new(0, 6),
    PaddingRight = UDim.new(0, 6),
}, logFrame)
corner(logFrame)
local logStroke = stroke(logFrame)

-- updateLayout gives the fill items (spacer or log) whatever height the widget
-- has left, but never less than LOG_MIN_HEIGHT for the open log. Anything
-- taller than the widget scrolls.
local function updateLayout()
    local used = PADDING_TOP + PADDING_BOTTOM
    local visible = 0
    for _, child in root:GetChildren() do
        if child:IsA("GuiObject") and child.Visible then
            visible += 1
            if child ~= spacer and child ~= logFrame then
                used += child.AbsoluteSize.Y
            end
        end
    end
    used += GAP * math.max(visible - 1, 0)
    local height = math.max(scroller.AbsoluteSize.Y, used + (if showLogs then LOG_MIN_HEIGHT else 0))
    root.Size = UDim2.new(1, 0, 0, height)
    scroller.CanvasSize = UDim2.fromOffset(0, height)
end
scroller:GetPropertyChangedSignal("AbsoluteSize"):Connect(updateLayout)
statusBlock:GetPropertyChangedSignal("AbsoluteSize"):Connect(updateLayout)
tokenRow:GetPropertyChangedSignal("Visible"):Connect(updateLayout)

local logHover = false
local function styleLogSection()
    local headerColor = color(if logHover then SGC.MainText else SGC.DimmedText)
    logChevron.TextColor3 = headerColor
    logTitle.TextColor3 = headerColor
    logChevron.Rotation = if showLogs then 90 else 0
    logFrame.Visible = showLogs
    spacer.Visible = not showLogs
    logFrame.BackgroundColor3 = color(SGC.InputFieldBackground)
    logFrame.ScrollBarImageColor3 = color(SGC.ScrollBar)
    logStroke.Color = color(SGC.Border)
end
themed(styleLogSection)
logHeader.MouseEnter:Connect(function()
    logHover = true
    styleLogSection()
end)
logHeader.MouseLeave:Connect(function()
    logHover = false
    styleLogSection()
end)
logHeader.MouseButton1Click:Connect(function()
    showLogs = not showLogs
    plugin:SetSetting(SETTING_LOGS, showLogs)
    styleLogSection()
    updateLayout()
    if showLogs then
        -- Bring the newly opened log into view when the panel has to scroll.
        task.defer(function()
            scroller.CanvasPosition = Vector2.new(0, scroller.AbsoluteCanvasSize.Y)
        end)
    end
end)
updateLayout()

-- ---------- Logging ----------

type LogLevel = "info" | "ok" | "warn" | "error" | "dim"

local function levelColor(level: LogLevel): Color3
    if level == "ok" then
        return SUCCESS_COLOR
    elseif level == "warn" then
        return color(SGC.WarningText)
    elseif level == "error" then
        return color(SGC.ErrorText)
    elseif level == "dim" then
        return color(SGC.DimmedText)
    end
    return color(SGC.MainText)
end

local logLines: { TextLabel } = {}
local logOrder = 0

themed(function()
    for _, line in logLines do
        line.TextColor3 = levelColor(line:GetAttribute("Level") :: LogLevel)
    end
end)

local function scrollLogToEnd()
    logFrame.CanvasPosition = Vector2.new(0, logFrame.AbsoluteCanvasSize.Y)
end

local function logLine(msg: string, level: LogLevel?)
    local lvl: LogLevel = level or "info"
    if lvl == "warn" or lvl == "error" then
        warn("[ChainLink] " .. msg)
    end
    logOrder += 1
    local line = textLabel({
        Size = UDim2.new(1, 0, 0, 16),
        Text = msg,
        LayoutOrder = logOrder,
        FontFace = FONT_MONO,
        TextSize = 12,
        TextTruncate = Enum.TextTruncate.AtEnd,
        TextColor3 = levelColor(lvl),
    }, logFrame)
    line:SetAttribute("Level", lvl)
    table.insert(logLines, line)
    if #logLines > MAX_LOG_LINES then
        table.remove(logLines, 1):Destroy()
    end
    if showLogs then
        -- Wait for the automatic canvas size to include the new line.
        task.defer(scrollLogToEnd)
    end
end

-- ---------- Status ----------

type Tone = "idle" | "found" | "busy" | "connected" | "error"
local statusTone: Tone = "idle"

local function toneColor(tone: Tone): Color3
    if tone == "connected" then
        return SUCCESS_COLOR
    elseif tone == "found" then
        return color(SGC.DialogMainButton)
    elseif tone == "busy" then
        return color(SGC.WarningText)
    elseif tone == "error" then
        return color(SGC.ErrorText)
    end
    return color(SGC.DimmedText)
end

themed(function()
    statusDot.BackgroundColor3 = toneColor(statusTone)
    statusTitle.TextColor3 = color(SGC.MainText)
    statusDetail.TextColor3 = color(SGC.DimmedText)
end)

local function setStatus(tone: Tone, title: string, detail: string)
    statusTone = tone
    statusDot.BackgroundColor3 = toneColor(tone)
    statusTitle.Text = title
    statusDetail.Text = detail
    connectButton.Text = if running then "Disconnect" else "Connect"
    urlBox.TextEditable = not running
    tokenBox.TextEditable = not running
    styleConnect()
end

local IDLE_DETAIL = "Run chainlink serve in your project, then connect."

-- ---------- Instance resolution ----------

local function resolveTarget(target: string): Instance?
    local parts = string.split(target, ".")
    local ok, current = pcall(function()
        return game:GetService(parts[1])
    end)
    if not ok or not current then
        return nil
    end
    for i = 2, #parts do
        local child = current:FindFirstChild(parts[i])
        if not child then
            child = Instance.new("Folder")
            child.Name = parts[i]
            child.Parent = current
        end
        current = child
    end
    return current
end

-- keyFor names the instance at segments[1..count] under target.
local function keyFor(target: string, segments: { string }, count: number?): string
    return target .. "/" .. table.concat(segments, "/", 1, count or #segments)
end

local function findInstance(target: string, segments: { string }): Instance?
    local key = keyFor(target, segments)
    local cached = instanceCache[key]
    if cached and cached.Parent then
        return cached
    end
    instanceCache[key] = nil

    local current = resolveTarget(target)
    if not current then
        return nil
    end
    for _, seg in segments do
        current = current:FindFirstChild(seg)
        if not current then
            return nil
        end
    end
    instanceCache[key] = current
    return current
end

local function ensureParent(target: string, segments: { string }): Instance?
    local current = resolveTarget(target)
    if not current then
        return nil
    end
    for i = 1, #segments - 1 do
        local seg = segments[i]
        local key = keyFor(target, segments, i)
        local child = current:FindFirstChild(seg)
        if not child then
            child = Instance.new("Folder")
            child.Name = seg
            child:SetAttribute(ATTR, key)
            child.Parent = current
        end
        instanceCache[key] = child
        current = child
    end
    return current
end

-- replaceInstance swaps old for a new instance of newClass, keeping its name,
-- attributes and children. With syncedOnly, only children synced from other
-- files move; the rest came from a model file and go away with it.
local function replaceInstance(old: Instance, newClass: string, syncedOnly: boolean?): Instance
    local new = Instance.new(newClass)
    new.Name = old.Name
    for attr, value in old:GetAttributes() do
        new:SetAttribute(attr, value)
    end
    for _, child in old:GetChildren() do
        if not syncedOnly or child:GetAttribute(ATTR) ~= nil then
            child.Parent = new
        end
    end
    new.Parent = old.Parent
    old:Destroy()
    return new
end

local BASE64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
local BASE64_VALUES: { [number]: number } = {}
for i = 1, #BASE64 do
    BASE64_VALUES[string.byte(BASE64, i)] = i - 1
end
local PAD = string.byte("=")

local function decodeBase64(data: string): buffer
    local len = #data
    if len % 4 ~= 0 then
        error("invalid base64 model payload")
    end
    local padding = 0
    if len > 0 and string.byte(data, len) == PAD then
        padding += 1
        if string.byte(data, len - 1) == PAD then
            padding += 1
        end
    end
    local out = buffer.create(len // 4 * 3 - padding)
    local o = 0
    for i = 1, len, 4 do
        local a, b, c, d = string.byte(data, i, i + 3)
        local va, vb = BASE64_VALUES[a], BASE64_VALUES[b]
        local vc = if c == PAD then 0 else BASE64_VALUES[c]
        local vd = if d == PAD then 0 else BASE64_VALUES[d]
        if not va or not vb or not vc or not vd then
            error("invalid base64 model payload")
        end
        local n = va * 262144 + vb * 4096 + vc * 64 + vd
        buffer.writeu8(out, o, n // 65536)
        o += 1
        if c ~= PAD then
            buffer.writeu8(out, o, (n // 256) % 256)
            o += 1
        end
        if d ~= PAD then
            buffer.writeu8(out, o, n % 256)
            o += 1
        end
    end
    return out
end

local function isModelPath(path: string): boolean
    return string.match(string.lower(path), "%.rbxmx?$") ~= nil
end

-- importModel replaces the instance called name under parent with the contents
-- of a .rbxm or .rbxmx file. Studio decodes both formats itself, so every
-- property type loads exactly as it would from Studio's own files.
local function importModel(parent: Instance, name: string, key: string, data: string): Instance?
    local items = SerializationService:DeserializeInstancesAsync(decodeBase64(data))
    local synced: Instance
    if #items == 1 then
        synced = items[1]
    elseif #items == 0 then
        logLine(name .. " has no instances; skipping", "warn")
        return nil
    else
        logLine(name .. " has several top-level instances; wrapping them in a Folder", "warn")
        synced = Instance.new("Folder")
        for _, item in items do
            item.Parent = synced
        end
    end
    -- Named after the file, like every other synced instance.
    synced.Name = name
    -- A model saved from a synced place carries stale sync attributes that
    -- would make a full sync prune its contents.
    for _, d in synced:GetDescendants() do
        if d:GetAttribute(ATTR) ~= nil then
            d:SetAttribute(ATTR, nil)
        end
    end
    synced:SetAttribute(ATTR, key)

    local existing = parent:FindFirstChild(name)
    if existing then
        -- Keep instances synced from other files: the contents of a Foo/ folder
        -- next to Foo.rbxmx, or the siblings of an init.rbxmx.
        for _, child in existing:GetChildren() do
            if child:GetAttribute(ATTR) ~= nil then
                child.Parent = synced
            end
        end
        existing:Destroy()
    end
    synced.Parent = parent
    return synced
end

local function setSource(inst: Instance, ev)
    if inst:IsA("LuaSourceContainer") then
        local source = ev.source or ""
        local ok = pcall(function()
            ScriptEditorService:UpdateSourceAsync(inst, function()
                return source
            end)
        end)
        if not ok then
            (inst :: any).Source = source
        end
    elseif inst:IsA("StringValue") then
        inst.Value = ev.source or ""
    end
end

-- ---------- Applying events ----------

local function applyCreateOrUpdate(ev)
    local segments = ev.instancePath
    if #segments == 0 then
        return
    end
    local key = keyFor(ev.target, segments)

    local parent = ensureParent(ev.target, segments)
    if not parent then
        logLine("Target not found: " .. ev.target, "warn")
        return
    end
    local name = segments[#segments]

    -- Model files replace the instance outright with whatever the file holds.
    if ev.modelFormat then
        local ok, synced = pcall(importModel, parent, name, key, ev.modelData)
        if not ok then
            logLine("Couldn't import " .. ev.path .. ": " .. tostring(synced), "error")
        elseif synced then
            instanceCache[key] = synced
        end
        return
    end

    local inst = parent:FindFirstChild(name)
    if inst and inst.ClassName ~= ev.className then
        inst = replaceInstance(inst, ev.className)
    elseif not inst then
        inst = Instance.new(ev.className)
        inst.Name = name
        inst.Parent = parent
    end
    inst:SetAttribute(ATTR, key)
    instanceCache[key] = inst
    local ok, err = pcall(setSource, inst, ev)
    if not ok then
        logLine("Couldn't update " .. ev.path .. ": " .. tostring(err), "error")
    end
end

local function applyDelete(ev)
    local key = keyFor(ev.target, ev.instancePath)
    if #ev.instancePath == 0 then
        logLine("Skipping delete of root target: " .. ev.target, "warn")
        return
    end
    local inst = findInstance(ev.target, ev.instancePath)
    instanceCache[key] = nil
    if not inst then
        return
    end
    if inst:GetAttribute(ATTR) ~= key then
        logLine("Skipping delete of unmanaged " .. inst:GetFullName(), "warn")
        return
    end
    if ev.keepChildren and #inst:GetChildren() > 0 then
        instanceCache[key] = replaceInstance(inst, "Folder", isModelPath(ev.path))
    else
        inst:Destroy()
    end
end

local function pruneStale(validKeys: { [string]: boolean })
    for _, inst in game:GetDescendants() do
        local key = inst:GetAttribute(ATTR)
        -- Parent is nil once an ancestor earlier in the list was destroyed.
        if type(key) ~= "string" or validKeys[key] or not inst.Parent then
            continue
        end
        local keepAsFolder = false
        if inst:IsA("Folder") then
            for _, d in inst:GetDescendants() do
                local dk = d:GetAttribute(ATTR)
                if type(dk) == "string" and validKeys[dk] then
                    keepAsFolder = true
                    break
                end
            end
        end
        if not keepAsFolder then
            inst:Destroy()
        end
    end
    table.clear(instanceCache)
end

local function applyBatch(resp)
    local events = resp.events or {}
    if #events == 0 and not resp.full then
        return
    end

    local recording = ChangeHistoryService:TryBeginRecording("ChainLink sync")
    local ok, err = pcall(function()
        if resp.full then
            local valid = {}
            for _, ev in events do
                for i = 1, #ev.instancePath do
                    valid[keyFor(ev.target, ev.instancePath, i)] = true
                end
            end
            pruneStale(valid)
        end
        for _, ev in events do
            if ev.op == "DELETE" then
                applyDelete(ev)
            else
                applyCreateOrUpdate(ev)
            end
            if not resp.full then
                logLine(string.format("%-6s %s", ev.op, ev.path))
            end
        end
        if resp.full then
            logLine(string.format("Synced %d files", #events), "ok")
        end
    end)
    if recording then
        ChangeHistoryService:FinishRecording(
            recording,
            if ok then Enum.FinishRecordingOperation.Commit else Enum.FinishRecordingOperation.Cancel
        )
    end
    if not ok then
        logLine("Sync failed: " .. tostring(err), "error")
    end
end

-- ---------- Connection ----------

-- serverUrl reads the URL field, adding a scheme and dropping trailing slashes.
local function serverUrl(): string
    local url = string.match(urlBox.Text, "^%s*(.-)/*%s*$") or ""
    if url == "" then
        return DEFAULT_URL
    end
    if not string.find(url, "^%a[%w+.-]*://") then
        url = "http://" .. url
    end
    return url
end

local function hostOf(url: string): string
    return (string.gsub(url, "^%a[%w+.-]*://", ""))
end

-- httpGet returns the status code and, on success, the decoded JSON body.
-- It throws when the request never reaches a server.
local function httpGet(url: string, token: string): (number, any)
    local headers = {}
    if token ~= "" then
        headers["X-ChainLink-Token"] = token
    end
    local res = HttpService:RequestAsync({ Url = url, Method = "GET", Headers = headers })
    if not res.Success then
        return res.StatusCode, nil
    end
    return res.StatusCode, HttpService:JSONDecode(res.Body)
end

local function stop(tone: Tone, title: string, detail: string)
    generation += 1
    running = false
    setStatus(tone, title, detail)
end

local function needToken(message: string)
    tokenRow.Visible = true
    stop("error", "Token required", message)
end

local function pollLoop(gen: number, baseUrl: string, token: string)
    local host = hostOf(baseUrl)
    local connected = false
    local backoff = 0
    while gen == generation do
        local url = string.format("%s/changes?since=%d&session=%s", baseUrl, lastSeq, HttpService:UrlEncode(sessionId))
        local ok, code, body = pcall(httpGet, url, token)
        if gen ~= generation then
            return
        end

        if ok and code == 200 then
            if not connected then
                logLine("Connected to " .. host, "ok")
            elseif backoff > 0 then
                logLine("Reconnected", "ok")
            end
            connected = true
            backoff = 0
            setStatus("connected", "Connected", "Syncing with " .. host)
            sessionId = body.sessionId
            applyBatch(body)
            lastSeq = body.seq
            continue
        end

        if ok and code == 401 then
            logLine("Server rejected the token", "error")
            needToken("The server rejected the token. Copy it from chainlink.json.")
            return
        end

        local msg = if ok then string.format("HTTP %d", code) else tostring(code)
        if string.find(msg, "not enabled", 1, true) then
            logLine("HTTP requests are disabled", "error")
            stop("error", "HTTP requests are off", "Turn on Game Settings > Security > Allow HTTP Requests.")
            return
        end
        if not connected then
            logLine("Couldn't connect to " .. host .. ": " .. msg, "error")
            stop(
                "error",
                "Couldn't connect",
                if ok then host .. " answered " .. msg .. "." else "No server at " .. host .. ". " .. IDLE_DETAIL
            )
            return
        end

        if backoff == 0 then
            logLine("Connection lost: " .. msg, "warn")
        end
        if string.find(msg, "exceeded limit", 1, true) then
            backoff = math.clamp(backoff * 2, 5, 30)
            setStatus("busy", "Rate limited", "Roblox's HTTP limit was reached. Retrying in " .. backoff .. "s.")
        else
            backoff = math.clamp(backoff * 2, 1, 10)
            setStatus(
                "busy",
                "Reconnecting…",
                "Lost the connection to " .. host .. ". Retrying in " .. backoff .. "s."
            )
        end
        task.wait(backoff)
    end
end

local function connect()
    local baseUrl = serverUrl()
    local token = tokenBox.Text
    urlBox.Text = baseUrl
    plugin:SetSetting(SETTING_URL, baseUrl)
    plugin:SetSetting(SETTING_TOKEN, token)

    generation += 1
    running = true
    lastSeq = 0
    sessionId = ""
    setStatus("busy", "Connecting…", hostOf(baseUrl))
    logLine("Connecting to " .. baseUrl, "dim")
    task.spawn(pollLoop, generation, baseUrl, token)
end

local function disconnect()
    stop("idle", "Disconnected", "Synced instances stay in place. Connect to resume syncing.")
    logLine("Disconnected", "dim")
end

-- probe checks for a running server without connecting. With reveal set it
-- opens the panel so the user sees the prompt to connect.
local function probe(reveal: boolean)
    if running or probing then
        return
    end
    probing = true
    local gen = generation
    local baseUrl = serverUrl()
    local ok, code = pcall(httpGet, baseUrl .. "/health", tokenBox.Text)
    -- A connect or disconnect while the probe was out owns the status now.
    if gen == generation then
        local host = hostOf(baseUrl)
        if ok and code == 200 then
            setStatus(
                "found",
                "Server found",
                "chainlink serve is running at " .. host .. ". Connect to start syncing."
            )
        elseif ok and code == 401 then
            tokenRow.Visible = true
            setStatus("found", "Server found", "The server at " .. host .. " needs a token. Enter it, then connect.")
        else
            setStatus("idle", "Not connected", IDLE_DETAIL)
        end
        if reveal and ok and (code == 200 or code == 401) then
            widget.Enabled = true
        end
    end
    probing = false
end

connectButton.MouseButton1Click:Connect(function()
    if running then
        disconnect()
    else
        connect()
    end
end)

urlBox.FocusLost:Connect(function()
    if running then
        return
    end
    urlBox.Text = serverUrl()
    plugin:SetSetting(SETTING_URL, urlBox.Text)
    task.spawn(probe, false)
end)

tokenBox.FocusLost:Connect(function()
    plugin:SetSetting(SETTING_TOKEN, tokenBox.Text)
end)

toggleButton.Click:Connect(function()
    widget.Enabled = not widget.Enabled
end)

widget:GetPropertyChangedSignal("Enabled"):Connect(function()
    toggleButton:SetActive(widget.Enabled)
    if widget.Enabled then
        task.spawn(probe, false)
    end
end)

plugin.Unloading:Connect(function()
    generation += 1
    running = false
    themeConnection:Disconnect()
end)

toggleButton:SetActive(widget.Enabled)
setStatus("idle", "Not connected", IDLE_DETAIL)
task.spawn(probe, true)
