package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	c "github.com/TwiN/go-color"
	. "github.com/tp-link-extender/RCCService/Shared"
)

// orbiter holds everything the three listeners share
type orbiter struct {
	cfg config
	version string
	pool *pool
	site *http.Client

	// gameservers, keyed by place id. gsMu guards the maps and the channel below, and
	// is taken before any gameserver's own mutex, never after
	gsMu sync.Mutex
	servers map[int]*gameserver
	starting map[int]struct{} // ids with a start in progress
	pending map[int][][]string // statuses that arrived before their server was registered
	added chan struct{} // closed and replaced whenever a server is registered

	// serve scripts for Studio, keyed by place id, handed out with a token
	serveMu sync.RWMutex
	serves map[int]serveScript
}

const (
	loopbackPort = 64990 // RCC scripts and Studio, on this machine only
	privatePort = 64991 // the Site
	publicPort = 64992 // browsers, in dev only; production serves gs.{DOMAIN} on 443

	loopbackAddr = "127.0.0.1:64990"
	privateAddr = ":64991"
	publicAddr = ":64992"

	// render ids are SurrealDB record ids
	maxRenderIDLength = 64

	// the callbacks a gameserver sends are a word and maybe a user id, so anything bigger is junk
	hostStatusLimit = 4096

	// how long a start or a stream waits for a server that's still being registered
	serverWait = 2 * time.Second
	streamWait = 30 * time.Second
)

var (
	renderIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

	// knownHostStatuses are the statuses hosted gameservers push to the loopback hostping route
	knownHostStatuses = map[string]bool{
		"Closed": true,
		"Heartbeat": true,
		"Loaded": true,
		"PlayerAdded": true,
		"PlayersLeft": true,
		"Ready": true,
		"Unoccupied": true,
	}
)

// keyMatches reports whether the request carries the configured bearer. An unset key matches nothing
func keyMatches(r *http.Request, key string) bool {
	if key == "" {
		return false
	}

	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")

	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1
}

func newOrbiter(cfg config, version string) *orbiter {
	return &orbiter{
		cfg: cfg,
		version: version,
		pool: newPool(cfg, loopbackPort, privatePort, publicPort),
		site: &http.Client{
			Timeout: 60 * time.Second,
		},
		servers: make(map[int]*gameserver),
		starting: make(map[int]struct{}),
		pending: make(map[int][][]string),
		added: make(chan struct{}),
		serves: make(map[int]serveScript),
	}
}

// loopbackMux is the listener for RCC and Studio, bound to 127.0.0.1 only
func (o *orbiter) loopbackMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /ping/{id}", o.renderPingRoute)
	mux.HandleFunc("POST /hostping/{id}", o.hostPingRoute)
	mux.HandleFunc("GET /serve/{id}", o.serveRoute)

	return mux
}

// publicMux serves browsers the status streams and nothing else
func (o *orbiter) publicMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{id}", o.streamRoute)

	return mux
}

// writeError sends the contract's error body: {"error": "<message>"}
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// gameserverID reads the place id in the path, a positive decimal integer
func gameserverID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "gameserver id must be a positive integer")
		return 0, false
	}

	return id, true
}

// requireKey guards a route with the bearer key, failing closed
func (o *orbiter) requireKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !keyMatches(r, o.cfg.Key) {
			Log(c.InRed("Rejected " + r.Method + " " + r.URL.Path + ": missing or wrong bearer"))
			writeError(w, http.StatusUnauthorized, "missing or wrong bearer token")

			return
		}

		next(w, r)
	}
}

// privateMux is the Site's listener, :64991. Everything on it needs the key
func (o *orbiter) privateMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /render/{id}", o.requireKey(o.renderStartRoute))
	mux.HandleFunc("GET /gameservers", o.requireKey(o.listRoute))
	mux.HandleFunc("PUT /gameserver/{id}", o.requireKey(o.startRoute))
	mux.HandleFunc("DELETE /gameserver/{id}", o.requireKey(o.closeRoute))

	return mux
}

// renderID reads and validates the render id in the path
func renderID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !renderIDPattern.MatchString(id) || len(id) > maxRenderIDLength {
		writeError(w, http.StatusBadRequest, "render id must be 1 to 64 characters of A-Z, a-z, 0-9, _ or -")
		return "", false
	}

	return id, true
}
