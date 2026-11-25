package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	c "github.com/TwiN/go-color" // the log colours are kinda random loll whatever
	"github.com/caddyserver/certmagic"
	env "github.com/joho/godotenv"
	. "github.com/tp-link-extender/RCCService/Shared"
)

type (
	Proxy struct {
		Port int
		conn *net.UDPConn
		wg sync.WaitGroup
	}

	Session struct {
		client *net.UDPAddr
		gs *net.UDPConn
		last atomic.Int64 // unix nanoseconds, written by both directions of the relay
	}

	Status uint8

	GameserverInfo struct {
		Pid int `json:"pid"`
		Proxy *Proxy
		StartTime int64 `json:"startTime"`
		Status Status `json:"status"`
		statusChanged chan struct{}
		statusMu sync.Mutex
		lastPush time.Time // any push (~30s engine heartbeats) walks this forward
	}

	Gameserver struct {
		GameserverInfo
		*exec.Cmd

		// "rcc" gameservers aren't backed by a process we own, but by a job on an
		// RCCService instance (managed by the Proxy). Closing them means telling the
		// Proxy to send CloseJobEx
		engine string
		stopFn func() error
		done chan struct{}
		stopOnce sync.Once
	}

	Gameservers struct {
		version string

		// handlers and trackers run concurrently, so the map is only touched under serversMu
		serversMu sync.Mutex
		servers map[int]*Gameserver

		// closed and replaced whenever a server is added, waking everything waiting for one
		serverAdded chan struct{}

		// statuses pushed between a hosted job starting and its tracker registering
		// the server; flushed on registration
		pendingMu sync.Mutex
		pending map[int][][]string
	}

	serveScript struct {
		token string
		body []byte
	}
)

const (
	exeName = `MercuryStudioBeta.exe`
	versionPathStart = "./Versions/version-"

	// engines for hosting

	engineStudio = "studio" // 2013 Studio hosted gameservers
	engineRCC = "rcc" // gameservers hosted via RCCService jobs through the Proxy

	proto = "udp4"
	idleTimeout = 35 * time.Second
)

const (
	Closed Status = iota
	Starting
	Running
)

const (
	proxyOffset = 25000

	// RCC-safe host script. IMPORTANT: OpenJobEx script environments don't pump a
	// scheduler on this RCC build - wait() and delay() never resume - so this is entirely
	// inline (see gameserver.txt) and ends with RunService:Run(), which takes over the
	// script forever while pumping everything else (including the reporting code the
	// Proxy prepends)
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

-- report to the Orbiter right away; anything below this line runs forever once
-- RunService:Run() takes over, and nothing past the end of the script ever executes
pcall(function() game:HttpPost("_HOSTPING_URL", "Ready", true, "text/json") end)

-- periodic engine liveness reports, once every ~600 heartbeats into an integer
local heartbeat_count = 0
RunService.Heartbeat:connect(function()
	heartbeat_count = heartbeat_count + 1
	if heartbeat_count % 600 == 0 then
		pcall(function() game:HttpPost("_HOSTPING_URL", "Heartbeat", true, "text/json") end)
	end
end)

-- inactivity report (like Studio's host.lua: 5 minutes after the last player
-- leaves, including the initial grace period before anyone joins). game:Shutdown()
-- doesn't exist in RCC jobs, so the Orbiter closes the job on receiving this
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

var (
	serveScripts = struct {
		sync.RWMutex

		m map[int]serveScript
	}{
		m: make(map[int]serveScript),
	}
	reBaseURL = regexp.MustCompile(`local url = "http://" \.\. "([^"]+)"`) // the serve loadscript sets this up with the Site's DomainInsecure
	reMapLocation = regexp.MustCompile(`local mapLoc = "([^"]*)"`)
	rePresenceURL = regexp.MustCompile(`[a-z]+://[^"'\s]+/game/serverpresence\?ticket=[^"'\s]+`)
)

func CheckServerUp(port int) bool {
	// start a UDP server on the same port and see if it errors
	// gee, I sure hope this never interferes with the actual server starting
	conn, err := net.ListenUDP(proto, &net.UDPAddr{
		Port: port,
	}) // gs-client communication only works on ipv4...
	if err != nil {
		return true
	}

	conn.Close()

	return false
}

func (g *GameserverInfo) SetStatus(s Status) {
	// SetStatus(Closed) both signals and closes; don't let that channel double-close
	g.statusMu.Lock()

	defer g.statusMu.Unlock()

	if g.Status == Closed {
		return
	}

	Log(fmt.Sprintf("[status] - changed: %d -> %d", g.Status, s))

	g.Status = s

	if s == Starting || s == Running {
		g.lastPush = time.Now() // cover statuses that constitute liveliness
	}

	g.statusChanged <- struct{}{}

	if s == Closed {
		close(g.statusChanged)
	}
}

// lastPushTime reads the last push timestamp under the status lock
func (g *GameserverInfo) lastPushTime() time.Time {
	g.statusMu.Lock()

	defer g.statusMu.Unlock()

	return g.lastPush
}

// addServer registers a server and wakes everything waiting for one
func (gs *Gameservers) addServer(id int, server *Gameserver) {
	gs.serversMu.Lock()

	defer gs.serversMu.Unlock()

	gs.servers[id] = server

	close(gs.serverAdded)

	gs.serverAdded = make(chan struct{})
}

// deferStatus stores a push that arrived before its server was registered
func (gs *Gameservers) deferStatus(id int, data []string) {
	gs.pendingMu.Lock()

	gs.pending[id] = append(gs.pending[id], data)

	gs.pendingMu.Unlock()
}

// server returns the server registered for id (nil if none), and a channel closed when the next server is added
func (gs *Gameservers) server(id int) (*Gameserver, <-chan struct{}) {
	gs.serversMu.Lock()

	defer gs.serversMu.Unlock()

	return gs.servers[id], gs.serverAdded
}

// waitForServer returns the server registered for id, waiting up to timeout for it to be added
func (gs *Gameservers) waitForServer(id int, timeout time.Duration) *Gameserver {
	for deadline := time.After(timeout); ; {
		server, added := gs.server(id)
		if server != nil {
			return server
		}

		select {
		case <-added:
		case <-deadline:
			return nil
		}
	}
}

func (gs *Gameservers) streamRoute(w http.ResponseWriter, r *http.Request) {
	// Return events from statusChanged as SSE
	const wait = 30 * time.Second

	// don't check the IP, this route is public
	w.Header().Set("Access-Control-Allow-Origin", "*")

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	Log(fmt.Sprintf("[stream] %d request received", id))

	server := gs.waitForServer(id, wait)
	if server == nil {
		http.Error(w, "Gameserver not found for this ID", http.StatusNotFound)
		return
	}

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	enc := json.NewEncoder(w)
	flusher, ok := w.(http.Flusher)

	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	send := func(s Status) {
		Log(fmt.Sprintf("[stream] %d sending update", id))
		w.Write([]byte("data: "))
		enc.Encode(s)
		w.Write([]byte{'\n'})
		flusher.Flush()
	}

	for {
		send(server.Status)

		if server.Status != Starting {
			return
		}

		_, more := <-server.statusChanged
		if !more {
			Log(fmt.Sprintf("[stream] %d status channel closed, ending stream", id))
			return
		}
	}
}

func NewGameservers(version string) *Gameservers {
	return &Gameservers{
		version: version,
		servers: make(map[int]*Gameserver),
		serverAdded: make(chan struct{}),
		pending: make(map[int][][]string),
	}
}

func (p *Proxy) Stop() error {
	if p.conn != nil {
		// Closing the UDPConn will cause runProxyOnPort to exit.
		_ = p.conn.Close()

		p.wg.Wait()

		p.conn = nil
	}

	Log(c.InYellow(fmt.Sprintf("[proxy:%d] stopped", p.Port)))

	return nil
}

func (g *Gameserver) Stop() error {
	if g.Status == Closed {
		return nil
	}

	g.SetStatus(Closed) // block Track's done wait and let it bail on the Closed check
	g.stopOnce.Do(func() {
		if g.stopFn != nil {
			if err := g.stopFn(); err != nil { // close RCC job (nil for studio servers)
				Log(c.InRed(fmt.Sprintf("Failed to close RCC job: %s", err.Error())))
			}
		}

		if g.Cmd != nil {
			g.Process.Kill()
		}

		close(g.done)
	})

	if g.Proxy != nil {
		g.Proxy.Stop()
	}

	return nil
}

// applyStatus processes a pushed status from a hosted gameserver (first line of
// the body is the status)
func (gs *Gameservers) applyStatus(id int, server *Gameserver, data []string) {
	status := data[0]

	// any push from the hosted script proves the engine is alive
	server.GameserverInfo.statusMu.Lock()

	server.GameserverInfo.lastPush = time.Now()

	server.GameserverInfo.statusMu.Unlock()

	switch status {
	case "Ready":
		Log(c.InGreen(fmt.Sprintf("[hoststatus] %d server is ready", id)))

		if server.Status == Starting {
			server.SetStatus(Running)
		}
	case "Loaded":
		Log(c.InGreen(fmt.Sprintf("[hoststatus] %d host script loaded", id)))
	case "PlayerAdded":
		var userId string

		if len(data) > 1 {
			userId = data[1]
		}

		Log(c.InGreen(fmt.Sprintf("[hoststatus] %d player joined: %s", id, userId)))
	case "PlayersLeft":
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d player left", id)))
	case "Unoccupied":
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d server unoccupied, closing job", id)))
		server.Stop()
	case "Closed":
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d server announced its own closure", id)))
		server.Stop()
	default:
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d unknown status: %s", id, status)))
	}
}

// flushStatuses applies (and clears) any deferred statuses for id
func (gs *Gameservers) flushStatuses(id int, server *Gameserver) {
	gs.pendingMu.Lock()

	deferred := gs.pending[id]

	delete(gs.pending, id)
	gs.pendingMu.Unlock()

	for _, data := range deferred {
		Log(c.InBlue(fmt.Sprintf("[track] %d - flushing deferred status: %s", id, data[0])))
		gs.applyStatus(id, server, data)
	}
}

// allowedIP reports whether an address is this machine's, or the IPV4 or IPV6 one configured
func allowedIP(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	if ip.IsLoopback() {
		return true
	}

	for _, name := range []string{"IPV4", "IPV6"} {
		if configured := net.ParseIP(strings.Trim(os.Getenv(name), "[]")); configured != nil && configured.Equal(ip) {
			return true
		}
	}

	return false
}

func checkIP(r *http.Request, w http.ResponseWriter, route string) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if !allowedIP(host) {
		Log(c.InRed("IP " + host + " is not allowed! (" + route + ")"))
		w.WriteHeader(http.StatusForbidden)

		return false
	}

	return true
}

func (gs *Gameservers) listRoute(w http.ResponseWriter, r *http.Request) {
	Log("Received list request")

	if !checkIP(r, w, "list") {
		return
	}

	gs.serversMu.Lock()

	serverInfo := make([][2]any, 0, len(gs.servers))

	for id, server := range gs.servers {
		serverInfo = append(serverInfo, [2]any{id, &server.GameserverInfo})
	}
	gs.serversMu.Unlock()

	// serverInfo = append(serverInfo, [2]any{-1, GameserverInfo{Pid: os.Getpid(), StartTime: time.Now().UnixMilli()}}) // test
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(serverInfo); err != nil {
		Log(c.InRed("Failed to encode server list: " + err.Error()))
	}
}

// Pushed status updates for a gameserver (relayed by the RCC proxy's hostping route,
// from hosted gameservers). The verb mirrors the other routes sharing /{id}:
// GET /{id} queries the status, POST /{id} pushes one. The status is the first line
// of the body
func (gs *Gameservers) statusPushRoute(w http.ResponseWriter, r *http.Request) {
	if !checkIP(r, w, "hoststatus") {
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	// statuses are a word and maybe a user ID, so anything bigger is junk
	readBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		http.Error(w, "Failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	data := strings.Split(string(readBody), "\n")
	status := data[0]

	// the push loop closes fast - statuses can arrive before startRoute's tracker even
	// registers the server, so apply it directly if the server is here, otherwise
	// defer it (Track flushes deferred statuses on registration)
	server := gs.waitForServer(id, 2*time.Second)
	if server == nil {
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d server not yet tracked, deferring status: %s", id, status)))
		gs.deferStatus(id, data)
		w.WriteHeader(http.StatusOK)

		return
	}

	gs.applyStatus(id, server, data)
}

func (gs *Gameservers) statusRoute(w http.ResponseWriter, r *http.Request) {
	if !checkIP(r, w, "status") {
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	Log(fmt.Sprintf("[status] %d request received", id))

	server, _ := gs.server(id)
	if server == nil || server.Status == Closed {
		Log(fmt.Sprintf("[status] %d not running", id))
		http.Error(w, "Gameserver not running for this ID", http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(&server.GameserverInfo); err != nil {
		Log(c.InRed(fmt.Sprintf("[status] %d failed to encode: %s", id, err.Error())))
	}
}

func idToPort(id int) int {
	return 10000 + id%proxyOffset
}

// TrackRCCJob monitors a hosted RCC gameserver purely on engine pushes:
// "Ready" (which promotes the server to Running) and periodic "Heartbeat" pings.
// The port-based probe is deliberately not used, since RCC's RakNet bindings
// aren't reliably checkable with test-bind sockets
func TrackRCCJob(server *Gameserver, id int) {
	// liveness: the hosted script pings "Heartbeat" ~every 30 seconds; stop the
	// server when nothing has been heard from it for a while
	const gracePeriod = 90 * time.Second

	port := idToPort(id)

	Log(c.InBlue(fmt.Sprintf("[track] %d rcc job - (port %05d) waiting for ready signal...", id, port)))

	// startup: wait for the hosted script's Ready push (or a clean stop)
	start := time.Now()

	for time.Since(start) < 120*time.Second {
		time.Sleep(500 * time.Millisecond)

		if server.Status == Closed {
			return
		}

		if server.Status == Running {
			Log(c.InGreen(fmt.Sprintf("[track] %d rcc job - (port %05d) engine reported ready", id, port)))
			break
		}
	}

	// if the ready signal never arrives (map failure etc.), the job silently died
	if server.Status != Running {
		Log(c.InRed(fmt.Sprintf("[track] %d rcc job - no ready signal, terminating", id)))
		server.Stop()

		return
	}

	for {
		time.Sleep(10 * time.Second)

		if server.Status == Closed {
			return
		}

		if time.Since(server.GameserverInfo.lastPushTime()) > gracePeriod {
			break
		}
	}

	Log(c.InRed(fmt.Sprintf("[track] %d rcc job - no engine activity (last push %s ago), terminating", id, time.Since(server.GameserverInfo.lastPushTime()).Round(time.Second))))
	server.Stop()
}

func proxyToClient(p *Proxy, sessKey string, sess *Session) {
	b := make([]byte, 65535)

	for {
		n2, err := sess.gs.Read(b)
		if err != nil {
			Log(c.InYellow(fmt.Sprintf("[proxy:%d] gameserver connection closed for client %s: %v", p.Port, sessKey, err)))
			return
		}

		if _, err := p.conn.WriteToUDP(b[:n2], sess.client); err != nil {
			Log(c.InRed(fmt.Sprintf("[proxy:%d] failed to forward to client %s: %v", p.Port, sess.client.String(), err)))
			return
		}

		sess.last.Store(time.Now().UnixNano())
	}
}

func registerServeScript(id int, body []byte) (string, error) {
	b := make([]byte, 16)

	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate serve token: %w", err)
	}

	token := hex.EncodeToString(b)

	serveScripts.Lock()

	serveScripts.m[id] = serveScript{
		token: token,
		body: body,
	}

	serveScripts.Unlock()

	return token, nil
}

func runProxyOnPort(p *Proxy) {
	gsPort := p.Port - proxyOffset
	sessions := make(map[string]*Session)

	var mu sync.Mutex

	deleteFromSessions := func(key string) {
		mu.Lock()
		delete(sessions, key)
		mu.Unlock()
	}

	// Periodic cleanup for idle sessions, until the listener closes
	done := make(chan struct{})
	ticker := time.NewTicker(10 * time.Second)

	defer close(done)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}

			cutoff := time.Now().Add(-idleTimeout).UnixNano()

			mu.Lock()
			for k, s := range sessions {
				if s.last.Load() >= cutoff {
					continue
				}

				Log(c.InYellow(fmt.Sprintf("[proxy:%d] timing out session %s", p.Port, k)))
				s.gs.Close()
				delete(sessions, k)
			}
			mu.Unlock()
		}
	}()

	buf := make([]byte, 65535)

	for {
		n, clientAddr, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			Log(c.InRed(fmt.Sprintf("[proxy:%d] read error: %v", p.Port, err)))
			break
		}

		// only IPv4 clients are allowed (:c)
		if clientAddr.IP.To4() == nil {
			continue
		}

		key := clientAddr.String()

		mu.Lock()

		s, exists := sessions[key]

		mu.Unlock()

		if !exists {
			gsAddr := &net.UDPAddr{
				IP: net.ParseIP("127.0.0.1"),
				Port: gsPort,
			}

			gsConn, err := net.DialUDP(proto, nil, gsAddr)
			if err != nil {
				Log(c.InRed(fmt.Sprintf("[proxy:%d] failed to dial gameserver %s: %v", p.Port, gsAddr.String(), err)))
				continue
			}

			s = &Session{
				client: &net.UDPAddr{
					IP: clientAddr.IP,
					Port: clientAddr.Port,
				},
				gs: gsConn,
			}

			s.last.Store(time.Now().UnixNano())
			mu.Lock()

			sessions[key] = s

			mu.Unlock()
			Log(c.InBlue(fmt.Sprintf("[proxy:%d] new client session %s -> 127.0.0.1:%d", p.Port, key, gsPort)))

			// start goroutine to read gameserver responses and forward to client
			go func() {
				proxyToClient(p, key, s)
				s.gs.Close()
				deleteFromSessions(key)
			}()
		}

		// forward client's packet to gameserver
		if _, err := s.gs.Write(buf[:n]); err != nil {
			Log(c.InRed(fmt.Sprintf("[proxy:%d] failed to send to gameserver for client %s: %v", p.Port, key, err)))
			s.gs.Close()
			deleteFromSessions(key)

			continue
		}

		s.last.Store(time.Now().UnixNano())
	}

	// cleanup any remaining sessions (e.g. when listener is closed)
	mu.Lock()
	for k, s := range sessions {
		Log(c.InYellow(fmt.Sprintf("[proxy:%d] closing session %s", p.Port, k)))
		s.gs.Close()
		delete(sessions, k)
	}
	mu.Unlock()
}

func (p *Proxy) Start() error {
	conn, err := net.ListenUDP(proto, &net.UDPAddr{
		IP: net.IPv4zero,
		Port: p.Port,
	})
	if err != nil {
		return fmt.Errorf("bind proxy port %d: %w", p.Port, err)
	}

	p.conn = conn

	p.wg.Go(func() {
		runProxyOnPort(p)
	})
	Log(c.InGreen(fmt.Sprintf("[proxy:%d] listener started", p.Port)))

	return nil
}

func serveInternalScript(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")

	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	serveScripts.RLock()

	s, ok := serveScripts.m[id]

	serveScripts.RUnlock()

	if !ok || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(s.token)) != 1 {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(s.body)
}

func servePublicStatus(gameservers *Gameservers) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{id}", gameservers.streamRoute) // public

	if os.Getenv("ENV") != "dev" {
		Log(c.InCyan("~ Public status server is up on port 443 ~"))

		gsDomain := fmt.Sprintf("gs.%s", os.Getenv("DOMAIN"))

		err := certmagic.HTTPS([]string{gsDomain}, mux)
		if err != nil {
			Log(c.InRed("Failed to start public status server with HTTPS: " + err.Error()))
			return
		}
	} else {
		Log(c.InCyan("~ Public status server is up on port 64992 ~"))

		if err := http.ListenAndServe(":64992", mux); err != nil {
			Log(c.InRed("Failed to start public status server: " + err.Error()))
		}
	}
}

// We don't need the launcher from setup, we're just running Studio
// (arguably we don't need the Client either, but there's gonna be so many more clients than servers it's probably worth it)
// setupDomain returns an absolute setup domain; allows e.g. http://localhost:63488 for dev
func setupDomain() string {
	domain := os.Getenv("SETUPDOMAIN")

	if found := strings.Contains(domain, "://"); !found {
		return "https://" + domain
	}

	return domain
}

func InstallSetup(version string) error {
	// http request to {SetupDomain}/version/download
	res, err := http.Get(fmt.Sprintf("%s/2013/%s", setupDomain(), version))
	if err != nil {
		return fmt.Errorf("get version from setup: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("get version %s from setup: unexpected status %s", version, res.Status)
	}

	Log(c.InPurple("Get successful, downloading and extracting..."))

	// gunzip time
	gz, err := gzip.NewReader(res.Body)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}

	versionDir := versionPathStart + version

	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return fmt.Errorf("create version directory: %w", err)
	}

	// every path goes through the root, so entries like ../x can't write outside the version directory
	root, err := os.OpenRoot(versionDir)
	if err != nil {
		return fmt.Errorf("open version directory: %w", err)
	}

	defer root.Close()

	// untar time
	for tr := tar.NewReader(gz); ; {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		switch name := filepath.FromSlash(header.Name); header.Typeflag {
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return fmt.Errorf("create directory: %w", err)
			}

			f, err := root.Create(name)
			if err != nil {
				return fmt.Errorf("create file: %w", err)
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write file: %w", err)
			}

			if err := f.Close(); err != nil {
				return fmt.Errorf("write file: %w", err)
			}
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return fmt.Errorf("create directory: %w", err)
			}
		default:
			return fmt.Errorf("unknown tar header type: %c in file %s", header.Typeflag, header.Name)
		}
	}

	Log(c.InGreen(fmt.Sprintf("Version %s downloaded and extracted successfully", version)))

	return nil
}

func LoadFromSetup() (string, error) {
	// http request to {SetupDomain}/version
	res, err := http.Get(fmt.Sprintf("%s/2013/version", setupDomain()))
	if err != nil {
		return "", fmt.Errorf("get version from setup: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get version from setup: unexpected status %s", res.Status)
	}

	verbytes, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("read version from response body: %w", err)
	}

	ver := strings.TrimSpace(string(verbytes))

	// check if ./Versions/{ver} exists
	if _, err := os.Stat(versionPathStart + ver); errors.Is(err, os.ErrNotExist) {
		Log(c.InPurple(fmt.Sprintf("Version %s not found, downloading from %s...", ver, os.Getenv("SETUPDOMAIN"))))
		return ver, InstallSetup(ver)
	}

	Log(c.InGreen(fmt.Sprintf("Version %s already exists, skipping download", ver)))

	return ver, nil
}

// siteURL is where the Site's rbxclient routes are, overridable with SITE_URL
func siteURL() string {
	if u := os.Getenv("SITE_URL"); u != "" {
		return strings.TrimSuffix(u, "/")
	}

	return fmt.Sprintf("http://www.%s", os.Getenv("DOMAIN"))
}

// fetchServeScript gets a place's serve loadscript, which the Site only gives out with the gameserver key
func fetchServeScript(id int) ([]byte, error) {
	key := os.Getenv("GAMESERVER_KEY")
	if key == "" {
		return nil, errors.New("GAMESERVER_KEY is not set")
	}

	url := fmt.Sprintf("%s/game/%d/serve", siteURL(), id)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build serve request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+key)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch serve script: %w", err)
	}

	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read serve script: %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch serve script: status %d: %s", res.StatusCode, string(body))
	}

	return body, nil
}

// composeHostScript builds an inline, RCC-safe host script. Most parameters are taken
// from the Site's serve loadscript, since it already has the right domain, map and
// authentication info embedded
func composeHostScript(id int) (string, error) {
	serveScript, err := fetchServeScript(id)
	if err != nil {
		return "", err
	}

	s := string(serveScript)
	baseURL := os.Getenv("DOMAIN")

	if resBaseURL := reBaseURL.FindStringSubmatch(s); resBaseURL != nil {
		baseURL = resBaseURL[1]
	}

	mapLocation, overridden := os.LookupEnv("RCC_MAP_LOCATION")
	if overridden && mapLocation == "none" { // "none" skips map loading (tests)
		mapLocation = ""
	} else if overridden && mapLocation != "none" {
		// explicit override, used verbatim
	} else {
		// RCC's own hosting script (gameserver.txt) loads places via the asset route,
		// not the rbxclient /game/{id} route (which is what Studio-based hosting uses)
		mapLocation = fmt.Sprintf("http://%s/asset/?id=%d", baseURL, id)
	}

	var presenceLine string

	if resPresenceURL := rePresenceURL.FindStringSubmatch(s); resPresenceURL != nil {
		// matches the serverpresence URL directly instead of assuming how the
		// loadscript spells out its Visit:SetPing call
		presenceLine = fmt.Sprintf("pcall(function() Visit:SetPing(%q, 30) end)", resPresenceURL[0])
	}

	script := strings.ReplaceAll(rccHostScript, "_BASE_URL", baseURL)
	script = strings.ReplaceAll(script, "_PLACE_ID", strconv.Itoa(id))
	script = strings.ReplaceAll(script, "_MAP_LOCATION", strconv.Quote(mapLocation))
	script = strings.ReplaceAll(script, "_SERVER_PORT", strconv.Itoa(idToPort(id)))
	script = strings.ReplaceAll(script, "_PRESENCE_PING", presenceLine)
	// the hosted server reports "Ready" directly to the proxy's hostping route as soon
	// as NetworkServer:Start succeeds - much more reliable than the port probe
	hostingURL := strings.TrimSuffix(os.Getenv("PROXY_URL"), "/")
	script = strings.ReplaceAll(script, "_HOSTPING_URL", fmt.Sprintf("%s/hostping/%d", hostingURL, id))

	Log(c.InBlue(fmt.Sprintf("[compose] %d - host script ready (%d chars): base=%q map=%q port=%d presence=%v", id, len(script), baseURL, mapLocation, idToPort(id), presenceLine != "")))

	return script, nil
}

// startGameProxy is the per-game UDP relay shared by both hosting engines
func startGameProxy(id int) (*Proxy, error) {
	proxy := &Proxy{
		Port: idToPort(id) + proxyOffset, // proxy port is offset from gameserver port by a fixed number
	}

	if err := proxy.Start(); err != nil {
		return nil, fmt.Errorf("start proxy: %w", err)
	}

	return proxy, nil
}

// NewRCCGameserver hosts a gameserver as a job on an RCCService instance, by sending
// the host script to the RCC proxy. The Proxy owns the RCC instances and load balances
// jobs between them
func NewRCCGameserver(version string, id int) (*Gameserver, error) {
	proxy, err := startGameProxy(id)
	if err != nil {
		return nil, err
	}

	script, err := composeHostScript(id)
	if err != nil {
		proxy.Stop()
		return nil, err
	}

	Log(c.InBlue(fmt.Sprintf("Sending host job %d to RCC proxy", id)))

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/host/%d", os.Getenv("PROXY_URL"), id), strings.NewReader(script))
	if err != nil {
		proxy.Stop()
		return nil, fmt.Errorf("create request to RCC proxy: %w", err)
	}

	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("PROXY_KEY"))

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		proxy.Stop()
		return nil, fmt.Errorf("send request to RCC proxy: %w", err)
	}

	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if res.StatusCode != http.StatusOK {
		proxy.Stop()
		return nil, fmt.Errorf("send request to RCC proxy: unexpected status %s", res.Status)
	}

	closeJob := func() error {
		req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/host/%d", os.Getenv("PROXY_URL"), id), nil)
		if err != nil {
			return err
		}

		req.Header.Set("Authorization", "Bearer "+os.Getenv("PROXY_KEY"))

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}

		res.Body.Close()

		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %s", res.Status)
		}

		return nil
	}

	return &Gameserver{
		GameserverInfo: GameserverInfo{
			Pid: os.Getpid(),
			Proxy: proxy,
			StartTime: time.Now().UnixMilli(),
			Status: Starting,
			statusChanged: make(chan struct{}, 100),
		},
		Cmd: nil,
		engine: engineRCC,
		stopFn: closeJob,
		done: make(chan struct{}),
	}, nil
}

func startInternalServeServer() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /serve/{id}", serveInternalScript)

	go func() {
		Log(c.InCyan("~ Internal serve script server is up on 127.0.0.1:64993 ~"))

		if err := http.ListenAndServe("127.0.0.1:64993", mux); err != nil {
			Log(c.InRed("Failed to start internal serve script server: " + err.Error()))
		}
	}()
}

func unregisterServeScript(id int) {
	serveScripts.Lock()
	delete(serveScripts.m, id)
	serveScripts.Unlock()
}

func (gs *Gameservers) closeRoute(w http.ResponseWriter, r *http.Request) {
	if !checkIP(r, w, "close") {
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	Log(fmt.Sprintf("[close] %d request received", id))

	server, _ := gs.server(id)
	if server == nil || server.Status == Closed {
		return
	}

	unregisterServeScript(id)
	server.Stop()
	Log(fmt.Sprintf("[close] %d closed", id))
}

func NewGameserver(version string, id int) (*Gameserver, error) {
	exePath := fmt.Sprintf("%s%s/%s", versionPathStart, version, exeName)

	// eh it still (kinda) makes sense to have this stat
	if _, err := os.Stat(exePath); err != nil {
		return nil, fmt.Errorf("retrieve studio executable metadata: %w", err)
	}

	proxy, err := startGameProxy(id)
	if err != nil {
		return nil, err
	}

	scriptBody, err := fetchServeScript(id)
	if err != nil {
		proxy.Stop()
		return nil, fmt.Errorf("fetch serve script: %w", err)
	}

	token, err := registerServeScript(id, scriptBody)
	if err != nil {
		proxy.Stop()
		return nil, err
	}

	args := []string{
		exePath,
		"-script",
		fmt.Sprintf(`dofile("http://127.0.0.1:64993/serve/%d?t=%s")`, id, token),
	}

	if runtime.GOOS != "windows" {
		args = append([]string{"wine"}, args...)
	}

	cmd := exec.Command(args[0], args[1:]...)

	// set stdout
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		unregisterServeScript(id)
		proxy.Stop()

		return nil, fmt.Errorf("start MercuryStudioBeta.exe: %w", err)
	}

	return &Gameserver{
		GameserverInfo: GameserverInfo{
			Pid: cmd.Process.Pid,
			Proxy: proxy,
			StartTime: time.Now().UnixMilli(),
			Status: Starting,
			statusChanged: make(chan struct{}, 100),
		},
		Cmd: cmd,
		engine: engineStudio,
		done: make(chan struct{}),
	}, nil
}

func TrackNetwork(server *Gameserver, id int) {
	if server.engine == engineRCC {
		TrackRCCJob(server, id)
		return
	}

	var up bool

	port := idToPort(id)

	Log(c.InBlue(fmt.Sprintf("[track] %d network - (port %05d) monitoring network status...", id, port)))

	start := time.Now()

	for i := 0; time.Since(start) < 30*time.Second; i++ {
		time.Sleep(100 * time.Millisecond)

		if server.Status == Closed {
			return
		}

		// the engine may have already reported itself ready (RCC-hosted servers send "Ready" through the proxy the moment NetworkServer:Start succeeds); trusting that is more reliable than the port probe itself
		if server.Status == Running {
			Log(c.InBlue(fmt.Sprintf("[track] %d engine - reported ready, skipping wait", id)))
			break
		}

		if up = CheckServerUp(port); up {
			break
		}

		if i%10 == 0 {
			Log(c.InBlue(fmt.Sprintf("[track] %d network - (port %05d) waiting for start...", id, port)))
		}
	}

	if !up && server.Status != Running {
		Log(c.InRed(fmt.Sprintf("[track] %d network - (port %05d) failed to start in time, terminating", id, port)))
		unregisterServeScript(id)
		server.Stop()

		return
	}

	Log(c.InGreen(fmt.Sprintf("[track] %d network - (port %05d) is up and running", id, port)))

	if server.Status != Running {
		server.SetStatus(Running)
	}

	for {
		time.Sleep(10 * time.Second)

		if server.Status == Closed {
			unregisterServeScript(id)
			return
		}

		if !CheckServerUp(port) {
			break
		}
	}

	Log(c.InRed(fmt.Sprintf("[track] %d network - (port %05d) appears to be down, terminating", id, port)))
	unregisterServeScript(id)
	server.Stop()
}

func (gs *Gameservers) Track(server *Gameserver, id int) {
	gs.addServer(id, server)
	gs.flushStatuses(id, server)
	Log(fmt.Sprintf("[track] %d - tracking started", id))

	go TrackNetwork(server, id)

	if server.Cmd != nil { // studio servers exit for themselves
		err := server.Cmd.Wait()

		if server.Status == Closed { // if tracked multiple times
			unregisterServeScript(id)
			return
		}

		if err != nil {
			Log(c.InRed(fmt.Sprintf("[track] %d process - exited with error %s", id, err.Error())))
		} else {
			Log(c.InYellow(fmt.Sprintf("[track] %d process - exited normally", id)))
		}
	} else { // RCC-hosted servers: wait for the tracker/orbiter to close the job
		<-server.done

		if server.Status == Closed { // if tracked multiple times
			unregisterServeScript(id)
			return
		}

		Log(c.InYellow(fmt.Sprintf("[track] %d rcc job - closed", id)))
	}

	unregisterServeScript(id)
	server.Stop() // not just SetStatus(Closed), so the game's UDP proxy releases its port
}

func (gs *Gameservers) startRoute(w http.ResponseWriter, r *http.Request) {
	if !checkIP(r, w, "start") {
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	Log(fmt.Sprintf("[start] %d request received", id))

	if s, _ := gs.server(id); s != nil && s.Status != Closed {
		return
	}

	engine := r.URL.Query().Get("engine")
	if engine == "" {
		engine = os.Getenv("DEFAULT_ENGINE")
	}

	NewFunc := NewGameserver

	if engine == engineRCC {
		NewFunc = NewRCCGameserver
	}

	server, err := NewFunc(gs.version, id)
	if err != nil {
		Log(c.InRed(fmt.Sprintf("[start] failed to start gameserver for ID %d: %s", id, err.Error())))
		http.Error(w, "Failed to start gameserver: "+err.Error(), http.StatusInternalServerError)

		return
	}

	go gs.Track(server, id)

	Log(fmt.Sprintf("[start] %d started", id))
}

func main() {
	Log(c.InYellow("Loading environment variables..."))
	Fatal(env.Load(".env"), "Failed to load environment variables. Please place them in a .env file in the current directory.")
	Log(c.InYellow("Checking for gameserver files..."))

	ver, err := LoadFromSetup()

	Fatal(err, c.InRed("Failed to load necessary gameserver files from Setup domain."))

	// if runtime.GOOS != "windows" {
	// 	Log(c.InYellow("Starting display server..."))
	// 	if err := StartDisplayServer(); err != nil {
	// 		Log(c.InRed("Failed to start display server: " + err.Error()))
	// 		os.Exit(1)
	// 	}
	// }
	Log(c.InPurple("Starting gameservers..."))

	gameservers := NewGameservers(ver)

	http.HandleFunc("GET /", gameservers.listRoute)
	http.HandleFunc("GET /{id}", gameservers.statusRoute)
	http.HandleFunc("PUT /{id}", gameservers.startRoute)
	http.HandleFunc("DELETE /{id}", gameservers.closeRoute) // idempotency!!
	http.HandleFunc("POST /{id}", gameservers.statusPushRoute)

	go servePublicStatus(gameservers)

	startInternalServeServer()
	Log(c.InGreen("~ Orbiter is up on port 64991 ~"))

	if err := http.ListenAndServe(":64991", nil); err != nil {
		Log(c.InRed("Failed to start Orbiter on port 64991: " + err.Error()))
		os.Exit(1)
	}
}
