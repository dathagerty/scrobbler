package main

import (
	"math"
	"time"
)

// Track is what we know about the currently loaded track in the player.
type Track struct {
	ID          string // player-local persistent ID
	Name        string
	Artist      string
	Album       string
	AlbumArtist string
	Duration    float64 // seconds
}

// Snapshot is one poll of the player.
type Snapshot struct {
	State    string // "playing", "paused", "stopped", "notrunning"
	Track    *Track
	Position float64 // seconds into the track
	At       time.Time
}

// EventKind distinguishes tracker output.
type EventKind int

const (
	EventNowPlaying EventKind = iota // a track started (or resumed into a new session) playing
	EventScrobble                    // listen threshold crossed; submit a play
)

type Event struct {
	Kind      EventKind
	Track     Track
	StartedAt time.Time // when playback of this play began
	Remaining float64   // seconds left in the track (NowPlaying only)
}

type session struct {
	track       Track
	startedAt   time.Time
	listened    float64
	lastPos     float64
	lastAt      time.Time
	lastPlaying bool
	announced   bool
	scrobbled   bool
}

// Tracker turns a stream of player snapshots into now-playing and scrobble
// events. It counts only time actually spent playing, so pausing, seeking
// forward, or leaving the player paused overnight does not inflate a play.
type Tracker struct {
	cur *session
}

// scrobbleThreshold follows teal.fm's play lexicon: the whole track if it is
// under 2 minutes, otherwise half its length capped at 4 minutes. "Whole" is
// relaxed to 90% because polling can miss the last few seconds before the
// player advances.
func scrobbleThreshold(duration float64) float64 {
	if duration <= 0 {
		return math.Inf(1) // streams/radio without a length: never scrobble
	}
	if duration < 120 {
		return duration * 0.9
	}
	return math.Min(duration/2, 240)
}

func (t *Tracker) Update(s Snapshot) []Event {
	if s.Track == nil || (s.State != "playing" && s.State != "paused") {
		t.cur = nil
		return nil
	}
	playing := s.State == "playing"

	newSession := t.cur == nil || t.cur.track.ID != s.Track.ID
	// Same track restarted from the top (repeat-one, or replayed by hand).
	if !newSession && s.Position+5 < t.cur.lastPos && s.Position < 15 {
		newSession = true
	}
	if newSession {
		t.cur = &session{
			track:     *s.Track,
			startedAt: s.At.Add(-time.Duration(s.Position * float64(time.Second))),
			lastPos:   s.Position,
			lastAt:    s.At,
		}
	} else if playing && t.cur.lastPlaying {
		delta := s.Position - t.cur.lastPos
		wall := s.At.Sub(t.cur.lastAt).Seconds()
		// Count forward progress no larger than real elapsed time (+ slack for
		// timer jitter). A bigger jump is a seek and is not listening time.
		if delta > 0 && delta <= wall+2 {
			t.cur.listened += delta
		}
	}
	t.cur.lastPos = s.Position
	t.cur.lastAt = s.At
	t.cur.lastPlaying = playing

	var out []Event
	if playing && !t.cur.announced {
		t.cur.announced = true
		out = append(out, Event{
			Kind:      EventNowPlaying,
			Track:     t.cur.track,
			StartedAt: t.cur.startedAt,
			Remaining: math.Max(t.cur.track.Duration-s.Position, 0),
		})
	}
	if !t.cur.scrobbled && t.cur.listened >= scrobbleThreshold(t.cur.track.Duration) {
		t.cur.scrobbled = true
		out = append(out, Event{Kind: EventScrobble, Track: t.cur.track, StartedAt: t.cur.startedAt})
	}
	return out
}
