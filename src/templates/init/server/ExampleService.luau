local ServerStorage = use "ServerStorage"
local ReplicatedStorage = use "ReplicatedStorage"

function mod:init()
	-- prefab/Office.rbxmx syncs to ServerStorage; put a copy in the world.
	local office = ServerStorage.Office:Clone()
	office.Parent = workspace

	-- Once `chainlink assets` has uploaded assets/spawn.png, show it on every logo.
	if ReplicatedStorage:FindFirstChild("assets") then
		local logo = asset "spawn.png"
		for _, item in office:GetDescendants() do
			if item:IsA("Decal") and item.Name == "Logo" then
				item.Texture = logo
			end
		end
	end
end
