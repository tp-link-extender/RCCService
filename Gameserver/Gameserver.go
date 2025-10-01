package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"
)

type Gameserver struct {
	*exec.Cmd
	StartTime time.Time
}

func CreateGameserver() (*Gameserver, error) {
	const path = "./staging/MercuryStudioBeta.exe"
	_, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("error stating MercuryStudioBeta.exe: %w", err)
	}

	cmd := exec.Command(path, "-console")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("error starting MercuryStudioBeta.exe: %w", err)
	}

	return &Gameserver{
		Cmd:       cmd,
		StartTime: time.Now(),
	}, nil
}

type Gameservers struct {
	servers map[string]*Gameserver
}

func NewGameservers() *Gameservers {
	return &Gameservers{
		servers: make(map[string]*Gameserver),
	}
}

func (g *Gameservers) startRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	fmt.Println("Received start request for ID:", id)

	if _, exists := g.servers[id]; exists {
		http.Error(w, "Gameserver already running for this ID", http.StatusConflict)
		return
	}

	server, err := CreateGameserver()
	if err != nil {
		http.Error(w, "Failed to start gameserver: "+err.Error(), http.StatusInternalServerError)
		return
	}

	g.servers[id] = server
	fmt.Println("Started gameserver for ID:", id)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Gameserver started"))
}

func main() {
	fmt.Println("SUP world")

	gameservers := &Gameservers{
		servers: make(map[string]*Gameserver),
	}

	http.HandleFunc("POST /start/{id}", gameservers.startRoute)

	fmt.Println("Gameserver is up on port 64991")
	if err := http.ListenAndServe(":64991", nil); err != nil {
		fmt.Println("Failed to start gameserver on port 64991:", err)
		os.Exit(1)
	}
}
