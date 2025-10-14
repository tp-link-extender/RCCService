package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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
	if ip := r.RemoteAddr[:strings.LastIndex(r.RemoteAddr, ":")]; ip != os.Getenv("IP") && ip != "[::1]" {
		Log(c.InRed("IP " + ip + " is not allowed! (" + route + ")"))
		w.WriteHeader(http.StatusForbidden)
		return false
	}
	return true
}

type GameserverInfo struct {
	Pid       int   `json:"pid"`
	StartTime int64 `json:"startTime"`
}

type Gameserver struct {
	GameserverInfo
	*exec.Cmd
}

func NewGameserver(id int) (*Gameserver, error) {
	const path = `./staging/MercuryStudioBeta.exe`
	_, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("error starting MercuryStudioBeta.exe: %w", err)
	}

	cmd := exec.Command(path, "-script", fmt.Sprintf("https://xtcy.dev/game/%d/serve", id))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("error starting MercuryStudioBeta.exe: %w", err)
	}

	return &Gameserver{
		GameserverInfo: GameserverInfo{
			Pid:       cmd.Process.Pid,
			StartTime: time.Now().UnixMilli(),
		},
		Cmd: cmd,
	}, nil
}

func (g *Gameserver) StopGameserver() error {
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

func (gs *Gameservers) startRoute(w http.ResponseWriter, r *http.Request) {
	if !checkIP(r, w, "start") {
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	Log(fmt.Sprintf("Received start request for ID: %d", id))

	if _, exists := gs.servers[id]; exists {
		http.Error(w, "Gameserver already running for this ID", http.StatusConflict)
		return
	}

	server, err := NewGameserver(id)
	if err != nil {
		http.Error(w, "Failed to start gameserver: "+err.Error(), http.StatusInternalServerError)
		return
	}

	gs.servers[id] = server
	Log(fmt.Sprintf("Started gameserver for ID: %d", id))
	w.Write([]byte("Gameserver started"))
}

func (gs *Gameservers) closeRoute(w http.ResponseWriter, r *http.Request) {
	if !checkIP(r, w, "close") {
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
	}

	Log(fmt.Sprintf("Received close request for ID: %d", id))

	server, exists := gs.servers[id]

	if !exists {
		http.Error(w, "Gameserver not running for this ID", http.StatusNotFound)
		return
	}

	server.StopGameserver()

	Log(fmt.Sprintf("Stopped gameserver for ID: %d", id))
	w.Write([]byte("Gameserver stopped"))
}

func main() {
	Log(c.InYellow("Loading environment variables..."))
	Fatal(env.Load(".env"), "Failed to load environment variables. Please place them in a .env file in the current directory.")

	Log(c.InPurple("Starting gameservers..."))
	gameservers := NewGameservers()

	http.HandleFunc("GET /", gameservers.listRoute)
	http.HandleFunc("POST /{id}", gameservers.startRoute)
	http.HandleFunc("POST /close/{id}", gameservers.closeRoute)

	Log(c.InGreen("~ Orbiter is up on port 64990 ~"))
	Log(c.InGreen("Send a POST request to /{your place id} with the host script as the body to host a gameserver"))
	if err := http.ListenAndServe(":64991", nil); err != nil {
		Log(c.InRed("Failed to start Orbiter on port 64991: " + err.Error()))
		os.Exit(1)
	}
}
