// The Orbiter is the one service on the RCC machine. It runs gameservers (2013 Studio processes and RCC jobs) and renders for the Site, and relays their traffic. Its wire contract is docs/src/content/docs/design/orbiter-contract.md in the MercuryCore repository
package main

import (
	"net"
	"net/http"
	"os"

	c "github.com/TwiN/go-color"
	"github.com/caddyserver/certmagic"
	env "github.com/joho/godotenv"
	. "github.com/tp-link-extender/RCCService/Shared"
)

// servePublic serves the browsers' status streams. In dev it's plain HTTP on :64992; in production it's gs.{DOMAIN} on 443, with certificates from certmagic
func servePublic(o *orbiter, cfg config) {
	if cfg.Dev {
		Log(c.InCyan("~ Public status server is up on " + publicAddr + " ~"))
		Fatal(http.ListenAndServe(publicAddr, o.publicMux()), "Failed to start public status server.")

		return
	}

	Log(c.InCyan("~ Public status server is up on port 443 ~"))

	gsDomain := "gs." + cfg.Domain

	if err := certmagic.HTTPS([]string{gsDomain}, o.publicMux()); err != nil {
		Log(c.InRed("Failed to start public status server with HTTPS: " + err.Error()))
	}
}

func main() {
	Log(c.InYellow("Loading environment variables..."))
	Fatal(env.Load(".env"), "Failed to load environment variables. Please place them in a .env file in the current directory.")

	cfg, err := loadConfig(os.LookupEnv)

	Fatal(err, "Invalid configuration.")
	Log(c.InYellow("Checking for gameserver files..."))

	ver, err := LoadFromSetup(cfg.SetupDomain)

	Fatal(err, c.InRed("Failed to load necessary gameserver files from Setup domain."))

	o := newOrbiter(cfg, ver)

	// bind both internal listeners before spawning RCC instances, so a bind failure doesn't leave orphaned instances behind
	loopback, err := net.Listen("tcp", loopbackAddr)

	Fatal(err, "Failed to bind the loopback listener on "+loopbackAddr+".")

	private, err := net.Listen("tcp", privateAddr)

	Fatal(err, "Failed to bind the Site listener on "+privateAddr+".")
	Log(c.InPurple("Starting RCCService..."))
	Fatal(o.pool.start(), "RCCService took too long to start.")

	go servePublic(o, cfg)
	go func() {
		Fatal(http.Serve(loopback, o.loopbackMux()), "The loopback listener stopped.")
	}()

	Log(c.InGreen("~ Orbiter is up: Site on " + privateAddr + ", RCC and Studio on " + loopbackAddr + " ~"))
	Fatal(http.Serve(private, o.privateMux()), "The Site listener stopped.")
}
