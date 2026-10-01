package main

import (
	"archive/tar"
	"compress/gzip"
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
	"time"

	c "github.com/TwiN/go-color" // the log colours are kinda random loll whatever
	"github.com/caddyserver/certmagic"
	env "github.com/joho/godotenv"
	. "github.com/tp-link-extender/RCCService/Shared"
)

const (
	exeName          = `MercuryStudioBeta.exe`
	versionPathStart = "./Versions/version-"
)

// engines for hosting
const (
	engineStudio = "studio" // 2013 Studio hosted gameservers
	engineRCC    = "rcc"    // gameservers hosted via RCCService jobs through the Proxy
)

// We don't need the launcher from setup, we're just running Studio
// (arguably we don't need the Client either, but there's gonna be so many more clients than servers it's probably worth it)
// setupDomain returns an absolute setup domain; allows e.g. http://localhost:63488 for dev
func setupDomain() string {
	domain := os.Getenv("SETUPDOMAIN")
	if scheme := strings.Index(domain, "://"); scheme == -1 {
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
	if err := os.MkdirAll(versionDir, 0755); err != nil {
		return fmt.Errorf("create version directory: %w", err)
	}

	// untar time
	tr := tar.NewReader(gz)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		switch target := filepath.Join(versionDir, header.Name); header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("create directory: %w", err)
			}
		case tar.TypeReg:
			f, err := os.Create(target)
			if err != nil {
				return fmt.Errorf("create file: %w", err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write file: %w", err)
			}
			f.Close()
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

func checkIP(r *http.Request, w http.ResponseWriter, route string) bool {
	allowedIPs := map[string]struct{}{
		"[::1]":           {},
		"127.0.0.1":       {},
		os.Getenv("IPV4"): {},
		os.Getenv("IPV6"): {},
	}

	ip := r.RemoteAddr[:strings.LastIndex(r.RemoteAddr, ":")]
	if _, ok := allowedIPs[ip]; !ok {
		Log(c.InRed("IP " + ip + " is not allowed! (" + route + ")"))
		w.WriteHeader(http.StatusForbidden)
		return false
	}
	return true
}

const (
	proto       = "udp4"
	idleTimeout = 35 * time.Second
)

type Proxy struct {
	Port int
	conn *net.UDPConn
	wg   sync.WaitGroup
}

type Session struct {
	client *net.UDPAddr
	gs     *net.UDPConn
	last   time.Time
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
		sess.last = time.Now()
	}
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

	// Periodic cleanup for idle sessions
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			now := time.Now()
			for k, s := range sessions {
				if now.Sub(s.last) > idleTimeout {
					Log(c.InYellow(fmt.Sprintf("[proxy:%d] timing out session %s", p.Port, k)))
					s.gs.Close()
					deleteFromSessions(k)
				}
			}
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
		s, exists := sessions[key]

		if !exists {
			gsAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: gsPort}
			gsConn, err := net.DialUDP(proto, nil, gsAddr)
			if err != nil {
				Log(c.InRed(fmt.Sprintf("[proxy:%d] failed to dial gameserver %s: %v", p.Port, gsAddr.String(), err)))
				continue
			}

			s = &Session{
				client: &net.UDPAddr{IP: clientAddr.IP, Port: clientAddr.Port},
				gs:     gsConn,
				last:   time.Now(),
			}
			mu.Lock()
			sessions[key] = s
			mu.Unlock()

			// start goroutine to read gameserver responses and forward to client
			// go readWrite(key, s)
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
		s.last = time.Now()
	}

	// cleanup any remaining sessions (e.g. when listener is closed)
	for k, s := range sessions {
		Log(c.InYellow(fmt.Sprintf("[proxy:%d] closing session %s", p.Port, k)))
		s.gs.Close()
		deleteFromSessions(k)
	}
}

func (p *Proxy) Start() error {
	conn, err := net.ListenUDP(proto, &net.UDPAddr{IP: net.IPv4zero, Port: p.Port})
	if err != nil {
		return fmt.Errorf("bind proxy port %d: %w", p.Port, err)
	}
	p.conn = conn

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		runProxyOnPort(p)
	}()

	Log(c.InGreen(fmt.Sprintf("[proxy:%d] listener started", p.Port)))
	return nil
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

type Status uint8

const (
	Closed Status = iota
	Starting
	Running
)

type GameserverInfo struct {
	Pid           int `json:"pid"`
	Proxy         *Proxy
	StartTime     int64  `json:"startTime"`
	Status        Status `json:"status"`
	statusChanged chan struct{}

	statusMu sync.Mutex
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
	g.statusChanged <- struct{}{}
	if s == Closed {
		close(g.statusChanged)
	}
}

type Gameserver struct {
	GameserverInfo
	*exec.Cmd

	// "rcc" gameservers aren't backed by a process we own, but by a job on an
	// RCCService instance (managed by the Proxy). Closing them means telling the
	// Proxy to send CloseJobEx
	engine string
	stopFn func() error
	done   chan struct{}

	stopOnce sync.Once
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

	args := []string{
		exePath,
		"-script",
		fmt.Sprintf(`dofile("http://www.%s/game/%d/serve")`, os.Getenv("DOMAIN"), id),
	}
	if runtime.GOOS != "windows" {
		args = append([]string{"wine"}, args...)
	}

	cmd := exec.Command(args[0], args[1:]...)

	// set stdout
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		proxy.Stop()
		return nil, fmt.Errorf("start MercuryStudioBeta.exe: %w", err)
	}

	return &Gameserver{
		GameserverInfo: GameserverInfo{
			Pid:           cmd.Process.Pid,
			Proxy:         proxy,
			StartTime:     time.Now().UnixMilli(),
			Status:        Starting,
			statusChanged: make(chan struct{}, 100),
		},
		Cmd:    cmd,
		engine: engineStudio,
		done:   make(chan struct{}),
	}, nil
}

// RCC-safe host script. IMPORTANT: OpenJobEx script environments don't pump a
// scheduler on this RCC build - wait() and delay() never resume - so this is entirely
// inline (see gameserver.txt) and ends with RunService:Run(), which takes over the
// script forever while pumping everything else (including the reporting code the
// Proxy prepends)
const rccHostScript = `print "[Orbiter][RCC]: Starting hosted gameserver..."

local ScriptContext = game:GetService("ScriptContext")
local NetworkServer = game:GetService("NetworkServer")
local Players = game:GetService("Players")
local Visit = game:GetService("Visit")

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
	game:Load(_MAP_LOCATION)
end

Players.PlayerAdded:connect(function(player)
	print("Player " .. player.userId .. " added")
end)
Players.PlayerRemoving:connect(function(player)
	print("Player " .. player.userId .. " leaving")
end)

NetworkServer:Start(_SERVER_PORT)
_PRESENCE_PING

-- report to the Orbiter right away; anything below this line runs forever once
-- RunService:Run() takes over, and nothing past the end of the script ever executes
pcall(function() game:HttpPost("_HOSTPING_URL", "Ready", true, "text/json") end)

ScriptContext:SetTimeout(10)
ScriptContext.ScriptsDisabled = false

game:GetService("RunService"):Run()`

var (
	reBaseURL     = regexp.MustCompile(`local url = "http://" \.\. "([^"]+)"`) // the serve loadscript sets this up with the Site's DomainInsecure
	reMapLocation = regexp.MustCompile(`local mapLoc = "([^"]*)"`)
	rePresenceURL = regexp.MustCompile(`Visit:SetPing\("([^"]+)", \d+\)`)
)

// composeHostScript builds an inline, RCC-safe host script. Most parameters are taken
// from the Site's serve loadscript, since it already has the right domain, map and
// authentication info embedded
func composeHostScript(id int) (string, error) {
	siteURL := os.Getenv("SITE_URL")
	if siteURL == "" {
		siteURL = fmt.Sprintf("http://www.%s", os.Getenv("DOMAIN"))
	}
	serveURL := fmt.Sprintf("%s/game/%d/serve", siteURL, id)

	res, err := http.Get(serveURL)
	if err != nil {
		return "", fmt.Errorf("fetch serve loadscript: %w", err)
	}
	serveScript, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		return "", fmt.Errorf("read serve loadscript: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch serve loadscript: unexpected status %s", res.Status)
	}

	s := string(serveScript)

	baseURL := os.Getenv("DOMAIN")
	resBaseURL := reBaseURL.FindStringSubmatch(s)
	if resBaseURL != nil {
		baseURL = resBaseURL[1]
	}

	mapLocation, extract := os.LookupEnv("RCC_MAP_LOCATION")
	if mapLocation == "none" { // "none" disables map loading in the hosted script
		mapLocation = ""
		extract = false
	}
	if mapLocation == "" && extract { // unset = extracted from the serve script
		if resMapLocation := reMapLocation.FindStringSubmatch(s); resMapLocation != nil {
			mapLocation = resMapLocation[1]
		}
	}

	presenceLine := ""
	if resPresenceURL := rePresenceURL.FindStringSubmatch(s); resPresenceURL != nil {
		presenceLine = fmt.Sprintf("pcall(function() Visit:SetPing(%q, 30) end)", resPresenceURL[1])
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

	return script, nil
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
			Pid:           os.Getpid(),
			Proxy:         proxy,
			StartTime:     time.Now().UnixMilli(),
			Status:        Starting,
			statusChanged: make(chan struct{}, 100),
		},
		Cmd:    nil,
		engine: engineRCC,
		stopFn: closeJob,
		done:   make(chan struct{}),
	}, nil
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

type Gameservers struct {
	version     string
	servers     map[int]*Gameserver
	serverAdded chan int
}

func NewGameservers(version string) *Gameservers {
	return &Gameservers{
		version:     version,
		servers:     make(map[int]*Gameserver),
		serverAdded: make(chan int, 100),
	}
}

func CheckServerUp(port int) bool {
	// start a UDP server on the same port and see if it errors
	// gee, I sure hope this never interferes with the actual server starting
	laddr, err := net.ResolveUDPAddr(proto, fmt.Sprintf(":%d", port))
	conn, err := net.ListenUDP(proto, laddr) // gs-client communication only works on ipv4...
	if err != nil {
		return true
	}
	conn.Close()
	return false
}

const proxyOffset = 25000

func idToPort(id int) int {
	return 10000 + id%proxyOffset
}

func TrackNetwork(server *Gameserver, id int) {
	var up bool

	port := idToPort(id)
	Log(c.InBlue(fmt.Sprintf("[track] %d network - (port %05d) monitoring network status...", id, port)))

	start := time.Now()
	for i := 0; time.Since(start) < 30*time.Second; i++ {
		time.Sleep(100 * time.Millisecond)
		if server.Status == Closed {
			return
		}
		// the engine may have already reported itself ready (RCC-hosted servers
		// send "Ready" through the proxy the moment NetworkServer:Start succeeds);
		// trusting that is more reliable than the port probe itself
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
			return
		}
		if !CheckServerUp(port) {
			break
		}
	}

	Log(c.InRed(fmt.Sprintf("[track] %d network - (port %05d) appears to be down, terminating", id, port)))
	server.Stop()
}

func (gs *Gameservers) Track(server *Gameserver, id int) {
	gs.servers[id] = server
	gs.serverAdded <- id

	Log(fmt.Sprintf("[track] %d - tracking started", id))

	go TrackNetwork(server, id)

	if server.Cmd != nil { // studio servers exit for themselves
		err := server.Cmd.Wait()
		if server.Status == Closed { // if tracked multiple times
			return
		}

		if err != nil {
			Log(c.InRed(fmt.Sprintf("[track] %d process - exited with error %s", id, err.Error())))
		} else {
			Log(c.InYellow(fmt.Sprintf("[track] %d process - exited normally", id)))
		}
	} else { // RCC-hosted servers: wait for the tracker/orbiter to close the job
		select {
		case <-server.done:
		}
		if server.Status == Closed { // if tracked multiple times
			return
		}
		Log(c.InYellow(fmt.Sprintf("[track] %d rcc job - closed", id)))
	}
	server.SetStatus(Closed)
}

func (gs *Gameservers) listRoute(w http.ResponseWriter, r *http.Request) {
	Log("Received list request")
	if !checkIP(r, w, "list") {
		return
	}

	serverInfo := make([][2]any, 0, len(gs.servers))
	for id, server := range gs.servers {
		serverInfo = append(serverInfo, [2]any{id, &server.GameserverInfo})
	}
	// serverInfo = append(serverInfo, [2]any{-1, GameserverInfo{Pid: os.Getpid(), StartTime: time.Now().UnixMilli()}}) // test

	if err := json.NewEncoder(w).Encode(serverInfo); err != nil {
		http.Error(w, "Failed to encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
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

	server, exists := gs.servers[id]
	if !exists || server.Status == Closed {
		Log(fmt.Sprintf("[status] %d not running", id))
		http.Error(w, "Gameserver not running for this ID", http.StatusNotFound)
		return
	}

	if err := json.NewEncoder(w).Encode(&server.GameserverInfo); err != nil {
		http.Error(w, "Failed to encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
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

	if s, exists := gs.servers[id]; exists && s.Status != Closed {
		return
	}

	engine := r.URL.Query().Get("engine")
	if engine == "" {
		engine = os.Getenv("DEFAULT_ENGINE")
	}

	var NewFunc = NewGameserver
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

func (gs *Gameservers) closeRoute(w http.ResponseWriter, r *http.Request) {
	if !checkIP(r, w, "close") {
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
	}

	Log(fmt.Sprintf("[close] %d request received", id))

	server, exists := gs.servers[id]
	if !exists || server.Status == Closed {
		return
	}

	server.Stop()

	Log(fmt.Sprintf("[close] %d closed", id))
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

	readBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data := strings.Split(string(readBody), "\n")
	status := data[0]

	server, exists := gs.servers[id]

	switch status {
	case "Ready":
		Log(c.InGreen(fmt.Sprintf("[hoststatus] %d server is ready", id)))
		if exists && server.Status == Starting {
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
	case "Closed":
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d server announced its own closure", id)))
		if exists {
			server.Stop()
		}
	default:
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d unknown status: %s", id, status)))
	}
}

func (gs *Gameservers) streamRoute(w http.ResponseWriter, r *http.Request) {
	// don't check the IP, this route is public
	w.Header().Set("Access-Control-Allow-Origin", "*")

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	Log(fmt.Sprintf("[stream] %d request received", id))

	// Return events from statusChanged as SSE
	const wait = 30 * time.Second

	server, exists := gs.servers[id]
	if !exists {
		// wait for server to be started
		Log(fmt.Sprintf("[stream] %d waiting for server to be started", id))

		start := time.Now()
	loop:
		for time.Since(start) < wait {
			select {
			case <-gs.serverAdded:
				server, exists = gs.servers[id]
				if exists {
					Log(fmt.Sprintf("[stream] %d server started, proceeding", id))
					break loop
				}
			case <-time.After(wait):
				break loop
			}
		}
	}
	if !exists {
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

	Log(c.InGreen("~ Orbiter is up on port 64991 ~"))
	if err := http.ListenAndServe(":64991", nil); err != nil {
		Log(c.InRed("Failed to start Orbiter on port 64991: " + err.Error()))
		os.Exit(1)
	}
}
