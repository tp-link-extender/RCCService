package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

type Gameserver struct {
	*exec.Cmd
	StartTime time.Time
}

func NewGameserver(id int) (*Gameserver, error) {
	const path = `./staging/MercuryStudioBeta.exe`
	_, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("error starting MercuryStudioBeta.exe: %w", err)
	}

	cmd := exec.Command(path, "-fileLocation", `C:\Users\alfee\Documents\GitHub\RCCService\Gameserver\places\`+strconv.Itoa(id)+`.rbxl`, "-script", "http://xtcy.dev/game/host?ticket=l5wty9tmqk5hj2cairef")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("error starting MercuryStudioBeta.exe: %w", err)
	}

	return &Gameserver{
		Cmd:       cmd,
		StartTime: time.Now(),
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
	fmt.Println("Received list request")

	if err := json.NewEncoder(w).Encode(gs.servers); err != nil {
		http.Error(w, "Failed to encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
}

func (gs *Gameservers) startRoute(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	fmt.Println("Received start request for ID:", id)

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
	fmt.Println("Started gameserver for ID:", id)
	w.Write([]byte("Gameserver started"))
}

func (gs *Gameservers) closeRoute(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
	}

	fmt.Println("Received close request for ID:", id)

	server, exists := gs.servers[id]

	if !exists {
		http.Error(w, "Gameserver not running for this ID", http.StatusNotFound)
		return
	}

	server.StopGameserver()

	fmt.Println("Stopped gameserver for ID: ", id)
	w.Write([]byte("Gameserver stopped"))
}

func main() {
	fmt.Println("SUP world")

	gameservers := NewGameservers()

	http.HandleFunc("GET /", gameservers.listRoute)
	http.HandleFunc("POST /start/{id}", gameservers.startRoute)
	http.HandleFunc("POST /close/{id}", gameservers.closeRoute)

	fmt.Println("Orbiter is up on port 64991")
	if err := http.ListenAndServe(":64991", nil); err != nil {
		fmt.Println("Failed to start orbiter on port 64991:", err)
		os.Exit(1)
	}
}
