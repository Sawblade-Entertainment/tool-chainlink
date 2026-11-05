--!strict
--!optimize 2
-- ChainLink Studio sync plugin

local HttpService = game:GetService("HttpService")
local ScriptEditorService = game:GetService("ScriptEditorService")
local SerializationService = game:GetService("SerializationService")
local ChangeHistoryService = game:GetService("ChangeHistoryService")

local ATTR = "ChainLinkPath"
local SETTING_URL = "ChainLink_Url"
local SETTING_TOKEN = "ChainLink_Token"
local SETTING_LOGS = "ChainLink_ShowLogs"
local DEFAULT_URL = "http://localhost:8013"

-- ---------- State ----------

local running = false
local lastSeq = 0
local sessionId = ""
local instanceCache = {}

-- ---------- Theme ----------

local Studio = settings().Studio
local SGC = Enum.StudioStyleGuideColor
local SGM = Enum.StudioStyleGuideModifier

local themeBindings: { () -> () } = {}

local function themed(fn: () -> ())
    table.insert(themeBindings, fn)
    fn()
end

local function color(c: Enum.StudioStyleGuideColor, m: Enum.StudioStyleGuideModifier?): Color3
    return Studio.Theme:GetColor(c, m or SGM.Default)
end

Studio.ThemeChanged:Connect(function()
    for _, fn in themeBindings do
        fn()
    end
end)

-- ---------- UI ----------

local toolbar = plugin:CreateToolbar("ChainLink")
local toggleButton = toolbar:CreateButton("ChainLink", "Open ChainLink sync panel", "rbxassetid://4458901886")
toggleButton.Icon = "rbxassetid://121842935231412"

local widget = plugin:CreateDockWidgetPluginGuiAsync(
    "ChainLinkWidget",
    DockWidgetPluginGuiInfo.new(Enum.InitialDockState.Right, false, false, 300, 400, 220, 180)
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

local function addBorder(inst: GuiObject, colorKey: Enum.StudioStyleGuideColor)
    local stroke = make("UIStroke", { Thickness = 1, ApplyStrokeMode = Enum.ApplyStrokeMode.Border }, inst)
    themed(function()
        stroke.Color = color(colorKey)
    end)
    return stroke
end

local root = make("Frame", { Size = UDim2.fromScale(1, 1), BorderSizePixel = 0 }, widget)
themed(function()
    root.BackgroundColor3 = color(SGC.MainBackground)
end)
make("UIPadding", {
    PaddingTop = UDim.new(0, 8),
    PaddingBottom = UDim.new(0, 8),
    PaddingLeft = UDim.new(0, 8),
    PaddingRight = UDim.new(0, 8),
}, root)
make("UIListLayout", { Padding = UDim.new(0, 6), SortOrder = Enum.SortOrder.LayoutOrder }, root)

-- Status row
local statusRow = make("Frame", { Size = UDim2.new(1, 0, 0, 20), BackgroundTransparency = 1, LayoutOrder = 1 }, root)
local statusDot = make("Frame", {
    Size = UDim2.fromOffset(8, 8),
    Position = UDim2.fromOffset(2, 6),
    BorderSizePixel = 0,
}, statusRow)
make("UICorner", { CornerRadius = UDim.new(1, 0) }, statusDot)
local statusLabel = make("TextLabel", {
    Size = UDim2.new(1, -16, 1, 0),
    Position = UDim2.fromOffset(16, 0),
    BackgroundTransparency = 1,
    Text = "Disconnected",
    TextXAlignment = Enum.TextXAlignment.Left,
    Font = Enum.Font.SourceSans,
    TextSize = 14,
    TextTruncate = Enum.TextTruncate.AtEnd,
}, statusRow)
themed(function()
    statusLabel.TextColor3 = color(SGC.MainText)
end)

-- Labeled text field (Studio properties-panel style)
local function field(labelText: string, placeholder: string, value: string, order: number): TextBox
    local row = make("Frame", { Size = UDim2.new(1, 0, 0, 40), BackgroundTransparency = 1, LayoutOrder = order }, root)
    local label = make("TextLabel", {
        Size = UDim2.new(1, 0, 0, 16),
        BackgroundTransparency = 1,
        Text = labelText,
        Font = Enum.Font.SourceSans,
        TextSize = 14,
        TextXAlignment = Enum.TextXAlignment.Left,
    }, row)
    local box = make("TextBox", {
        Size = UDim2.new(1, 0, 0, 22),
        Position = UDim2.fromOffset(0, 18),
        BorderSizePixel = 0,
        Text = value,
        PlaceholderText = placeholder,
        ClearTextOnFocus = false,
        Font = Enum.Font.SourceSans,
        TextSize = 14,
        TextXAlignment = Enum.TextXAlignment.Left,
    }, row)
    make("UIPadding", { PaddingLeft = UDim.new(0, 6), PaddingRight = UDim.new(0, 6) }, box)
    local stroke = addBorder(box, SGC.InputFieldBorder)

    local function restyle()
        local focused = box:IsFocused()
        label.TextColor3 = color(SGC.MainText)
        box.BackgroundColor3 = color(SGC.InputFieldBackground)
        box.TextColor3 = color(SGC.MainText)
        box.PlaceholderColor3 = color(SGC.DimmedText)
        stroke.Color = color(SGC.InputFieldBorder, focused and SGM.Selected or SGM.Default)
    end
    themed(restyle)
    box.Focused:Connect(restyle)
    box.FocusLost:Connect(restyle)
    return box
end

local urlBox = field("Server URL", "http://localhost:8080", plugin:GetSetting(SETTING_URL) or DEFAULT_URL, 2)
local tokenBox = field("Token", "optional", plugin:GetSetting(SETTING_TOKEN) or "", 3)

-- Connect button
local connectButton = make("TextButton", {
    Size = UDim2.new(1, 0, 0, 26),
    BorderSizePixel = 0,
    Text = "Connect",
    Font = Enum.Font.SourceSans,
    TextSize = 14,
    AutoButtonColor = false,
    LayoutOrder = 4,
}, root)
make("UICorner", { CornerRadius = UDim.new(0, 3) }, connectButton)
local connectStroke = addBorder(connectButton, SGC.ButtonBorder)
local connectHover = false

local function styleConnect()
    local mod = connectHover and SGM.Hover or SGM.Default
    if running then
        connectButton.BackgroundColor3 = color(SGC.Button, mod)
        connectButton.TextColor3 = color(SGC.ButtonText, mod)
        connectStroke.Color = color(SGC.ButtonBorder, mod)
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
    styleConnect()
end)

-- Divider
local divider = make("Frame", { Size = UDim2.new(1, 0, 0, 1), BorderSizePixel = 0, LayoutOrder = 5 }, root)
themed(function()
    divider.BackgroundColor3 = color(SGC.Border)
end)

-- Collapsible "Output" section (like the Properties panel headers)
local showLogs = plugin:GetSetting(SETTING_LOGS) == true

local logHeader = make("TextButton", {
    Size = UDim2.new(1, 0, 0, 20),
    BorderSizePixel = 0,
    Text = "",
    AutoButtonColor = false,
    LayoutOrder = 6,
}, root)
local logArrow = make("TextLabel", {
    Size = UDim2.fromOffset(16, 20),
    BackgroundTransparency = 1,
    Font = Enum.Font.SourceSans,
    TextSize = 14,
}, logHeader)
local logTitle = make("TextLabel", {
    Size = UDim2.new(1, -16, 1, 0),
    Position = UDim2.fromOffset(16, 0),
    BackgroundTransparency = 1,
    Text = "Output",
    Font = Enum.Font.SourceSansSemibold,
    TextSize = 14,
    TextXAlignment = Enum.TextXAlignment.Left,
}, logHeader)

local logFrame = make("ScrollingFrame", {
    Size = UDim2.new(1, 0, 0, 0),
    BorderSizePixel = 0,
    CanvasSize = UDim2.new(),
    AutomaticCanvasSize = Enum.AutomaticSize.Y,
    ScrollBarThickness = 8,
    ScrollBarImageTransparency = 0.4,
    LayoutOrder = 7,
    Visible = false,
}, root)
make("UIFlexItem", { FlexMode = Enum.UIFlexMode.Fill }, logFrame)
make("UIListLayout", { SortOrder = Enum.SortOrder.LayoutOrder }, logFrame)
make("UIPadding", { PaddingLeft = UDim.new(0, 4), PaddingTop = UDim.new(0, 2) }, logFrame)
addBorder(logFrame, SGC.Border)

local logHover = false
local function styleLogSection()
    logHeader.BackgroundColor3 = color(SGC.CategoryItem, logHover and SGM.Hover or SGM.Default)
    logArrow.TextColor3 = color(SGC.MainText)
    logTitle.TextColor3 = color(SGC.MainText)
    logArrow.Text = showLogs and "-" or "+"
    logFrame.Visible = showLogs
    logFrame.BackgroundColor3 = color(SGC.ScrollBarBackground)
    logFrame.ScrollBarImageColor3 = color(SGC.ScrollBar)
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
end)

-- ---------- Logging ----------

type LogLevel = "info" | "ok" | "warn" | "error" | "dim"

local function levelColor(level: LogLevel): Color3
    if level == "ok" then
        return color(SGC.InfoText)
    elseif level == "warn" then
        return color(SGC.WarningText)
    elseif level == "error" then
        return color(SGC.ErrorText)
    elseif level == "dim" then
        return color(SGC.DimmedText)
    end
    return color(SGC.MainText)
end

local logCount = 0
local function logLine(msg: string, level: LogLevel?)
    logCount += 1
    local lvl: LogLevel = level or "info"
    local line = make("TextLabel", {
        Size = UDim2.new(1, -8, 0, 16),
        BackgroundTransparency = 1,
        Text = msg,
        LayoutOrder = logCount,
        Font = Enum.Font.Code,
        TextSize = 13,
        TextXAlignment = Enum.TextXAlignment.Left,
        TextTruncate = Enum.TextTruncate.AtEnd,
    }, logFrame)
    themed(function()
        if line.Parent then
            line.TextColor3 = levelColor(lvl)
        end
    end)
    if logCount > 200 then
        for _, c in logFrame:GetChildren() do
            if c:IsA("TextLabel") then
                c:Destroy()
                break
            end
        end
    end
    if showLogs then
        logFrame.CanvasPosition = Vector2.new(0, math.huge)
    end
end

local function setStatus(connected: boolean, text: string)
    statusDot.BackgroundColor3 = connected and Color3.fromRGB(60, 180, 90) or color(SGC.ErrorText)
    statusLabel.Text = text
    connectButton.Text = running and "Disconnect" or "Connect"
    styleConnect()
end

-- ---------- Instance resolution ----------

local function resolveTarget(target)
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

local function keyFor(target, segments)
    return target .. "/" .. table.concat(segments, "/")
end

local function findInstance(target, segments)
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

local function ensureParent(target, segments)
    local current = resolveTarget(target)
    if not current then
        return nil
    end
    for i = 1, #segments - 1 do
        local seg = segments[i]
        local child = current:FindFirstChild(seg)
        if not child then
            child = Instance.new("Folder")
            child.Name = seg
            child:SetAttribute(ATTR, keyFor(target, { table.unpack(segments, 1, i) }))
            child.Parent = current
        end
        instanceCache[keyFor(target, { table.unpack(segments, 1, i) })] = child
        current = child
    end
    return current
end

local function replaceInstance(old, newClass)
    local new = Instance.new(newClass)
    new.Name = old.Name
    for attr, value in old:GetAttributes() do
        new:SetAttribute(attr, value)
    end
    for _, child in old:GetChildren() do
        child.Parent = new
    end
    new.Parent = old.Parent
    old:Destroy()
    return new
end

local BASE64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

local function decodeBase64(data: string): string
    local output = table.create(math.floor(#data * 3 / 4))
    local outCount = 0
    for i = 1, #data, 4 do
        local a = string.find(BASE64, string.sub(data, i, i), 1, true)
        local b = string.find(BASE64, string.sub(data, i + 1, i + 1), 1, true)
        local cChar = string.sub(data, i + 2, i + 2)
        local dChar = string.sub(data, i + 3, i + 3)
        local c = cChar == "=" and 1 or string.find(BASE64, cChar, 1, true)
        local d = dChar == "=" and 1 or string.find(BASE64, dChar, 1, true)
        if not a or not b or not c or not d then
            error("invalid base64 model payload")
        end
        a -= 1
        b -= 1
        c -= 1
        d -= 1
        outCount += 1
        output[outCount] = string.char(a * 4 + math.floor(b / 16))
        if cChar ~= "=" then
            outCount += 1
            output[outCount] = string.char((b % 16) * 16 + math.floor(c / 4))
        end
        if dChar ~= "=" then
            outCount += 1
            output[outCount] = string.char((c % 4) * 64 + d)
        end
    end
    return table.concat(output, "", 1, outCount)
end

local function xmlDecode(text: string): string
    text = string.gsub(text, "&#x([%da-fA-F]+);", function(hex)
        local n = tonumber(hex, 16)
        return n and utf8.char(n) or ""
    end)
    text = string.gsub(text, "&#(%d+);", function(dec)
        local n = tonumber(dec)
        return n and utf8.char(n) or ""
    end)
    text = string.gsub(text, "&lt;", "<")
    text = string.gsub(text, "&gt;", ">")
    text = string.gsub(text, "&quot;", "\"")
    text = string.gsub(text, "&apos;", "'")
    text = string.gsub(text, "&amp;", "&")
    return text
end

local function parseXml(xml: string)
    local root = { name = "#document", attrs = {}, children = {}, textParts = {} }
    local stack = { root }
    local pos = 1
    while pos <= #xml do
        local lt = string.find(xml, "<", pos, true)
        if not lt then
            local tail = string.sub(xml, pos)
            if #tail > 0 then table.insert(stack[#stack].textParts, xmlDecode(tail)) end
            break
        end
        if lt > pos then
            table.insert(stack[#stack].textParts, xmlDecode(string.sub(xml, pos, lt - 1)))
        end
        if string.sub(xml, lt, lt + 8) == "<![CDATA[" then
            local close = string.find(xml, "]]>", lt + 9, true)
            if not close then error("unterminated XML CDATA section") end
            table.insert(stack[#stack].textParts, string.sub(xml, lt + 9, close - 1))
            pos = close + 3
        elseif string.sub(xml, lt, lt + 3) == "<!--" then
            local close = string.find(xml, "-->", lt + 4, true)
            if not close then error("unterminated XML comment") end
            pos = close + 3
        elseif string.sub(xml, lt, lt + 1) == "<?" then
            local close = string.find(xml, "?>", lt + 2, true)
            if not close then error("unterminated XML processing instruction") end
            pos = close + 2
        elseif string.sub(xml, lt, lt + 1) == "<!" then
            local close = string.find(xml, ">", lt + 2, true)
            if not close then error("unterminated XML declaration") end
            pos = close + 1
        else
            local close = string.find(xml, ">", lt + 1, true)
            if not close then error("unterminated XML tag") end
            local inside = string.sub(xml, lt + 1, close - 1)
            if string.sub(inside, 1, 1) == "/" then
                local name = string.match(inside, "^/%s*([^%s>]+)")
                if #stack <= 1 or not name or stack[#stack].name ~= name then
                    error("mismatched XML closing tag " .. tostring(name))
                end
                table.remove(stack)
            else
                local selfClosing = string.match(inside, "/%s*$") ~= nil
                if selfClosing then inside = string.gsub(inside, "/%s*$", "") end
                local name = string.match(inside, "^%s*([^%s]+)")
                if not name then error("empty XML tag") end
                local node = { name = name, attrs = {}, children = {}, textParts = {} }
                local tagName = string.match(inside, "^%s*([^%s]+)")
                local rest = string.sub(inside, #tagName + 1)
                while true do
                    rest = string.gsub(rest, "^%s+", "")
                    if rest == "" then break end
                    local attr, afterName = string.match(rest, "^([%w_:%.%-]+)%s*=%s*(.*)$")
                    if not attr then error("malformed XML attribute") end
                    local quote = string.sub(afterName, 1, 1)
                    if quote ~= "'" and quote ~= '\"' then error("unquoted XML attribute") end
                    local endQuote = string.find(afterName, quote, 2, true)
                    if not endQuote then error("unterminated XML attribute") end
                    node.attrs[attr] = xmlDecode(string.sub(afterName, 2, endQuote - 1))
                    rest = string.sub(afterName, endQuote + 1)
                end
                table.insert(stack[#stack].children, node)
                if not selfClosing then table.insert(stack, node) end
            end
            pos = close + 1
        end
    end
    if #stack ~= 1 then error("unclosed XML element " .. tostring(stack[#stack].name)) end
    local function finish(node)
        node.text = table.concat(node.textParts)
        node.textParts = nil
        for _, child in node.children do finish(child) end
    end
    finish(root)
    return root
end

local function xmlChild(node, name)
    for _, child in node.children do
        if child.name == name then return child end
    end
    return nil
end

local function xmlText(node, name)
    local child = xmlChild(node, name)
    return child and child.text or ""
end

local function xmlTrim(text: string): string
    return string.match(text, "^%s*(.-)%s*$") or ""
end

local function xmlNumber(node, name): number
    return tonumber(xmlTrim(xmlText(node, name))) or 0
end

local function xmlVector2(node, prefix)
    local pre = prefix or ""
    return Vector2.new(xmlNumber(node, pre .. "X"), xmlNumber(node, pre .. "Y"))
end

local function xmlVector3(node, prefix)
    local pre = prefix or ""
    return Vector3.new(xmlNumber(node, pre .. "X"), xmlNumber(node, pre .. "Y"), xmlNumber(node, pre .. "Z"))
end

local function parseRbxmxProperty(prop, sharedStrings)
    local kind = prop.name
    local text = xmlTrim(prop.text)
    if kind == "string" or kind == "ProtectedString" then
        return prop.text
    elseif kind == "bool" then
        return text == "true" or text == "1"
    elseif kind == "int" or kind == "int64" or kind == "float" or kind == "double" or kind == "token" then
        return tonumber(text) or 0
    elseif kind == "Vector3" then
        return xmlVector3(prop)
    elseif kind == "Vector2" then
        return xmlVector2(prop)
    elseif kind == "Color3" then
        return Color3.new(xmlNumber(prop, "R"), xmlNumber(prop, "G"), xmlNumber(prop, "B"))
    elseif kind == "Color3uint8" then
        return Color3.fromRGB(xmlNumber(prop, "R"), xmlNumber(prop, "G"), xmlNumber(prop, "B"))
    elseif kind == "UDim" then
        return UDim.new(xmlNumber(prop, "S"), xmlNumber(prop, "O"))
    elseif kind == "UDim2" then
        return UDim2.new(xmlNumber(prop, "XS"), xmlNumber(prop, "XO"), xmlNumber(prop, "YS"), xmlNumber(prop, "YO"))
    elseif kind == "CFrame" or kind == "CoordinateFrame" then
        local position = xmlVector3(prop)
        local matrix = {}
        for _, key in { "R00", "R01", "R02", "R10", "R11", "R12", "R20", "R21", "R22" } do
            matrix[#matrix + 1] = xmlNumber(prop, key)
        end
        if xmlChild(prop, "R00") then
            return CFrame.new(position.X, position.Y, position.Z, table.unpack(matrix))
        end
        return CFrame.new(position)
    elseif kind == "Ref" then
        return { __chainlinkRef = text }
    elseif kind == "Content" then
        return xmlText(prop, "url") ~= "" and xmlText(prop, "url") or prop.text
    elseif kind == "url" then
        return prop.text
    elseif kind == "BinaryString" then
        return decodeBase64(text)
    elseif kind == "SharedString" then
        local reference = xmlChild(prop, "SharedString")
        local key = (reference and reference.attrs.md5) or prop.attrs.md5 or text
        return sharedStrings[key] or ""
    elseif kind == "NumberRange" then
        local min = tonumber(xmlTrim(xmlText(prop, "Min"))) or tonumber(xmlTrim(xmlText(prop, "min"))) or tonumber(prop.attrs.min) or 0
        local max = tonumber(xmlTrim(xmlText(prop, "Max"))) or tonumber(xmlTrim(xmlText(prop, "max"))) or tonumber(prop.attrs.max) or 0
        return NumberRange.new(min, max)
    elseif kind == "Rect" then
        local minNode = xmlChild(prop, "min") or xmlChild(prop, "Min")
        local maxNode = xmlChild(prop, "max") or xmlChild(prop, "Max")
        if minNode and maxNode then
            return Rect.new(xmlVector2(minNode), xmlVector2(maxNode))
        end
        return Rect.new(Vector2.new(xmlNumber(prop, "MinX"), xmlNumber(prop, "MinY")), Vector2.new(xmlNumber(prop, "MaxX"), xmlNumber(prop, "MaxY")))
    end
    return nil, "unsupported XML property type " .. kind
end

local function parseRbxmx(xml: string)
    local document = parseXml(xml)
    local roblox = xmlChild(document, "roblox")
    if not roblox then error("XML model has no <roblox> root") end
    local sharedStrings = {}
    local sharedNode = xmlChild(roblox, "SharedStrings")
    if sharedNode then
        for _, entry in sharedNode.children do
            if entry.name == "SharedString" and entry.attrs.md5 then
                sharedStrings[entry.attrs.md5] = entry.text
            end
        end
    end
    local topLevelItems = {}
    local topLevelCount = 0
    local referents = {}
    local pendingRefs = {}
    local madeItems = {}
    local function buildItem(node, parent)
        local className = node.attrs.class
        if not className then warn("ChainLink: skipping <Item> without class") return end
        local ok, inst = pcall(Instance.new, className)
        if not ok then warn("ChainLink: cannot create XML class " .. className .. ": " .. tostring(inst)) return end
        inst.Parent = parent
        if node.attrs.referent and node.attrs.referent ~= "" then referents[node.attrs.referent] = inst end
        table.insert(madeItems, inst)
        local properties = xmlChild(node, "Properties")
        if properties then
            for _, prop in properties.children do
                local name = prop.attrs.name
                if name then
                    local value, problem = parseRbxmxProperty(prop, sharedStrings)
                    if problem then
                        warn("ChainLink: " .. problem .. " (" .. name .. ")")
                    elseif type(value) == "table" and value.__chainlinkRef then
                        table.insert(pendingRefs, { inst = inst, property = name, referent = value.__chainlinkRef })
                    elseif value ~= nil then
                        if prop.name == "token" and type(value) == "number" then
                            local enumOk, currentValue = pcall(function() return (inst :: any)[name] end)
                            if enumOk and typeof(currentValue) == "EnumItem" then
                                local itemsOk, items = pcall(function() return Enum[currentValue.EnumType.Name]:GetEnumItems() end)
                                if itemsOk then
                                    for _, enumItem in items do
                                        if enumItem.Value == value then value = enumItem break end
                                    end
                                end
                            end
                        end
                        local setOk, setErr = pcall(function() (inst :: any)[name] = value end)
                        if not setOk then warn("ChainLink: cannot set " .. className .. "." .. name .. ": " .. tostring(setErr)) end
                    end
                end
            end
        end
        for _, child in node.children do
            if child.name == "Item" then buildItem(child, inst) end
        end
    end
    for _, node in roblox.children do
        if node.name == "Item" then
            topLevelCount += 1
            local before = #madeItems
            buildItem(node, nil)
            if #madeItems > before then
                table.insert(topLevelItems, madeItems[before + 1])
            end
        end
    end
    for _, ref in pendingRefs do
        local target = referents[ref.referent]
        if target then
            local ok, err = pcall(function() (ref.inst :: any)[ref.property] = target end)
            if not ok then warn("ChainLink: cannot resolve Ref property " .. ref.property .. ": " .. tostring(err)) end
        elseif ref.referent ~= "null" and ref.referent ~= "" then
            warn("ChainLink: unresolved XML referent " .. ref.referent .. " for " .. ref.property)
        end
    end
    return topLevelItems, topLevelCount
end

local function setRbxmx(parent: Instance, name: string, key: string, xml: string): Instance?
    local items, rootCount = parseRbxmx(xml)
    if rootCount == 0 then
        warn("ChainLink: .rbxmx has no top-level <Item>; skipping " .. name)
        return nil
    end
    if #items == 0 then
        warn("ChainLink: .rbxmx top-level items could not be created; skipping " .. name)
        return nil
    end

    local synced: Instance
    if rootCount == 1 and #items == 1 then
        synced = items[1]
        -- Name the synced root after the file for consistency with other synced files.
        synced.Name = name
    else
        warn("ChainLink: .rbxmx has multiple top-level <Item> roots; using a Folder for " .. name)
        local folder = Instance.new("Folder")
        folder.Name = name
        for _, item in items do
            item.Parent = folder
        end
        synced = folder
    end

    local existing = parent:FindFirstChild(name)
    if existing then existing:Destroy() end
    synced:SetAttribute(ATTR, key)
    synced.Parent = parent
    return synced
end

local function setModel(inst, ev)
    if ev.modelFormat ~= "rbxm" then
        error(".rbxm sync payload must be binary model data for automatic Studio insertion")
    end
    local bytes = decodeBase64(ev.modelData)
    local imported = SerializationService:DeserializeInstancesAsync(buffer.fromstring(bytes))
    for _, child in inst:GetChildren() do
        child:Destroy()
    end
    for _, item in imported do
        item.Parent = inst
    end
end

local function setSource(inst, source, ev)
    if ev and ev.modelData then
        if not inst:IsA("Model") then
            error("model file did not resolve to a Model instance")
        end
        setModel(inst, ev)
    elseif inst:IsA("LuaSourceContainer") then
        local ok = pcall(function()
            ScriptEditorService:UpdateSourceAsync(inst, function()
                return source
            end)
        end)
        if not ok then
            inst.Source = source
        end
    elseif inst:IsA("StringValue") then
        inst.Value = source
    end
end
-- ---------- Applying events ----------

local function applyCreateOrUpdate(ev)
    local segments = ev.instancePath
    local key = keyFor(ev.target, segments)

    if #segments == 0 then
        return
    end

    local parent = ensureParent(ev.target, segments)
    if not parent then
        logLine("Target not found: " .. ev.target, "warn")
        return
    end

    local name = segments[#segments]
    local isRbxmx = string.lower(ev.path or ""):sub(-6) == ".rbxmx"
    if isRbxmx and string.lower(name):sub(-6) == ".rbxmx" then
        name = string.sub(name, 1, #name - 7)
    end
    if isRbxmx then
        local xml = ev.source or ev.contents or ev.modelData or ""
        if type(xml) ~= "string" then error(".rbxmx payload has no XML text") end
        if string.sub(xml, 1, 5) ~= "<?xml" and not string.find(xml, "<roblox", 1, true) then
            local decodedOk, decoded = pcall(decodeBase64, xml)
            if decodedOk and string.find(decoded, "<roblox", 1, true) then xml = decoded end
        end
        local ok, synced = pcall(setRbxmx, parent, name, key, xml)
        if not ok then
            logLine(".rbxmx import failed: " .. tostring(synced), "error")
        elseif synced then
            instanceCache[key] = synced
        end
        return
    end

    local targetClass = ev.className
    local inst = parent:FindFirstChild(name)
    if inst and inst.ClassName ~= targetClass then
        inst = replaceInstance(inst, targetClass)
    elseif not inst then
        inst = Instance.new(targetClass)
        inst.Name = name
        inst.Parent = parent
    end
    inst:SetAttribute(ATTR, key)
    instanceCache[key] = inst
    local ok, err = pcall(setSource, inst, ev.source or "", ev)
    if not ok then
        logLine("Model/source import failed: " .. tostring(err), "error")
    end
end

local function applyDelete(ev)
    local key = keyFor(ev.target, ev.instancePath)
    local lookupSegments = ev.instancePath
    if string.lower(ev.path or ""):sub(-6) == ".rbxmx" and #ev.instancePath > 0 then
        lookupSegments = table.clone(ev.instancePath)
        local last = lookupSegments[#lookupSegments]
        if string.lower(last):sub(-6) == ".rbxmx" then
            lookupSegments[#lookupSegments] = string.sub(last, 1, #last - 7)
        end
    end
    local inst = findInstance(ev.target, lookupSegments)
    instanceCache[key] = nil
    if not inst then
        return
    end

    if #ev.instancePath == 0 then
        logLine("Skipping delete of root target: " .. ev.target, "warn")
        return
    end

    if inst:GetAttribute(ATTR) ~= key then
        logLine("Skipping delete of unmanaged " .. inst:GetFullName(), "warn")
        return
    end
    if ev.keepChildren and #inst:GetChildren() > 0 then
        instanceCache[key] = replaceInstance(inst, "Folder")
    else
        inst:Destroy()
    end
end

local function pruneStale(validKeys)
    for _, inst in game:GetDescendants() do
        local key = inst:GetAttribute(ATTR)
        if type(key) == "string" and not validKeys[key] then
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
    end
    instanceCache = {}
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
                valid[keyFor(ev.target, ev.instancePath)] = true
                for i = 1, #ev.instancePath - 1 do
                    valid[keyFor(ev.target, { table.unpack(ev.instancePath, 1, i) })] = true
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
                logLine(string.format("[%s] %s", ev.op, ev.path))
            end
        end
        if resp.full then
            logLine(string.format("Snapshot applied: %d files", #events), "ok")
        end
    end)
    if recording then
        ChangeHistoryService:FinishRecording(
            recording,
            ok and Enum.FinishRecordingOperation.Commit or Enum.FinishRecordingOperation.Cancel
        )
    end
    if not ok then
        logLine("Apply error: " .. tostring(err), "error")
    end
end

-- ---------- Polling ----------

local function request(url, token)
    local headers = {}
    if token ~= "" then
        headers["X-ChainLink-Token"] = token
    end
    local res = HttpService:RequestAsync({ Url = url, Method = "GET", Headers = headers })
    if not res.Success then
        error(string.format("HTTP %d %s", res.StatusCode, res.StatusMessage))
    end
    return HttpService:JSONDecode(res.Body)
end

local function pollLoop(baseUrl, token)
    local backoff = 1
    local fatal = false
    local skip = false
    while running do
        local url = string.format("%s/changes?since=%d&session=%s", baseUrl, lastSeq, HttpService:UrlEncode(sessionId))
        local ok, result = pcall(request, url, token)
        if not running then
            break
        end

        if ok then
            if backoff > 1 then
                logLine("Reconnected", "ok")
            end
            backoff = 1
            setStatus(true, "Connected (" .. baseUrl .. ")")

            if result.sessionId ~= sessionId then
                sessionId = result.sessionId
            end

            applyBatch(result)
            lastSeq = result.seq
        else
            local msg = tostring(result)
            if string.find(msg, "not enabled", 1, true) then
                logLine("Enable HTTP requests: Game Settings > Security > Allow HTTP Requests", "error")
                running = false
                fatal = true
                break
            elseif string.find(msg, "requests exceeded limit", 1, true) then
                logLine("Roblox HTTP limit reached. Backing off...", "warn")
                backoff = math.max(backoff * 2, 5)
                setStatus(false, "Rate limited... backing off")
                task.wait(backoff)
                skip = true
            end
            if not skip then
                setStatus(false, "Reconnecting… (" .. msg .. ")")
                if backoff == 1 then
                    logLine("Connection lost: " .. msg, "warn")
                end
                task.wait(backoff)
                backoff = math.min(backoff * 2, 10)
            end
            skip = false
        end
    end
    setStatus(false, fatal and "HTTP disabled" or "Disconnected")
end

local function connect()
    local baseUrl = string.gsub(urlBox.Text, "/+$", "")
    local token = tokenBox.Text
    plugin:SetSetting(SETTING_URL, baseUrl)
    plugin:SetSetting(SETTING_TOKEN, token)

    running = true
    lastSeq = 0
    sessionId = ""
    setStatus(false, "Connecting…")
    logLine("Connecting to " .. baseUrl)
    task.spawn(pollLoop, baseUrl, token)
end

local function disconnect()
    running = false
    setStatus(false, "Disconnected")
    logLine("Disconnected", "dim")
end

connectButton.MouseButton1Click:Connect(function()
    if running then
        disconnect()
    else
        connect()
    end
end)

toggleButton.Click:Connect(function()
    widget.Enabled = not widget.Enabled
end)

widget:GetPropertyChangedSignal("Enabled"):Connect(function()
    toggleButton:SetActive(widget.Enabled)
end)

plugin.Unloading:Connect(function()
    running = false
end)

setStatus(false, "Disconnected")
logLine("ChainLink ready. Run `chainlink serve` and press Connect.", "dim")