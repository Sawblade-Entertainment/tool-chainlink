function asset(p)
	local id = require(game.ReplicatedStorage:WaitForChild("assets"))[p]
	if id == nil then
		error("asset " .. p .. " has not been uploaded; run 'chainlink assets'", 2)
	end
	return id
end
