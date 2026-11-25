package main

import (
	"strings"
	"testing"
)

// envOf makes a lookup over a map, for loadConfig
func envOf(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]

		return v, ok
	}
}

// validEnv is the smallest environment the Orbiter starts with
func validEnv() map[string]string {
	return map[string]string{
		"ORBITER_KEY": "0123456789abcdef-long",
		"DOMAIN": "mercs.dev",
		"SITE_URL": "http://localhost:5173",
		"SETUPDOMAIN": "setup.mercs.dev",
		"ENV": "dev",
		"MAX_JOBS_PER_RCC": "3",
		"RCC_BASE_PORT": "64989",
		"HOST_EXPIRATION": "300",
	}
}

func TestLoadConfigAcceptsMinimalEnv(t *testing.T) {
	cfg, err := loadConfig(envOf(validEnv()))
	if err != nil {
		t.Fatalf("valid env rejected: %v", err)
	}

	if cfg.DefaultEngine != engineRCC {
		t.Errorf("DefaultEngine = %q, want %q", cfg.DefaultEngine, engineRCC)
	}

	if !cfg.Dev {
		t.Errorf("ENV=dev should set Dev")
	}

	if cfg.MapOverride {
		t.Errorf("RCC_MAP_LOCATION unset should not override the map")
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := map[string]func(map[string]string){
		"short key": func(m map[string]string) {
			m["ORBITER_KEY"] = "short"
		},
		"placeholder key": func(m map[string]string) {
			m["ORBITER_KEY"] = "change-me-to-a-long-random-key"
		},
		"missing key": func(m map[string]string) {
			delete(m, "ORBITER_KEY")
		},
		"SITE_URL without scheme": func(m map[string]string) {
			m["SITE_URL"] = "localhost:5173"
		},
		"SITE_URL with trailing slash": func(m map[string]string) {
			m["SITE_URL"] = "http://localhost:5173/"
		},
		"unknown ENV": func(m map[string]string) {
			m["ENV"] = "prod"
		},
		"bad engine": func(m map[string]string) {
			m["DEFAULT_ENGINE"] = "roblox"
		},
		"non-integer jobs": func(m map[string]string) {
			m["MAX_JOBS_PER_RCC"] = "three"
		},
		"zero jobs": func(m map[string]string) {
			m["MAX_JOBS_PER_RCC"] = "0"
		},
		"negative expiration": func(m map[string]string) {
			m["HOST_EXPIRATION"] = "-1"
		},
		"unknown spawn mode": func(m map[string]string) {
			m["RCC_SPAWN"] = "fork"
		},
		"missing setup domain": func(m map[string]string) {
			delete(m, "SETUPDOMAIN")
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnv()

			mutate(env)

			if _, err := loadConfig(envOf(env)); err == nil {
				t.Fatalf("expected an error")
			}
		})
	}
}

func TestLoadConfigMapOverride(t *testing.T) {
	env := validEnv()
	env["RCC_MAP_LOCATION"] = "none"

	cfg, err := loadConfig(envOf(env))
	if err != nil {
		t.Fatal(err)
	}

	if !cfg.MapOverride || cfg.MapLocation != "none" {
		t.Errorf("override = (%v, %q), want (true, none)", cfg.MapOverride, cfg.MapLocation)
	}
}

func TestLoadConfigErrorNamesTheVariable(t *testing.T) {
	env := validEnv()
	delete(env, "SITE_URL")

	_, err := loadConfig(envOf(env))
	if err == nil || !strings.Contains(err.Error(), "SITE_URL") {
		t.Fatalf("error = %v, want one naming SITE_URL", err)
	}
}
