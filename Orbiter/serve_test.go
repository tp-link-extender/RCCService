package main

import (
	"strings"
	"testing"
)

// serveLoadscript is the part of the Site's serve loadscript the host script reads
const serveLoadscript = `local url = "http://" .. "www.mercs.dev"
Visit:SetPing("https://www.mercs.dev/game/serverpresence?ticket=abc123", 30)`

// placeholders every host script must have filled in
var hostPlaceholders = []string{
	"_BASE_URL", "_PLACE_ID", "_MAP_LOCATION", "_SERVER_PORT", "_PRESENCE_PING", "_HOSTPING_URL",
}

func TestHostScriptFillsEveryPlaceholder(t *testing.T) {
	script := hostScriptFor(config{Domain: "mercs.dev"}, serveLoadscript, 42)

	for _, p := range hostPlaceholders {
		if strings.Contains(script, p) {
			t.Errorf("%s was left in the host script", p)
		}
	}
}

func TestHostScriptUsesTheLoopbackHostping(t *testing.T) {
	script := hostScriptFor(config{Domain: "mercs.dev"}, serveLoadscript, 42)

	if !strings.Contains(script, "http://127.0.0.1:64990/hostping/42") {
		t.Errorf("host script does not report to the loopback hostping route")
	}
}

func TestHostScriptTakesBaseAndPresenceFromServe(t *testing.T) {
	script := hostScriptFor(config{Domain: "fallback.dev"}, serveLoadscript, 42)

	if !strings.Contains(script, `"http://www.mercs.dev/asset/?id=42"`) {
		t.Errorf("map location did not use the serve script's base")
	}

	if !strings.Contains(script, `Visit:SetPing("https://www.mercs.dev/game/serverpresence?ticket=abc123", 30)`) {
		t.Errorf("presence ping was not carried over")
	}
}

func TestHostScriptFallsBackToDomain(t *testing.T) {
	script := hostScriptFor(config{Domain: "fallback.dev"}, "no url here", 42)

	if !strings.Contains(script, `"http://fallback.dev/asset/?id=42"`) {
		t.Errorf("without a serve base, the domain is used")
	}
}

func TestHostScriptMapNoneSkipsTheMap(t *testing.T) {
	cfg := config{Domain: "mercs.dev", MapOverride: true, MapLocation: "none"}

	script := hostScriptFor(cfg, serveLoadscript, 42)

	// the script's own guard, "if _MAP_LOCATION ~= "" then", becomes this once the map is empty
	if !strings.Contains(script, `if "" ~= "" then`) {
		t.Fatalf("the map guard does not see an empty map location")
	}

	if strings.Contains(script, "asset/?id=42") {
		t.Errorf("map=none still loads the default map")
	}
}

func TestHostScriptMapOverrideIsVerbatim(t *testing.T) {
	cfg := config{Domain: "mercs.dev", MapOverride: true, MapLocation: "https://example.com/map.rbxl"}

	script := hostScriptFor(cfg, serveLoadscript, 42)

	if !strings.Contains(script, `"https://example.com/map.rbxl"`) {
		t.Errorf("the override map is not used verbatim")
	}
}
