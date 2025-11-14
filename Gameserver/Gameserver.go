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
	Starting Status = iota
	Running
	Closed
)

type GameserverInfo struct {
	Pid       int    `json:"pid"`
	StartTime int64  `json:"startTime"`
	Status    Status `json:"status"`
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
			Pid:       cmd.Process.Pid,
			StartTime: time.Now().UnixMilli(),
		},
		Cmd: cmd,
	}, nil
}

func (g *Gameserver) Stop() error {
	return g.Process.Kill()
}

type Gameservers struct {
	servers map[int]*Gameserver
}

func NewGameservers() *Gameservers {
	return &Gameservers{
		servers: make(map[int]*Gameserver),
	}
}

func CheckServerUp() bool {
	const port = 53640

	// start a UDP server on the same port and see if it errors
	addr := fmt.Sprintf(":%d", port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return true
	}
	conn.Close()
	return false
}

func TrackNetwork(server *Gameserver, id int) {
	var up bool

	start := time.Now()
	for i := 0; time.Since(start) < 30*time.Second; i++ {
		time.Sleep(100 * time.Millisecond)
		if server.Status == Closed {
			return
		}
		up = CheckServerUp()
		if up {
			break
		}
		if i%10 == 0 {
			Log(c.InBlue(fmt.Sprintf("[track] %d network - waiting for start...", id)))
		}
	}

	if !up {
		Log(c.InRed(fmt.Sprintf("[track] %d network - failed to start in time, terminating", id)))
		server.Stop()
		server.Status = Closed
		return
	}

	Log(c.InGreen(fmt.Sprintf("[track] %d network - is up and running", id)))

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
	server.Status = Closed
}

func (gs *Gameservers) Track(server *Gameserver, id int) {
	gs.servers[id] = server

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
	server.Status = Closed
	delete(gs.servers, id)
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

	// server, exists := gs.servers[id]
	server, exists := gs.servers[id]
	if !exists {
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

	if _, exists := gs.servers[id]; exists {
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
	if !exists {
		return
	}

	server.Stop()

	delete(gs.servers, id)

	Log(fmt.Sprintf("[close] %d closed", id))
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

	// the forwarder don't actually work 😭 (it seems to work normally anywayso)

	Log(c.InGreen("~ Orbiter is up on port 64991 ~"))
	if err := http.ListenAndServe(":64991", nil); err != nil {
		Log(c.InRed("Failed to start Orbiter on port 64991: " + err.Error()))
		os.Exit(1)
	}
}
