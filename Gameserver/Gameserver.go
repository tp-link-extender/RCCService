package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
)

type Gameserver struct {
	*exec.Cmd
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
		Cmd: cmd,
	}, nil
}

func startRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	fmt.Println("Received start request for ID:", id)
}

func main() {
	fmt.Println("SUP world")

	http.HandleFunc("POST /start/{id}", startRoute)

	fmt.Println("Gameserver is up on port 64991")
	if err := http.ListenAndServe(":64991", nil); err != nil {
		fmt.Println("Failed to start gameserver on port 64991:", err)
		os.Exit(1)
	}
}
