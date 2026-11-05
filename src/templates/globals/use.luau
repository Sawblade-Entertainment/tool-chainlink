function use(m)
	if type(m) == "string" and m:sub(1,1) ~= "." and m:sub(1,1) ~= "@" then
		if m:sub(1,1) == "/" then
			local curr = game.ReplicatedStorage
			for _, child in pairs(m:sub(2,#m):split("/")) do
				curr = curr:WaitForChild(child)
			end
			m = curr
		elseif string.lower(m) == m then
			m = game.ReplicatedStorage:WaitForChild("packages"):WaitForChild(m)
		else
			return game:GetService(m)
		end
	end
	return require(m)
end
