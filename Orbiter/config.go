package main

import (
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"strings"
)

const (
	engineStudio = "studio" // 2013 Studio hosted gameservers
	engineRCC = "rcc" // gameservers hosted as jobs on RCCService instances

	modeDirect = "direct" // the RCC instance is a child process of the Orbiter
	modeDetached = "detached" // launched like a manual start and tracked by pid (Windows)

	minKeyLength = 16
)

// placeholderKeys are the example values from the env files. A key equal to one is refused
var placeholderKeys = []string{"password", "change-me-to-a-long-random-key"}

// config is the Orbiter's whole environment, read and checked once at startup
type config struct {
	Key string // ORBITER_KEY: the bearer for every Site-facing call, in both directions
	Domain string // DOMAIN: the public domain, gs.{Domain} serves the status streams in production
	SiteURL string // SITE_URL: the Site's address for the serve, place and render callbacks
	SetupDomain string // SETUPDOMAIN: where the 2013 client files are downloaded from
	Dev bool // ENV=dev: the status streams on :64992 over plain HTTP
	MaxJobsPerRCC int
	RCCBasePort int
	HostExpiration int // seconds; 0 = jobs last practically forever and are closed explicitly
	DefaultEngine string // engine for a start without ?engine=
	MapLocation string // RCC_MAP_LOCATION, used verbatim when MapOverride is set ("none" skips the map)
	MapOverride bool
	RCCSpawn string // modeDirect or modeDetached
}

// loadConfig reads and checks the environment. lookup is os.LookupEnv in production
func loadConfig(lookup func(string) (string, bool)) (config, error) {
	var cfg config

	var err error

	get := func(name string) string {
		v, _ := lookup(name)

		return strings.TrimSpace(v)
	}

	// str returns a required value, recording the first problem found
	str := func(name string) string {
		v := get(name)
		if v == "" && err == nil {
			err = fmt.Errorf("%s is required", name)
		}

		return v
	}

	// intVar returns a required integer of at least minimum
	intVar := func(name string, minimum int) int {
		v := str(name)
		if err != nil {
			return 0
		}

		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < minimum {
			err = fmt.Errorf("%s must be an integer of at least %d", name, minimum)

			return 0
		}

		return n
	}

	cfg.Key = get("ORBITER_KEY")

	switch {
	case len(cfg.Key) < minKeyLength:
		return cfg, fmt.Errorf("ORBITER_KEY must be at least %d characters", minKeyLength)
	case isPlaceholderKey(cfg.Key):
		return cfg, errors.New("ORBITER_KEY is an example value, set a real one")
	}

	cfg.Domain = str("DOMAIN")
	cfg.SetupDomain = str("SETUPDOMAIN")

	cfg.SiteURL = str("SITE_URL")
	if err == nil {
		u, parseErr := url.Parse(cfg.SiteURL)

		switch {
		case parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			err = errors.New("SITE_URL must be an absolute http or https URL")
		case strings.HasSuffix(cfg.SiteURL, "/"):
			err = errors.New("SITE_URL must not end with a slash")
		}
	}

	switch env := str("ENV"); env {
	case "dev":
		cfg.Dev = true
	case "production":
	default:
		if err == nil {
			err = errors.New(`ENV must be "dev" or "production"`)
		}
	}

	cfg.MaxJobsPerRCC = intVar("MAX_JOBS_PER_RCC", 1)
	cfg.RCCBasePort = intVar("RCC_BASE_PORT", 1)
	cfg.HostExpiration = intVar("HOST_EXPIRATION", 0)

	cfg.DefaultEngine = get("DEFAULT_ENGINE")
	if cfg.DefaultEngine == "" {
		cfg.DefaultEngine = engineRCC
	}

	if cfg.DefaultEngine != engineStudio && cfg.DefaultEngine != engineRCC {
		return cfg, errors.New("DEFAULT_ENGINE must be studio or rcc")
	}

	cfg.RCCSpawn = get("RCC_SPAWN")
	switch cfg.RCCSpawn {
	case "":
		cfg.RCCSpawn = modeDirect
		if runtime.GOOS == "windows" {
			cfg.RCCSpawn = modeDetached
		}
	case modeDirect, modeDetached:
	default:
		return cfg, errors.New("RCC_SPAWN must be direct or detached")
	}

	cfg.MapLocation, cfg.MapOverride = lookup("RCC_MAP_LOCATION")

	if err != nil {
		return cfg, err
	}

	return cfg, nil
}

// isPlaceholderKey reports whether key is one of the example values
func isPlaceholderKey(key string) bool {
	for _, p := range placeholderKeys {
		if key == p {
			return true
		}
	}

	return false
}
