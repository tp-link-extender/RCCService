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
	"sync"
	"time"

	c "github.com/TwiN/go-color"
	. "github.com/tp-link-extender/RCCService/Shared"
)

const (
	exeName = `MercuryStudioBeta.exe`

	// how long a tracker waits for a studio server's port, or an RCC job's ready signal
	studioStartTimeout = 30 * time.Second
	rccReadyTimeout = 120 * time.Second

	// a hosted RCC server is only stopped once the engine has been silent this long. Its
	// heartbeat arrives about every 30 seconds
	rccGracePeriod = 90 * time.Second
)

// Status is a gameserver's state, as the Site reads it: 0 closed, 1 starting, 2 running
type Status uint8

const (
	Closed Status = iota
	Starting
	Running
)

// gameserver is one hosted place, either a Studio process or an RCC job
type gameserver struct {
	id int
	pid int
	startTime int64
	engine string
	relay *udpRelay

	cmd *exec.Cmd // studio only
	stopFn func() error
	serveToken string // studio only, the token its serve script is fetched with

	done chan struct{} // closed once the server has been stopped
	stopOnce sync.Once

	mu sync.Mutex // guards the fields below
	state Status
	lastPush time.Time // any push from the engine walks this forward
	changed chan struct{} // closed and replaced on every state change
}

// gameserverInfo is one entry of the Site's gameserver list. Only these fields leave the Orbiter
type gameserverInfo struct {
	Pid int `json:"pid"`
	StartTime int64 `json:"startTime"`
	Status Status `json:"status"`
}

func newGameserver(id int, engine string, relay *udpRelay) *gameserver {
	return &gameserver{
		id: id,
		pid: os.Getpid(),
		startTime: time.Now().UnixMilli(),
		engine: engine,
		relay: relay,
		done: make(chan struct{}),
		state: Starting,
		changed: make(chan struct{}),
	}
}

// status reads the state under the server's lock
func (s *gameserver) status() Status {
	s.mu.Lock()

	defer s.mu.Unlock()

	return s.state
}

// watch returns the current state and a channel closed on its next change
func (s *gameserver) watch() (Status, <-chan struct{}) {
	s.mu.Lock()

	defer s.mu.Unlock()

	return s.state, s.changed
}

// info reads what the Site's list needs
func (s *gameserver) info() gameserverInfo {
	s.mu.Lock()

	defer s.mu.Unlock()

	return gameserverInfo{Pid: s.pid, StartTime: s.startTime, Status: s.state}
}

// setStatus moves the server to next, and reports false once the server is closed, which never reopens
func (s *gameserver) setStatus(next Status) bool {
	s.mu.Lock()

	defer s.mu.Unlock()

	if s.state == Closed {
		return false
	}

	Log(fmt.Sprintf("[status] %d changed: %d -> %d", s.id, s.state, next))

	s.state = next

	if next == Starting || next == Running {
		s.lastPush = time.Now() // covers statuses that constitute liveliness
	}

	close(s.changed)

	s.changed = make(chan struct{})

	return true
}

// touch records that the engine has pushed something, which proves it's alive
func (s *gameserver) touch() {
	s.mu.Lock()

	defer s.mu.Unlock()

	s.lastPush = time.Now()
}

func (s *gameserver) lastPushTime() time.Time {
	s.mu.Lock()

	defer s.mu.Unlock()

	return s.lastPush
}

// stop closes the server: its state moves to Closed, then the process or job, and the relay
// are shut down. Calling it again does nothing
func (s *gameserver) stop() {
	if !s.setStatus(Closed) {
		return
	}

	s.stopOnce.Do(func() {
		if s.stopFn != nil {
			if err := s.stopFn(); err != nil {
				Log(c.InRed(fmt.Sprintf("[close] %d failed to close RCC job: %s", s.id, err.Error())))
			}
		}

		if s.cmd != nil && s.cmd.Process != nil {
			s.cmd.Process.Kill()
		}

		s.relay.close()

		close(s.done)
	})
}

// gameserverByID returns the registered server for id (nil if none), and a channel closed when the next server is registered
func (o *orbiter) gameserverByID(id int) (*gameserver, <-chan struct{}) {
	o.gsMu.Lock()

	defer o.gsMu.Unlock()

	return o.servers[id], o.added
}

// waitForServer returns the server registered for id, waiting up to timeout for one
func (o *orbiter) waitForServer(id int, timeout time.Duration) *gameserver {
	deadline := time.After(timeout)

	for {
		server, added := o.gameserverByID(id)
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

// reserveStart claims the start of a place. It reports false when the place is already running or starting
func (o *orbiter) reserveStart(id int) bool {
	o.gsMu.Lock()

	defer o.gsMu.Unlock()

	if s := o.servers[id]; s != nil && s.status() != Closed {
		return false
	}

	if _, busy := o.starting[id]; busy {
		return false
	}

	o.starting[id] = struct{}{}

	return true
}

// abandonStart releases a claimed start that failed
func (o *orbiter) abandonStart(id int) {
	o.gsMu.Lock()

	defer o.gsMu.Unlock()

	delete(o.starting, id)
}

// registerServer makes a started server visible and returns the statuses that arrived before it
func (o *orbiter) registerServer(id int, s *gameserver) [][]string {
	o.gsMu.Lock()

	defer o.gsMu.Unlock()

	o.servers[id] = s

	delete(o.starting, id)

	close(o.added)

	o.added = make(chan struct{})

	deferred := o.pending[id]

	delete(o.pending, id)

	return deferred
}

// removeServer forgets a server once its tracker has finished with it. A newer server under the same id is left alone
func (o *orbiter) removeServer(id int, s *gameserver) {
	o.gsMu.Lock()

	defer o.gsMu.Unlock()

	if o.servers[id] == s {
		delete(o.servers, id)
	}
}

// deferOrGet returns the registered server for id, or else stores the status to apply once one is registered (nil)
func (o *orbiter) deferOrGet(id int, data []string) *gameserver {
	o.gsMu.Lock()

	defer o.gsMu.Unlock()

	if s := o.servers[id]; s != nil {
		return s
	}

	o.pending[id] = append(o.pending[id], data)

	return nil
}

// applyStatus acts on a status pushed by a hosted gameserver. The first line of data is the status
func (o *orbiter) applyStatus(s *gameserver, data []string) {
	status := data[0]

	// any push from the hosted script proves the engine is alive
	s.touch()

	switch status {
	case "Ready":
		Log(c.InGreen(fmt.Sprintf("[hoststatus] %d server is ready", s.id)))

		if s.status() == Starting {
			s.setStatus(Running)
		}
	case "Loaded":
		Log(c.InGreen(fmt.Sprintf("[hoststatus] %d host script loaded", s.id)))
	case "Heartbeat":
	case "PlayerAdded":
		var userID string

		if len(data) > 1 {
			userID = data[1]
		}

		Log(c.InGreen(fmt.Sprintf("[hoststatus] %d player joined: %s", s.id, userID)))
	case "PlayersLeft":
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d player left", s.id)))
	case "Unoccupied":
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d server unoccupied, closing job", s.id)))
		s.stop()
	case "Closed":
		Log(c.InYellow(fmt.Sprintf("[hoststatus] %d server announced its own closure", s.id)))
		s.stop()
	}
}

// listRoute serves the Site's gameserver list: [[placeId, {pid, startTime, status}], ...]
func (o *orbiter) listRoute(w http.ResponseWriter, r *http.Request) {
	o.gsMu.Lock()

	list := make([][2]any, 0, len(o.servers))

	for id, s := range o.servers {
		if info := s.info(); info.Status != Closed {
			list = append(list, [2]any{id, info})
		}
	}

	o.gsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(list); err != nil {
		Log(c.InRed("Failed to encode server list: " + err.Error()))
	}
}

// hostPingRoute takes a hosted gameserver's status on the loopback listener. Its body is the
// status line, then an optional user id
func (o *orbiter) hostPingRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := gameserverID(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hostStatusLimit))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read status: "+err.Error())

		return
	}

	data := strings.Split(string(body), "\n")

	if !knownHostStatuses[data[0]] {
		writeError(w, http.StatusBadRequest, "unknown status "+strconv.Quote(data[0]))

		return
	}

	Log(c.InGreen(fmt.Sprintf("Host job %d pinged: %s", id, data[0])))

	// a status can beat its server's registration, so it waits briefly, or is kept for later
	server := o.waitForServer(id, serverWait)
	if server == nil {
		if server = o.deferOrGet(id, data); server == nil {
			Log(c.InYellow(fmt.Sprintf("[hoststatus] %d server not yet tracked, deferring status: %s", id, data[0])))
			w.WriteHeader(http.StatusOK)

			return
		}
	}

	o.applyStatus(server, data)
	w.WriteHeader(http.StatusOK)
}

// startRoute starts a gameserver. Starting a place that's already running, or starting, is not an error
func (o *orbiter) startRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := gameserverID(w, r)
	if !ok {
		return
	}

	engine := r.URL.Query().Get("engine")
	if engine == "" {
		engine = o.cfg.DefaultEngine
	}

	if engine != engineStudio && engine != engineRCC {
		writeError(w, http.StatusBadRequest, "engine must be studio or rcc")

		return
	}

	Log(fmt.Sprintf("[start] %d request received (%s)", id, engine))

	if !o.reserveStart(id) {
		Log(fmt.Sprintf("[start] %d already running or starting", id))
		w.WriteHeader(http.StatusAccepted)

		return
	}

	server, err := o.newServer(engine, id)
	if err != nil {
		o.abandonStart(id)

		Log(c.InRed(fmt.Sprintf("[start] failed to start gameserver for ID %d: %s", id, err.Error())))
		writeError(w, http.StatusInternalServerError, "failed to start gameserver: "+err.Error())

		return
	}

	for _, data := range o.registerServer(id, server) {
		o.applyStatus(server, data)
	}

	go o.track(server, id)

	Log(fmt.Sprintf("[start] %d started", id))
	w.WriteHeader(http.StatusAccepted)
}

// closeRoute closes a gameserver. Closing one that isn't running also succeeds
func (o *orbiter) closeRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := gameserverID(w, r)
	if !ok {
		return
	}

	Log(fmt.Sprintf("[close] %d request received", id))

	if server, _ := o.gameserverByID(id); server != nil {
		server.stop()
		Log(fmt.Sprintf("[close] %d closed", id))
	}

	w.WriteHeader(http.StatusNoContent)
}

// streamRoute is the public status stream: an SSE event per state change until the server is running
func (o *orbiter) streamRoute(w http.ResponseWriter, r *http.Request) {
	// don't check the IP, this route is public
	w.Header().Set("Access-Control-Allow-Origin", "*")

	id, ok := gameserverID(w, r)
	if !ok {
		return
	}

	Log(fmt.Sprintf("[stream] %d request received", id))

	server := o.waitForServer(id, streamWait)
	if server == nil {
		writeError(w, http.StatusNotFound, "gameserver not found for this ID")

		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")

		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	enc := json.NewEncoder(w)

	for {
		status, changed := server.watch()

		w.Write([]byte("data: "))
		enc.Encode(status)
		w.Write([]byte{'\n'})
		flusher.Flush()

		if status != Starting {
			return
		}

		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

// newServer starts a gameserver on an engine
func (o *orbiter) newServer(engine string, id int) (*gameserver, error) {
	if engine == engineRCC {
		return o.newRCCServer(id)
	}

	return o.newStudioServer(id)
}

// newStudioServer starts a 2013 Studio process with the place's serve script
func (o *orbiter) newStudioServer(id int) (*gameserver, error) {
	exePath := fmt.Sprintf("%s%s/%s", versionPathStart, o.version, exeName)

	// eh it still (kinda) makes sense to have this stat
	if _, err := os.Stat(exePath); err != nil {
		return nil, fmt.Errorf("retrieve studio executable metadata: %w", err)
	}

	relay, err := startRelay(id)
	if err != nil {
		return nil, err
	}

	scriptBody, err := o.fetchServeScript(id)
	if err != nil {
		relay.close()

		return nil, fmt.Errorf("fetch serve script: %w", err)
	}

	token, err := o.registerServeScript(id, scriptBody)
	if err != nil {
		relay.close()

		return nil, err
	}

	argv := []string{exePath, "-script", fmt.Sprintf(`dofile("http://127.0.0.1:%d/serve/%d?t=%s")`, loopbackPort, id, token)}

	if runtime.GOOS != "windows" {
		argv = append([]string{"wine"}, argv...)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		o.removeServeScript(id, token)
		relay.close()

		return nil, fmt.Errorf("start MercuryStudioBeta.exe: %w", err)
	}

	server := newGameserver(id, engineStudio, relay)

	server.cmd = cmd
	server.pid = cmd.Process.Pid
	server.serveToken = token

	return server, nil
}

// newRCCServer hosts a place as a job on an RCCService instance, through the pool
func (o *orbiter) newRCCServer(id int) (*gameserver, error) {
	relay, err := startRelay(id)
	if err != nil {
		return nil, err
	}

	script, err := o.composeHostScript(id)
	if err != nil {
		relay.close()

		return nil, err
	}

	jobID := strconv.Itoa(id)

	Log(c.InBlue(fmt.Sprintf("Sending host job %d to RCC", id)))

	if err := o.pool.Submit(jobID, "host", o.hostJobSOAP(id, script)); err != nil {
		relay.close()

		return nil, fmt.Errorf("start RCC job: %w", err)
	}

	server := newGameserver(id, engineRCC, relay)

	server.stopFn = func() error {
		o.pool.CloseJob(jobID)

		return nil
	}

	return server, nil
}

// hostJobSOAP is the OpenJobEx request for a hosted place: the reporting epilogue first, then the host script
func (o *orbiter) hostJobSOAP(id int, script string) string {
	epilogue := strings.ReplaceAll(hostEpilogue, "_PING_URL", hostpingURL(id))
	full := epilogue + "\n\n" + script

	soap := strings.ReplaceAll(hostTemplate, "_TASK_ID", xmlText.Replace(strconv.Itoa(id)))
	soap = strings.ReplaceAll(soap, "_EXPIRATION", o.pool.expiration) // not the raw env, which uses 0 for "never"
	soap = strings.ReplaceAll(soap, "_HOST_SCRIPT", xmlText.Replace(full)+"  ")

	return soap
}

// track watches a gameserver until it ends, then tears it down and forgets it
func (o *orbiter) track(s *gameserver, id int) {
	Log(fmt.Sprintf("[track] %d - tracking started (%s)", id, s.engine))

	go o.trackNetwork(s, id)

	if s.cmd != nil {
		// studio servers exit for themselves
		err := s.cmd.Wait()

		if err != nil {
			Log(c.InRed(fmt.Sprintf("[track] %d process - exited with error %s", id, err.Error())))
		} else {
			Log(c.InYellow(fmt.Sprintf("[track] %d process - exited normally", id)))
		}
	} else {
		// RCC-hosted servers wait for the trackers, or a close, to end the job
		<-s.done

		Log(c.InYellow(fmt.Sprintf("[track] %d rcc job - closed", id)))
	}

	o.removeServeScript(id, s.serveToken)

	// not just setStatus(Closed): the relay and the job must be released too
	s.stop()
	o.removeServer(id, s)
}

// trackNetwork decides when a server is up, and when it has gone down
func (o *orbiter) trackNetwork(s *gameserver, id int) {
	if s.engine == engineRCC {
		o.trackRCCJob(s, id)

		return
	}

	port := idToPort(id)

	Log(c.InBlue(fmt.Sprintf("[track] %d network - (port %05d) monitoring network status...", id, port)))

	var up bool

	start := time.Now()

	for i := 0; time.Since(start) < studioStartTimeout; i++ {
		time.Sleep(100 * time.Millisecond)

		if s.status() == Closed {
			return
		}

		if up = CheckServerUp(port); up {
			break
		}

		if i%10 == 0 {
			Log(c.InBlue(fmt.Sprintf("[track] %d network - (port %05d) waiting for start...", id, port)))
		}
	}

	if !up && s.status() != Running {
		Log(c.InRed(fmt.Sprintf("[track] %d network - (port %05d) failed to start in time, terminating", id, port)))
		s.stop()

		return
	}

	Log(c.InGreen(fmt.Sprintf("[track] %d network - (port %05d) is up and running", id, port)))

	if s.status() != Running {
		s.setStatus(Running)
	}

	for {
		time.Sleep(10 * time.Second)

		if s.status() == Closed {
			return
		}

		if !CheckServerUp(port) {
			break
		}
	}

	Log(c.InRed(fmt.Sprintf("[track] %d network - (port %05d) appears to be down, terminating", id, port)))
	s.stop()
}

// trackRCCJob monitors a hosted RCC gameserver purely on engine pushes: "Ready" (which promotes
// the server to Running) and periodic "Heartbeat" pings. The port probe isn't used, since RCC's
// RakNet bindings aren't reliably checkable with test-bind sockets
func (o *orbiter) trackRCCJob(s *gameserver, id int) {
	port := idToPort(id)

	Log(c.InBlue(fmt.Sprintf("[track] %d rcc job - (port %05d) waiting for ready signal...", id, port)))

	// startup: wait for the hosted script's Ready push (or a clean stop)
	start := time.Now()

	for time.Since(start) < rccReadyTimeout {
		time.Sleep(500 * time.Millisecond)

		status := s.status()
		if status == Closed {
			return
		}

		if status == Running {
			Log(c.InGreen(fmt.Sprintf("[track] %d rcc job - (port %05d) engine reported ready", id, port)))

			break
		}
	}

	// if the ready signal never arrives (map failure etc.), the job silently died
	if s.status() != Running {
		Log(c.InRed(fmt.Sprintf("[track] %d rcc job - no ready signal, terminating", id)))
		s.stop()

		return
	}

	for {
		time.Sleep(10 * time.Second)

		if s.status() == Closed {
			return
		}

		if time.Since(s.lastPushTime()) > rccGracePeriod {
			break
		}
	}

	Log(c.InRed(fmt.Sprintf("[track] %d rcc job - no engine activity (last push %s ago), terminating", id, time.Since(s.lastPushTime()).Round(time.Second))))
	s.stop()
}

// CheckServerUp reports whether a UDP port is already in use, by trying to bind it
func CheckServerUp(port int) bool {
	conn, err := net.ListenUDP(proto, &net.UDPAddr{Port: port})
	if err != nil {
		return true
	}

	conn.Close()

	return false
}
