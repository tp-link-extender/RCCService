package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	c "github.com/TwiN/go-color"
	. "github.com/tp-link-extender/RCCService/Shared"
)

const versionPathStart = "./Versions/version-"

// setupURL returns an absolute setup domain; a domain with a scheme, such as http://localhost:63488 for dev, is used as it is
func setupURL(domain string) string {
	if found := strings.Contains(domain, "://"); !found {
		return "https://" + domain
	}

	return domain
}

// InstallSetup downloads and extracts a 2013 client version from the setup domain
func InstallSetup(domain, version string) error {
	res, err := http.Get(fmt.Sprintf("%s/2013/%s", setupURL(domain), version))
	if err != nil {
		return fmt.Errorf("get version from setup: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("get version %s from setup: unexpected status %s", version, res.Status)
	}

	Log(c.InPurple("Get successful, downloading and extracting..."))

	gz, err := gzip.NewReader(res.Body)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}

	versionDir := versionPathStart + version

	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return fmt.Errorf("create version directory: %w", err)
	}

	// every path goes through the root, so entries like ../x can't write outside the version directory
	root, err := os.OpenRoot(versionDir)
	if err != nil {
		return fmt.Errorf("open version directory: %w", err)
	}

	defer root.Close()

	for tr := tar.NewReader(gz); ; {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		switch name := filepath.FromSlash(header.Name); header.Typeflag {
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return fmt.Errorf("create directory: %w", err)
			}

			f, err := root.Create(name)
			if err != nil {
				return fmt.Errorf("create file: %w", err)
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write file: %w", err)
			}

			if err := f.Close(); err != nil {
				return fmt.Errorf("write file: %w", err)
			}
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return fmt.Errorf("create directory: %w", err)
			}
		default:
			return fmt.Errorf("unknown tar header type: %c in file %s", header.Typeflag, header.Name)
		}
	}

	Log(c.InGreen(fmt.Sprintf("Version %s downloaded and extracted successfully", version)))

	return nil
}

// LoadFromSetup returns the 2013 client version the setup domain serves, installing it if it isn't here yet
func LoadFromSetup(domain string) (string, error) {
	res, err := http.Get(fmt.Sprintf("%s/2013/version", setupURL(domain)))
	if err != nil {
		return "", fmt.Errorf("get version from setup: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get version from setup: unexpected status %s", res.Status)
	}

	verbytes, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("read version from response body: %w", err)
	}

	ver := strings.TrimSpace(string(verbytes))

	if _, err := os.Stat(versionPathStart + ver); errors.Is(err, os.ErrNotExist) {
		Log(c.InPurple(fmt.Sprintf("Version %s not found, downloading from %s...", ver, domain)))
		return ver, InstallSetup(domain, ver)
	}

	Log(c.InGreen(fmt.Sprintf("Version %s already exists, skipping download", ver)))

	return ver, nil
}
