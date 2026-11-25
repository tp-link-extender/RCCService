package main

const (
	// Lua that runs inside the RCC and Studio processes. The placeholders are filled in by the Orbiter

	// hostEpilogue is prepended to every host script sent to RCC. IMPORTANT: OpenJobEx script environments on this RCC build don't pump a scheduler - wait() and delay() never resume - so all reporting here is synchronous at script start, or fires off of engine events once RunService:Run() pumps the heartbeat in the rest of the script
	hostEpilogue = `print "[Orbiter][RCC]: Reporting to the Orbiter..."
local Players = game:GetService "Players"

pcall(function()
	game:HttpPost("_PING_URL", "Loaded", true, "text/json")
end)

Players.PlayerAdded:connect(function(player)
	pcall(function()
		game:HttpPost("_PING_URL", "PlayerAdded\n" .. tostring(player.userId), true, "text/json")
		print(("[Orbiter][RCC]: Player %d added and reported"):format(player.userId))
	end)
end)

Players.PlayerRemoving:connect(function(player)
	pcall(function()
		game:HttpPost("_PING_URL", "PlayersLeft\n" .. tostring(player.userId), true, "text/json")
		print(("[Orbiter][RCC]: Player %d left and reported"):format(player.userId))
	end)
end)`

	// rccHostScript is the RCC-safe host script. IMPORTANT: OpenJobEx script environments don't pump a scheduler on this RCC build - wait() and delay() never resume - so this is entirely inline (see gameserver.txt) and ends with RunService:Run(), which takes over the script forever while pumping everything else (including the reporting code the epilogue prepends)
	rccHostScript = `print "[Orbiter][RCC]: Starting hosted gameserver..."

local ScriptContext = game:GetService("ScriptContext")
local NetworkServer = game:GetService("NetworkServer")
local Players = game:GetService("Players")
local Visit = game:GetService("Visit")
local RunService = game:GetService("RunService")

pcall(function() settings().Network.UseInstancePacketCache = true end)
pcall(function() settings().Network.UsePhysicsPacketCache = true end)
settings()["Task Scheduler"].PriorityMethod = Enum.PriorityMethod.AccumulatedError
settings().Network.PhysicsSend = Enum.PhysicsSendMethod.TopNErrors
settings().Network.ExperimentalPhysicsEnabled = true
pcall(function() settings().Diagnostics:LegacyScriptMode() end)
pcall(function() settings().Diagnostics.LuaRamLimit = 0 end)

ScriptContext.ScriptsDisabled = true

local url = "http://_BASE_URL"

pcall(function() game:SetPlaceID(_PLACE_ID, false) end)
pcall(function() game:GetService("ChangeHistoryService"):SetEnabled(false) end)

pcall(function() Players:SetAbuseReportUrl(url .. "/AbuseReport/InGameChatHandler.ashx") end)
pcall(function() ScriptInformationProvider = game:GetService("ScriptInformationProvider"); ScriptInformationProvider:SetAssetUrl(url .. "/asset/") end)
pcall(function() ContentProvider:SetBaseUrl(url) end)
pcall(function() game:GetService("BadgeService"):SetPlaceId(_PLACE_ID) end)
pcall(function() game:GetService("BadgeService"):SetIsBadgeLegalUrl("") end)
pcall(function() InsertService:SetBaseSetsUrl(url .. "/Game/Tools/InsertAsset.ashx?nsets=10&type=base") end)
pcall(function() InsertService:SetUserSetsUrl(url .. "/Game/Tools/InsertAsset.ashx?nsets=20&type=user&userid=%d") end)
pcall(function() InsertService:SetCollectionUrl(url .. "/Game/Tools/InsertAsset.ashx?sid=%d") end)
pcall(function() InsertService:SetAssetUrl(url .. "/asset?id=%d") end)
pcall(function() InsertService:SetAssetVersionUrl(url .. "/asset?assetversionid=%d") end)

if _MAP_LOCATION ~= "" then
	print "[Orbiter][RCC]: loading map..."
	game:Load(_MAP_LOCATION)
	print "[Orbiter][RCC]: map loaded"
end

Players.PlayerAdded:connect(function(player)
	print("Player " .. player.userId .. " added")
end)
Players.PlayerRemoving:connect(function(player)
	print("Player " .. player.userId .. " leaving")
end)

NetworkServer:Start(_SERVER_PORT)
print "[Orbiter][RCC]: network server started"
_PRESENCE_PING

-- report to the Orbiter right away; anything below this line runs forever once RunService:Run() takes over, and nothing past the end of the script ever executes
pcall(function() game:HttpPost("_HOSTPING_URL", "Ready", true, "text/json") end)

-- periodic engine liveness reports, once every ~600 heartbeats into an integer
local heartbeat_count = 0
RunService.Heartbeat:connect(function()
	heartbeat_count = heartbeat_count + 1
	if heartbeat_count % 600 == 0 then
		pcall(function() game:HttpPost("_HOSTPING_URL", "Heartbeat", true, "text/json") end)
	end
end)

-- inactivity report (like Studio's host.lua: 5 minutes after the last player leaves, including the initial grace period before anyone joins). game:Shutdown() doesn't exist in RCC jobs, so the Orbiter closes the job on receiving this
local lastActive = os.time()
local hasReportedUnused = false
local RCCPlayers = game:GetService("Players")

RCCPlayers.PlayerAdded:connect(function()
	lastActive = os.time()
end)

RCCPlayers.PlayerRemoving:connect(function()
	lastActive = os.time()
end)

RunService.Heartbeat:connect(function()
	if not hasReportedUnused and #RCCPlayers:GetPlayers() == 0 and os.time() - lastActive > 300 then
		hasReportedUnused = true
		print "[Orbiter][RCC]: server empty for 5 minutes, requesting shutdown"
		pcall(function() game:HttpPost("_HOSTPING_URL", "Unoccupied", true, "text/json") end)
	end
end)

ScriptContext:SetTimeout(10)
ScriptContext.ScriptsDisabled = false

game:GetService("RunService"):Run()`
)
