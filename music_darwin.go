//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// JXA rather than AppleScript so the output is JSON (no locale-dependent
// decimal separators, no delimiter escaping). Checking running() first keeps
// the poller from launching Music when it is closed.
const musicJXA = `
const m = Application("Music");
function out() {
  if (!m.running()) return {state: "notrunning"};
  const state = m.playerState();
  if (state !== "playing" && state !== "paused") return {state: state};
  let t;
  try { t = m.currentTrack(); } catch (e) { return {state: "stopped"}; }
  const g = (f) => { try { return f(); } catch (e) { return null; } };
  return {
    state: state,
    position: g(() => m.playerPosition()) || 0,
    id: g(() => t.persistentID()),
    name: g(() => t.name()),
    artist: g(() => t.artist()),
    album: g(() => t.album()),
    albumArtist: g(() => t.albumArtist()),
    duration: g(() => t.duration()) || 0,
  };
}
JSON.stringify(out());
`

type musicOut struct {
	State       string  `json:"state"`
	Position    float64 `json:"position"`
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Artist      string  `json:"artist"`
	Album       string  `json:"album"`
	AlbumArtist string  `json:"albumArtist"`
	Duration    float64 `json:"duration"`
}

func pollPlayer(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", musicJXA)
	b, err := cmd.Output()
	now := time.Now()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return Snapshot{}, fmt.Errorf("osascript: %v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return Snapshot{}, err
	}
	var o musicOut
	if err := json.Unmarshal(b, &o); err != nil {
		return Snapshot{}, fmt.Errorf("parsing osascript output %q: %w", b, err)
	}
	s := Snapshot{State: o.State, Position: o.Position, At: now}
	if (o.State == "playing" || o.State == "paused") && o.Name != "" {
		id := o.ID
		if id == "" {
			id = o.Artist + "\x00" + o.Name + "\x00" + o.Album
		}
		s.Track = &Track{
			ID: id, Name: o.Name, Artist: o.Artist, Album: o.Album,
			AlbumArtist: o.AlbumArtist, Duration: o.Duration,
		}
	}
	return s, nil
}

func platformAgent() string {
	v, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return "Darwin; macOS"
	}
	return "Darwin; macOS " + strings.TrimSpace(string(v))
}
