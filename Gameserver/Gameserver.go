package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	c "github.com/TwiN/go-color"
	"github.com/caddyserver/certmagic"
	env "github.com/joho/godotenv"
)

func Log(txt string) {
	// Hey, Go date formatting isn't so bad
	fmt.Println(time.Now().Format("2006/01/02, 15:04:05 "), txt)
}

func Fatal(err error, txt string) {
	// so that I don't have to write this every time
	if err == nil {
		return
	}
	fmt.Println(err)
	Log(c.InRed(txt))
	os.Exit(1)
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

type Status uint8

const (
	Closed Status = iota
	Starting
	Running
)

type GameserverInfo struct {
	Pid           int    `json:"pid"`
	StartTime     int64  `json:"startTime"`
	Status        Status `json:"status"`
	statusChanged chan struct{}
}

func (g *GameserverInfo) SetStatus(s Status) {
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
}

func NewGameserver(id int) (*Gameserver, error) {
	const path = `./staging/MercuryStudioBeta.exe`
	_, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("retrieve studio executable metadata: %w", err)
	}

	args := []string{
		path,
		"-script",
		fmt.Sprintf(`dofile("http://mercs.dev/game/%d/serve")`, id),
	}
	if runtime.GOOS != "windows" {
		args = append([]string{"wine"}, args...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MercuryStudioBeta.exe: %w", err)
	}

	return &Gameserver{
		GameserverInfo: GameserverInfo{
			Pid:           cmd.Process.Pid,
			StartTime:     time.Now().UnixMilli(),
			Status:        Starting,
			statusChanged: make(chan struct{}, 100),
		},
		Cmd: cmd,
	}, nil
}

func (g *Gameserver) Stop() error {
	g.SetStatus(Closed)
	return g.Process.Kill()
}

type Gameservers struct {
	servers     map[int]*Gameserver
	serverAdded chan int
}

func NewGameservers() *Gameservers {
	return &Gameservers{
		servers:     make(map[int]*Gameserver),
		serverAdded: make(chan int, 100),
	}
}

func CheckServerUp() bool {
	const port = 53640
	const proto = "udp4"

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

func TrackNetwork(server *Gameserver, id int) {
	var up bool

	Log(c.InBlue(fmt.Sprintf("[track] %d network - monitoring network status...", id)))

	start := time.Now()
	for i := 0; time.Since(start) < 30*time.Second; i++ {
		time.Sleep(100 * time.Millisecond)
		if server.Status == Closed {
			return
		}
		if up = CheckServerUp(); up {
			break
		}
		if i%10 == 0 {
			Log(c.InBlue(fmt.Sprintf("[track] %d network - waiting for start...", id)))
		}
	}

	if !up {
		Log(c.InRed(fmt.Sprintf("[track] %d network - failed to start in time, terminating", id)))
		server.Stop()
		return
	}

	Log(c.InGreen(fmt.Sprintf("[track] %d network - is up and running", id)))
	server.SetStatus(Running)

	for {
		time.Sleep(10 * time.Second)
		if server.Status == Closed {
			return
		}
		if !CheckServerUp() {
			break
		}
	}

	Log(c.InRed(fmt.Sprintf("[track] %d network - appears to be down, terminating", id)))
	server.Stop()
}

func (gs *Gameservers) Track(server *Gameserver, id int) {
	gs.servers[id] = server
	gs.serverAdded <- id

	Log(fmt.Sprintf("[track] %d - tracking started", id))

	go TrackNetwork(server, id)

	err := server.Cmd.Wait()
	if server.Status == Closed {
		return
	}

	if err != nil {
		Log(c.InRed(fmt.Sprintf("[track] %d process - exited with error %s", id, err.Error())))
	} else {
		Log(c.InYellow(fmt.Sprintf("[track] %d process - exited normally", id)))
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
		serverInfo = append(serverInfo, [2]any{id, server.GameserverInfo})
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

	if err := json.NewEncoder(w).Encode(server.GameserverInfo); err != nil {
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

	server, err := NewGameserver(id)
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
		Log(c.InPurple("~ Public status server is up on port 443 ~"))
		err := certmagic.HTTPS([]string{"gs.mercs.dev"}, mux)
		if err != nil {
			Log(c.InRed("Failed to start public status server with HTTPS: " + err.Error()))
			return
		}
	} else {
		Log(c.InPurple("~ Public status server is up on port 64992 ~"))
		if err := http.ListenAndServe(":64992", mux); err != nil {
			Log(c.InRed("Failed to start public status server: " + err.Error()))
		}
	}
}

func main() {
	Log(c.InYellow("Loading environment variables..."))
	Fatal(env.Load(".env"), "Failed to load environment variables. Please place them in a .env file in the current directory.")

	Log(c.InPurple("Starting gameservers..."))
	gameservers := NewGameservers()

	http.HandleFunc("GET /", gameservers.listRoute)
	http.HandleFunc("GET /{id}", gameservers.statusRoute)
	http.HandleFunc("PUT /{id}", gameservers.startRoute)
	http.HandleFunc("DELETE /{id}", gameservers.closeRoute) // idempotency!!

	go servePublicStatus(gameservers)
	// the forwarder don't actually work 😭 (it seems to work normally anywayso)

	Log(c.InGreen("~ Orbiter is up on port 64991 ~"))
	if err := http.ListenAndServe(":64991", nil); err != nil {
		Log(c.InRed("Failed to start Orbiter on port 64991: " + err.Error()))
		os.Exit(1)
	}
}
