// Command scrobbler watches Apple Music on macOS and records plays to
// teal.fm (as fm.teal.feed.play records in your AT Protocol repository).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	appID      = "com.dathagerty.scrobbler"
	version    = "0.1.0"
	serviceURI = "https://music.apple.com"
	agentLabel = "com.dathagerty.teal-scrobbler"
)

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: scrobbler [flags] <command>

Commands:
  init        create the config file and store your app password in the Keychain
  check       log in and print what Music is playing, without writing anything
  run         watch Music and scrobble (default)
  install     install a LaunchAgent so it runs in the background at login
  uninstall   remove the LaunchAgent

Flags:
`)
	flag.PrintDefaults()
}

func main() {
	dir, err := configDir()
	if err != nil {
		log.Fatal(err)
	}
	cfgPath := flag.String("config", filepath.Join(dir, "config.json"), "config file")
	dryRun := flag.Bool("dry-run", false, "log plays instead of writing them")
	flag.Usage = usage
	flag.Parse()

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "run"
	}
	switch cmd {
	case "init":
		err = cmdInit(*cfgPath)
	case "check":
		err = cmdCheck(*cfgPath)
	case "run":
		err = cmdRun(*cfgPath, *dryRun)
	case "install":
		err = cmdInstall(*cfgPath)
	case "uninstall":
		err = cmdUninstall()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func cmdInit(cfgPath string) error {
	in := bufio.NewReader(os.Stdin)
	fmt.Print("Bluesky/AT Protocol handle (e.g. alice.bsky.social): ")
	handle, _ := in.ReadString('\n')
	handle = strings.TrimPrefix(strings.TrimSpace(handle), "@")
	if handle == "" {
		return fmt.Errorf("handle is required")
	}
	cfg := Config{Handle: handle}
	if _, err := os.Stat(cfgPath); err == nil {
		return fmt.Errorf("%s already exists; edit it directly or delete it first", cfgPath)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Println("Wrote", cfgPath)

	if runtime.GOOS != "darwin" {
		fmt.Println("Set TEAL_APP_PASSWORD or add app_password to the config.")
		return nil
	}
	fmt.Println("\nCreate an app password at https://bsky.app/settings/app-passwords (or your PDS's equivalent).")
	fmt.Println("Paste it below; it is stored in your login Keychain, not in the config file.")
	c := exec.Command("/usr/bin/security", "add-generic-password", "-U",
		"-s", keychainService, "-a", handle, "-l", "teal.fm scrobbler", "-w")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("storing password in Keychain: %w", err)
	}
	fmt.Println("\nDone. Next: `scrobbler check`, then `scrobbler install`.")
	return nil
}

func cmdCheck(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	pw, err := cfg.password()
	if err != nil {
		return err
	}
	ctx := context.Background()
	c := NewClient(cfg.Handle, pw, cfg.PDS)
	if err := c.Login(ctx); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	fmt.Printf("Logged in as %s (%s) on %s\nPlays go to %s\n", cfg.Handle, c.DID(), c.pds, cfg.Collection)
	s, err := pollPlayer(ctx)
	if err != nil {
		return fmt.Errorf("reading Music: %w", err)
	}
	if s.Track == nil {
		fmt.Println("Music:", s.State)
	} else {
		fmt.Printf("Music: %s — %s – %s (%s) at %.0f/%.0fs\n",
			s.State, s.Track.Artist, s.Track.Name, s.Track.Album, s.Position, s.Track.Duration)
	}
	return nil
}

func cmdRun(cfgPath string, dryRun bool) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	pw, err := cfg.password()
	if err != nil {
		return err
	}
	queue, err := openQueue(filepath.Join(filepath.Dir(cfgPath), "queue.json"))
	if err != nil {
		return fmt.Errorf("opening queue: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := NewClient(cfg.Handle, pw, cfg.PDS)
	loggedIn := false
	if !dryRun {
		// Keep trying: at login the network may not be up yet.
		for backoff := 5 * time.Second; ; backoff = min(backoff*2, 5*time.Minute) {
			err := client.Login(ctx)
			if err == nil {
				loggedIn = true
				break
			}
			var xe *xrpcError
			if errors.As(err, &xe) && xe.Status == 401 {
				return fmt.Errorf("login rejected: %w", err) // wrong password; don't hammer
			}
			log.Printf("login failed (retrying in %s): %v", backoff, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
		}
		log.Printf("logged in as %s (%s)", cfg.Handle, client.DID())
	}

	agent := fmt.Sprintf("%s/%s (%s)", appID, version, platformAgent())
	clockID := rand.Uint64N(1024)
	tracker := &Tracker{}
	var lastPollErr string
	var lastDrain time.Time

	drain := func() {
		if !loggedIn || queue.Len() == 0 {
			return
		}
		lastDrain = time.Now()
		sent, err := queue.Drain(func(p Pending) error {
			_, err := client.PutRecord(ctx, p.Collection, p.Rkey, p.Record)
			return err
		}, isPermanent)
		if sent > 0 {
			log.Printf("sent %d queued play(s)", sent)
		}
		if err != nil {
			log.Printf("queue: %d play(s) still pending: %v", queue.Len(), err)
		}
	}
	drain()

	tick := time.NewTicker(time.Duration(cfg.PollSeconds) * time.Second)
	defer tick.Stop()
	for {
		snap, err := pollPlayer(ctx)
		if err != nil {
			if msg := err.Error(); msg != lastPollErr {
				log.Printf("polling Music: %v", err)
				lastPollErr = msg
			}
		} else {
			lastPollErr = ""
			for _, ev := range tracker.Update(snap) {
				handleEvent(ctx, cfg, client, queue, ev, agent, clockID, dryRun)
			}
		}
		if queue.Len() > 0 && time.Since(lastDrain) > time.Minute {
			drain()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func handleEvent(ctx context.Context, cfg *Config, c *Client, q *Queue, ev Event, agent string, clockID uint64, dryRun bool) {
	t := ev.Track
	switch ev.Kind {
	case EventNowPlaying:
		log.Printf("now playing: %s – %s", t.Artist, t.Name)
		if dryRun || !*cfg.UpdateStatus {
			return
		}
		now := time.Now().UTC()
		expiry := now.Add(time.Duration(ev.Remaining*float64(time.Second)) + time.Minute)
		status := map[string]any{
			"$type":  cfg.statusCollection(),
			"time":   now.Format(time.RFC3339Nano),
			"expiry": expiry.Format(time.RFC3339Nano),
			"item":   playFields(t, ev.StartedAt, agent),
		}
		if _, err := c.PutRecord(ctx, cfg.statusCollection(), "self", status); err != nil {
			log.Printf("updating status: %v", err) // best effort; not queued
		}

	case EventScrobble:
		rec := playFields(t, ev.StartedAt, agent)
		rec["$type"] = cfg.Collection
		if dryRun {
			b, _ := json.Marshal(rec)
			log.Printf("would scrobble: %s", b)
			return
		}
		b, _ := json.Marshal(rec)
		p := Pending{Collection: cfg.Collection, Rkey: newTID(ev.StartedAt, clockID), Record: b}
		uri, err := c.PutRecord(ctx, p.Collection, p.Rkey, p.Record)
		switch {
		case err == nil:
			log.Printf("scrobbled: %s – %s (%s)", t.Artist, t.Name, uri)
		case isPermanent(err):
			log.Printf("scrobble rejected, dropping: %s – %s: %v", t.Artist, t.Name, err)
		default:
			if qerr := q.Add(p); qerr != nil {
				log.Printf("scrobble failed and could not be queued: %v / %v", err, qerr)
			} else {
				log.Printf("scrobble failed, queued for retry: %s – %s: %v", t.Artist, t.Name, err)
			}
		}
	}
}

// playFields builds the fields shared by a play record and a status playView.
func playFields(t Track, started time.Time, agent string) map[string]any {
	m := map[string]any{
		"trackName":             clip(t.Name, 256),
		"playedTime":            started.UTC().Format(time.RFC3339Nano),
		"submissionClientAgent": clip(agent, 256),
		"musicServiceUri":       serviceURI,
	}
	if t.Artist != "" {
		m["artists"] = []map[string]string{{"artistName": clip(t.Artist, 256)}}
	}
	if t.Album != "" {
		m["releaseName"] = clip(t.Album, 256)
	}
	if t.Duration > 0 {
		m["duration"] = int(math.Round(t.Duration))
	}
	return m
}

// clip truncates s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"), nil
}

func cmdInstall(cfgPath string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("install is only supported on macOS")
	}
	if _, err := loadConfig(cfgPath); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	cfgAbs, _ := filepath.Abs(cfgPath)
	logPath := filepath.Join(filepath.Dir(cfgAbs), "scrobbler.log")
	pp, err := plistPath()
	if err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>-config</string>
    <string>%s</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, agentLabel, xmlEscape(exe), xmlEscape(cfgAbs), xmlEscape(logPath), xmlEscape(logPath))
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(pp, []byte(plist), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("/bin/launchctl", "bootout", domain+"/"+agentLabel).Run() // ignore "not loaded"
	if out, err := exec.Command("/bin/launchctl", "bootstrap", domain, pp).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Printf("Installed %s\nRunning in the background; logs at %s\n", pp, logPath)
	fmt.Println("macOS will ask once for permission to control Music — allow it.")
	return nil
}

func cmdUninstall() error {
	pp, err := plistPath()
	if err != nil {
		return err
	}
	_ = exec.Command("/bin/launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), agentLabel)).Run()
	if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Uninstalled.")
	return nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
