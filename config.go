package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type Config struct {
	// Handle (e.g. "alice.bsky.social") or DID of the account to scrobble to.
	Handle string `json:"handle"`
	// App password from Bluesky settings (or your PDS). Optional here: the
	// TEAL_APP_PASSWORD env var or the macOS Keychain are preferred.
	AppPassword string `json:"app_password,omitempty"`
	// PDS URL. Leave empty to resolve it from the handle.
	PDS string `json:"pds,omitempty"`
	// Record collection for plays. teal.fm moved from fm.teal.alpha.feed.play
	// to fm.teal.feed.play; its indexer accepts both.
	Collection string `json:"collection,omitempty"`
	// Also publish a "now playing" status record.
	UpdateStatus *bool `json:"update_status,omitempty"`
	// Seconds between polls of Music.
	PollSeconds int `json:"poll_seconds,omitempty"`
}

const keychainService = "teal-scrobbler"

func configDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "teal-scrobbler"), nil
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s — run `scrobbler init` first", path)
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.Handle == "" {
		return nil, fmt.Errorf("%s: handle is required", path)
	}
	if c.Collection == "" {
		c.Collection = "fm.teal.feed.play"
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 5
	}
	if c.UpdateStatus == nil {
		t := true
		c.UpdateStatus = &t
	}
	return &c, nil
}

func (c *Config) statusCollection() string {
	if strings.HasPrefix(c.Collection, "fm.teal.alpha.") {
		return "fm.teal.alpha.actor.status"
	}
	return "fm.teal.actor.status"
}

// password looks in the environment, then the config file, then the Keychain.
func (c *Config) password() (string, error) {
	if p := os.Getenv("TEAL_APP_PASSWORD"); p != "" {
		return p, nil
	}
	if c.AppPassword != "" {
		return c.AppPassword, nil
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("/usr/bin/security", "find-generic-password",
			"-s", keychainService, "-a", c.Handle, "-w").Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", errors.New("no app password: set TEAL_APP_PASSWORD, app_password in the config, or store it in the Keychain (see README)")
}
