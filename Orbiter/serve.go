package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	c "github.com/TwiN/go-color"
	. "github.com/tp-link-extender/RCCService/Shared"
)

// serveScript is a place's serve loadscript from the Site, handed to one Studio process
type serveScript struct {
	token string
	body []byte
}

var (
	reBaseURL = regexp.MustCompile(`local url = "http://" \.\. "([^"]+)"`) // the serve loadscript sets this up with the Site's DomainInsecure
	rePresenceURL = regexp.MustCompile(`[a-z]+://[^"'\s]+/game/serverpresence\?ticket=[^"'\s]+`)
)

// hostpingURL is where RCC scripts send a hosted place's statuses
func hostpingURL(id int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/hostping/%d", loopbackPort, id)
}

// hostScriptFor fills the host script's placeholders from the config and a serve loadscript
func hostScriptFor(cfg config, serve string, id int) string {
	baseURL := cfg.Domain

	if m := reBaseURL.FindStringSubmatch(serve); m != nil {
		baseURL = m[1]
	}

	// RCC's own hosting script loads places via the asset route, not the rbxclient /game/{id} route that Studio-based hosting uses
	mapLocation := fmt.Sprintf("http://%s/asset/?id=%d", baseURL, id)

	switch {
	case cfg.MapOverride && cfg.MapLocation == "none": // "none" skips map loading (tests)
		mapLocation = ""
	case cfg.MapOverride:
		mapLocation = cfg.MapLocation
	}

	// matches the serverpresence URL directly instead of assuming how the loadscript spells out its Visit:SetPing call
	var presenceLine string

	if presenceURL := rePresenceURL.FindString(serve); presenceURL != "" {
		presenceLine = fmt.Sprintf("pcall(function() Visit:SetPing(%q, 30) end)", presenceURL)
	}

	// a single-pass replacer, so a substituted value is never rescanned for placeholders
	replacer := strings.NewReplacer("_BASE_URL", baseURL, "_PLACE_ID", strconv.Itoa(id), "_MAP_LOCATION", strconv.Quote(mapLocation), "_SERVER_PORT", strconv.Itoa(idToPort(id)), "_PRESENCE_PING", presenceLine, "_HOSTPING_URL", hostpingURL(id))

	return replacer.Replace(rccHostScript)
}

// fetchServeScript gets a place's serve loadscript from the Site, which only gives it out with the key
func (o *orbiter) fetchServeScript(id int) ([]byte, error) {
	url := fmt.Sprintf("%s/game/%d/serve", o.cfg.SiteURL, id)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build serve request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+o.cfg.Key)

	res, err := o.site.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch serve script: %w", err)
	}

	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxScriptBytes))
	if err != nil {
		return nil, fmt.Errorf("read serve script: %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch serve script: status %d: %s", res.StatusCode, string(body))
	}

	return body, nil
}

// composeHostScript builds the inline, RCC-safe host script for a place. Most of its parameters come from the Site's serve loadscript, which already has the right domain, map and authentication
func (o *orbiter) composeHostScript(id int) (string, error) {
	serve, err := o.fetchServeScript(id)
	if err != nil {
		return "", err
	}

	script := hostScriptFor(o.cfg, string(serve), id)

	Log(c.InBlue(fmt.Sprintf("[compose] %d - host script ready (%d chars)", id, len(script))))

	return script, nil
}

// registerServeScript stores a serve script under a fresh token and returns the token
func (o *orbiter) registerServeScript(id int, body []byte) (string, error) {
	b := make([]byte, 16)

	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate serve token: %w", err)
	}

	token := hex.EncodeToString(b)

	o.serveMu.Lock()

	o.serves[id] = serveScript{
		token: token,
		body: body,
	}

	o.serveMu.Unlock()

	return token, nil
}

// removeServeScript forgets a place's serve script, only if it's still the one with token
func (o *orbiter) removeServeScript(id int, token string) {
	if token == "" {
		return
	}

	o.serveMu.Lock()

	defer o.serveMu.Unlock()

	if cur, ok := o.serves[id]; ok && cur.token == token {
		delete(o.serves, id)
	}
}

// serveRoute serves a Studio process its serve script, to a request that carries the token
func (o *orbiter) serveRoute(w http.ResponseWriter, r *http.Request) {
	id, ok := gameserverID(w, r)
	if !ok {
		return
	}

	o.serveMu.RLock()

	s, found := o.serves[id]

	o.serveMu.RUnlock()

	if !found || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(s.token)) != 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(s.body)
}
