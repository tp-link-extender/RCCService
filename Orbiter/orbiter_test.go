package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "0123456789abcdef-long"

func testOrbiter(t *testing.T) *orbiter {
	t.Helper()

	cfg := config{
		Key: testKey,
		Domain: "mercs.dev",
		SiteURL: "http://site.invalid",
		DefaultEngine: engineRCC,
		MaxJobsPerRCC: 3,
		RCCBasePort: 64989,
		RCCSpawn: modeDirect,
	}

	return newOrbiter(cfg, "version")
}

// serve sends a request through a handler and returns the recorder
func serve(h http.Handler, method, path, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func TestKeyMatches(t *testing.T) {
	cases := []struct {
		name, key, header string
		want bool
	}{
		{"correct bearer", testKey, "Bearer " + testKey, true},
		{"wrong bearer", testKey, "Bearer nope", false},
		{"no header", testKey, "", false},
		{"no Bearer prefix", testKey, testKey, false},
		{"unset key matches nothing", "", "Bearer ", false},
		{"unset key vs empty bearer", "", "Bearer", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}

			if got := keyMatches(req, tc.key); got != tc.want {
				t.Fatalf("keyMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrivateRoutesNeedTheKey(t *testing.T) {
	o := testOrbiter(t)
	mux := o.privateMux()

	for _, route := range []struct{ method, path string }{
		{"POST", "/render/r1"},
		{"GET", "/gameservers"},
		{"PUT", "/gameserver/5"},
		{"DELETE", "/gameserver/5"},
	} {
		if rec := serve(mux, route.method, route.path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a bearer = %d, want 401", route.method, route.path, rec.Code)
		}

		if rec := serve(mux, route.method, route.path, "wrong-but-long-enough-key"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong bearer = %d, want 401", route.method, route.path, rec.Code)
		}
	}
}

func TestUnauthorizedBodyIsJSON(t *testing.T) {
	rec := serve(testOrbiter(t).privateMux(), "GET", "/gameservers", "")

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || body["error"] == "" {
		t.Fatalf("body = %q, want {\"error\": ...}", rec.Body.String())
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
}

func TestListeningSplitStaysSeparate(t *testing.T) {
	o := testOrbiter(t)

	// the Site's routes are not reachable from the loopback or public listeners
	if rec := serve(o.loopbackMux(), "POST", "/render/r1", testKey); rec.Code == http.StatusAccepted {
		t.Errorf("loopback served a Site route")
	}

	if rec := serve(o.loopbackMux(), "GET", "/gameservers", testKey); rec.Code == http.StatusOK {
		t.Errorf("loopback served the gameserver list")
	}

	if rec := serve(o.publicMux(), "GET", "/gameservers", ""); rec.Code == http.StatusOK {
		t.Errorf("public served the gameserver list")
	}

	if rec := serve(o.publicMux(), "POST", "/ping/r1", ""); rec.Code == http.StatusOK {
		t.Errorf("public served the render callback")
	}

	if rec := serve(o.privateMux(), "POST", "/ping/r1", testKey); rec.Code == http.StatusOK {
		t.Errorf("the Site listener served the render callback")
	}

	if rec := serve(o.privateMux(), "GET", "/serve/5", testKey); rec.Code == http.StatusOK {
		t.Errorf("the Site listener served a serve script")
	}
}

func TestRenderIDValidation(t *testing.T) {
	good := []string{"a", "abc-_123", strings.Repeat("x", 64)}
	bad := []string{"", "a/b", "a.b", "a b", strings.Repeat("x", 65)}

	for _, id := range good {
		if !renderIDPattern.MatchString(id) {
			t.Errorf("%q rejected, want accepted", id)
		}
	}

	for _, id := range bad {
		if renderIDPattern.MatchString(id) {
			t.Errorf("%q accepted, want rejected", id)
		}
	}
}

func TestGameserverIDMustBePositive(t *testing.T) {
	o := testOrbiter(t)

	for _, id := range []string{"0", "-3", "abc"} {
		rec := serve(o.privateMux(), "DELETE", "/gameserver/"+id, testKey)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("DELETE /gameserver/%s = %d, want 400", id, rec.Code)
		}
	}
}

func TestCloseUnknownGameserverIsNoContent(t *testing.T) {
	rec := serve(testOrbiter(t).privateMux(), "DELETE", "/gameserver/42", testKey)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE unknown = %d, want 204", rec.Code)
	}
}

func TestStartRunningGameserverIsAccepted(t *testing.T) {
	o := testOrbiter(t)

	running := newGameserver(7, engineRCC, &udpRelay{})
	running.setStatus(Running)
	o.servers[7] = running

	rec := serve(o.privateMux(), "PUT", "/gameserver/7?engine=studio", testKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("PUT on a running place = %d, want 202", rec.Code)
	}

	if o.servers[7] != running {
		t.Errorf("a start on a running place replaced its server")
	}
}

func TestStartRejectsUnknownEngine(t *testing.T) {
	rec := serve(testOrbiter(t).privateMux(), "PUT", "/gameserver/7?engine=roblox", testKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with a bad engine = %d, want 400", rec.Code)
	}
}

func TestListOmitsClosedAndProxyField(t *testing.T) {
	o := testOrbiter(t)

	open := newGameserver(3, engineRCC, &udpRelay{})
	open.setStatus(Running)
	o.servers[3] = open

	closed := newGameserver(4, engineRCC, &udpRelay{})
	closed.setStatus(Closed)
	o.servers[4] = closed

	rec := serve(o.privateMux(), "GET", "/gameservers", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}

	raw := rec.Body.Bytes()

	var pairs [][]json.RawMessage
	if err := json.Unmarshal(raw, &pairs); err != nil {
		t.Fatalf("list is not [[id, info]...]: %v (%s)", err, raw)
	}

	if len(pairs) != 1 {
		t.Fatalf("got %d entries, want only the running place: %s", len(pairs), raw)
	}

	var id int
	if err := json.Unmarshal(pairs[0][0], &id); err != nil || id != 3 {
		t.Fatalf("id = %s, want 3", pairs[0][0])
	}

	var info map[string]json.RawMessage
	if err := json.Unmarshal(pairs[0][1], &info); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"pid", "startTime", "status"} {
		if _, ok := info[key]; !ok {
			t.Errorf("info lacks %q: %s", key, pairs[0][1])
		}
	}

	if _, ok := info["Proxy"]; ok {
		t.Errorf("the list leaks the relay: %s", pairs[0][1])
	}

	if string(info["status"]) != "2" {
		t.Errorf("status = %s, want 2 (running)", info["status"])
	}
}

func TestStopIsOnceAndNeverReopens(t *testing.T) {
	s := newGameserver(9, engineRCC, &udpRelay{})

	calls := 0
	s.stopFn = func() error {
		calls++

		return nil
	}

	s.stop()
	s.stop()

	if calls != 1 {
		t.Errorf("stopFn ran %d times, want 1", calls)
	}

	if s.setStatus(Running) {
		t.Errorf("a closed server reopened")
	}

	if s.status() != Closed {
		t.Errorf("status = %d, want Closed", s.status())
	}

	select {
	case <-s.done:
	default:
		t.Errorf("done was not closed")
	}
}

func TestWatchWakesOnChange(t *testing.T) {
	s := newGameserver(10, engineStudio, &udpRelay{})

	_, changed := s.watch()

	go func() {
		time.Sleep(10 * time.Millisecond)
		s.setStatus(Running)
	}()

	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher was not woken by the change")
	}
}

func TestDeferredStatusIsAppliedOnRegister(t *testing.T) {
	o := testOrbiter(t)

	if got := o.deferOrGet(12, []string{"Ready"}); got != nil {
		t.Fatalf("deferOrGet found a server that isn't there")
	}

	s := newGameserver(12, engineRCC, &udpRelay{})

	deferred := o.registerServer(12, s)
	if len(deferred) != 1 || deferred[0][0] != "Ready" {
		t.Fatalf("deferred = %v, want the Ready status", deferred)
	}

	if o.deferOrGet(12, []string{"Ready"}) != s {
		t.Errorf("a registered server wasn't returned")
	}
}

func TestRemoveServerKeepsANewerServer(t *testing.T) {
	o := testOrbiter(t)

	old := newGameserver(20, engineRCC, &udpRelay{})
	o.registerServer(20, old)

	fresh := newGameserver(20, engineRCC, &udpRelay{})
	o.registerServer(20, fresh)

	o.removeServer(20, old)

	if o.servers[20] != fresh {
		t.Errorf("removing an old server dropped the new one")
	}
}

func TestHostPingRejectsUnknownStatus(t *testing.T) {
	rec := serve(testOrbiter(t).loopbackMux(), "POST", "/hostping/5", "")

	// an empty body is an empty status, which is unknown
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("hostping with no status = %d, want 400", rec.Code)
	}
}

func TestHostPingDefersWhenNoServerYet(t *testing.T) {
	o := testOrbiter(t)

	req := httptest.NewRequest("POST", "/hostping/31", strings.NewReader("Loaded\n"))
	rec := httptest.NewRecorder()

	// through the mux, so the place id is in the path. The server isn't registered, so the status is held for it
	o.loopbackMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("hostping = %d, want 200", rec.Code)
	}

	if len(o.pending[31]) != 1 {
		t.Errorf("pending = %v, want the Loaded status held", o.pending[31])
	}
}

func TestServeScriptNeedsTheToken(t *testing.T) {
	o := testOrbiter(t)

	token, err := o.registerServeScript(8, []byte("script body"))
	if err != nil {
		t.Fatal(err)
	}

	if rec := serve(o.loopbackMux(), "GET", "/serve/8?t=wrong", ""); rec.Code != http.StatusNotFound {
		t.Errorf("wrong token = %d, want 404", rec.Code)
	}

	rec := serve(o.loopbackMux(), "GET", "/serve/8?t="+token, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "script body" {
		t.Errorf("right token = %d %q", rec.Code, rec.Body.String())
	}
}

func TestRemoveServeScriptOnlyWithItsToken(t *testing.T) {
	o := testOrbiter(t)

	oldToken, _ := o.registerServeScript(8, []byte("old"))
	newToken, _ := o.registerServeScript(8, []byte("new"))

	o.removeServeScript(8, oldToken)

	if _, ok := o.serves[8]; !ok {
		t.Fatalf("a stale token removed the newer script")
	}

	o.removeServeScript(8, newToken)

	if _, ok := o.serves[8]; ok {
		t.Errorf("the script stayed after its own token removed it")
	}
}

func TestFetchServeScriptSendsKeyAndFailsOnError(t *testing.T) {
	var gotAuth, gotPath string

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path

		if r.URL.Path == "/game/4/serve" {
			w.Write([]byte("local url = \"http://\" .. \"mercs.dev\""))

			return
		}

		http.Error(w, "no", http.StatusForbidden)
	}))
	defer site.Close()

	o := testOrbiter(t)
	o.cfg.SiteURL = site.URL

	body, err := o.fetchServeScript(4)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), "mercs.dev") {
		t.Errorf("body = %q", body)
	}

	if gotAuth != "Bearer "+testKey || gotPath != "/game/4/serve" {
		t.Errorf("request = (%q, %q)", gotAuth, gotPath)
	}

	if _, err := o.fetchServeScript(5); err == nil {
		t.Errorf("a 403 from the Site was not an error")
	}
}

func TestIDToPortAndRelayOffset(t *testing.T) {
	if got := idToPort(1); got != 10001 {
		t.Errorf("idToPort(1) = %d", got)
	}

	if got := idToPort(25000 + 7); got != 10007 {
		t.Errorf("ids wrap at proxyOffset: idToPort = %d", got)
	}
}
