package main

import (
	"encoding/json"
	"fmt"
	"io"
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

var client http.Client

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
		return nil, fmt.Errorf("retrieve studio executable metadata: %w", err)
	}

	args := []string{
		path,
		"-script",
		fmt.Sprintf(`dofile("https://mercs.dev/game/%d/serve")`, id),
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

func (gs *Gameservers) Track(server *Gameserver, id int) {
	gs.servers[id] = server

	if err := server.Cmd.Wait(); err != nil {
		Log(c.InRed(fmt.Sprintf("Gameserver for ID %d exited with error %s", id, err.Error())))
	} else {
		Log(c.InYellow(fmt.Sprintf("Gameserver for ID %d exited normally", id)))
	}
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

func (gs *Gameservers) fileRoute(w http.ResponseWriter, r *http.Request) {
	if !checkIP(r, w, "file") {
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	Log(fmt.Sprintf("Received file request for ID: %d", id))

	req, err := http.NewRequest("GET", "https://xtcy.dev/game/"+strconv.Itoa(id), nil)
	if err != nil {
		Log(c.InRed("Failed to create request: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	req.Header.Set("Authorization", "Bearer "+os.Getenv("GAMESERVER_KEY"))

	res, err := client.Do(req)
	if err != nil {
		Log(c.InRed("Failed to send request: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		Log(c.InRed(fmt.Sprintf("Server responded with status code %d", res.StatusCode)))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if _, err = io.Copy(w, res.Body); err != nil {
		Log(c.InRed("Failed to copy response body: " + err.Error()))
		w.WriteHeader(http.StatusInternalServerError)
	}
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

	go gs.Track(server, id)

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

	server.Stop()

	delete(gs.servers, id)

	Log(fmt.Sprintf("Stopped gameserver for ID: %d", id))
	w.Write([]byte("Gameserver stopped"))
}

func forwardData(data []byte) {
	destAddr := &net.UDPAddr{
		Port: 53640,
		IP:   net.IPv4(127, 0, 0, 1),
	}
	conn, err := net.DialUDP("udp", nil, destAddr)
	if err != nil {
		Log(c.InRed("Failed to dial UDP: " + err.Error()))
		return
	}
	defer conn.Close()

	n, err := conn.Write(data)
	if err != nil {
		Log(c.InRed("Failed to write UDP data: " + err.Error()))
		return
	}
	Log(c.InGreen(fmt.Sprintf("Forwarded %d bytes to %s", n, destAddr.String())))
}

// read all UDP packets on port 53641 and forward them to 53640
func startForwarder() {
	addr := net.UDPAddr{
		Port: 53641,
		IP:   net.IPv6zero,
	}
	conn, err := net.ListenUDP("udp", &addr)
	Fatal(err, "Failed to start UDP listener on port 53641")

	defer conn.Close()
	Log(c.InBlue("UDP forwarder listening on port 53641"))

	for buf := make([]byte, 2048); ; {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			Log(c.InRed("Failed to read UDP packet: " + err.Error()))
			continue
		}

		Log(c.InYellow(fmt.Sprintf("Forwarder received %d bytes from %s", n, addr.String())))
		go forwardData(buf[:n])
	}
}

func main() {
	Log(c.InYellow("Loading environment variables..."))
	Fatal(env.Load(".env"), "Failed to load environment variables. Please place them in a .env file in the current directory.")

	Log(c.InPurple("Starting gameservers..."))
	gameservers := NewGameservers()

	http.HandleFunc("GET /", gameservers.listRoute)
	http.HandleFunc("GET /{id}", gameservers.fileRoute)
	http.HandleFunc("POST /{id}", gameservers.startRoute)
	http.HandleFunc("POST /close/{id}", gameservers.closeRoute)

	Log(c.InBlue("Starting forwarder on port 53641..."))
	go startForwarder()

	Log(c.InGreen("~ Orbiter is up on port 64991 ~"))
	Log(c.InGreen("Send a POST request to /{your place id} with the host script as the body to host a gameserver"))
	if err := http.ListenAndServe(":64991", nil); err != nil {
		Log(c.InRed("Failed to start Orbiter on port 64991: " + err.Error()))
		os.Exit(1)
	}
}
