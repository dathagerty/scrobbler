package main

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func snap(state string, tr *Track, pos float64, sec int) Snapshot {
	return Snapshot{State: state, Track: tr, Position: pos, At: t0.Add(time.Duration(sec) * time.Second)}
}

func kinds(evs []Event) []EventKind {
	var k []EventKind
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return k
}

// play feeds snapshots every `step` seconds from pos `from` for `secs` seconds
// starting at wall second `at`, and returns all events.
func play(tk *Tracker, tr *Track, from float64, at, secs, step int) []Event {
	var all []Event
	for i := 0; i <= secs; i += step {
		all = append(all, tk.Update(snap("playing", tr, from+float64(i), at+i))...)
	}
	return all
}

func TestThreshold(t *testing.T) {
	cases := map[float64]float64{60: 54, 200: 100, 600: 240, 0: -1}
	for d, want := range cases {
		got := scrobbleThreshold(d)
		if want < 0 {
			if got < 1e9 {
				t.Errorf("duration %v: want infinite, got %v", d, got)
			}
			continue
		}
		if got != want {
			t.Errorf("duration %v: want %v, got %v", d, want, got)
		}
	}
}

func TestScrobblesOnceAtThreshold(t *testing.T) {
	tr := &Track{ID: "a", Name: "Song", Duration: 200}
	tk := &Tracker{}
	evs := play(tk, tr, 0, 0, 99, 3)
	if got := kinds(evs); len(got) != 1 || got[0] != EventNowPlaying {
		t.Fatalf("before threshold: %v", got)
	}
	evs = play(tk, tr, 102, 102, 90, 3)
	if got := kinds(evs); len(got) != 1 || got[0] != EventScrobble {
		t.Fatalf("after threshold: %v", got)
	}
	if !evs[0].StartedAt.Equal(t0) {
		t.Errorf("startedAt = %v", evs[0].StartedAt)
	}
}

func TestPauseDoesNotCount(t *testing.T) {
	tr := &Track{ID: "a", Duration: 200}
	tk := &Tracker{}
	play(tk, tr, 0, 0, 30, 5)
	// Paused for an hour at position 30.
	for i := 0; i < 3600; i += 5 {
		if evs := tk.Update(snap("paused", tr, 30, 35+i)); len(evs) != 0 {
			t.Fatalf("event while paused: %v", kinds(evs))
		}
	}
	// Resume: needs 70 more seconds of play.
	evs := play(tk, tr, 30, 3700, 65, 5)
	if len(evs) != 0 {
		t.Fatalf("scrobbled too early: %v", kinds(evs))
	}
	evs = play(tk, tr, 100, 3770, 10, 5)
	if got := kinds(evs); len(got) != 1 || got[0] != EventScrobble {
		t.Fatalf("want scrobble, got %v", got)
	}
}

func TestSeekForwardDoesNotCount(t *testing.T) {
	tr := &Track{ID: "a", Duration: 300}
	tk := &Tracker{}
	play(tk, tr, 0, 0, 10, 5)
	// Jump to 250 seconds in.
	evs := play(tk, tr, 250, 15, 40, 5)
	if len(evs) != 0 {
		t.Fatalf("seek counted as listening: %v", kinds(evs))
	}
}

func TestRepeatOneScrobblesTwice(t *testing.T) {
	tr := &Track{ID: "a", Duration: 60}
	tk := &Tracker{}
	first := play(tk, tr, 0, 0, 58, 2)
	second := play(tk, tr, 0, 60, 58, 2)
	for _, evs := range [][]Event{first, second} {
		got := kinds(evs)
		if len(got) != 2 || got[0] != EventNowPlaying || got[1] != EventScrobble {
			t.Fatalf("want now-playing + scrobble, got %v", got)
		}
	}
}

func TestTrackChangeResets(t *testing.T) {
	a := &Track{ID: "a", Duration: 200}
	b := &Track{ID: "b", Duration: 200}
	tk := &Tracker{}
	play(tk, a, 0, 0, 90, 5) // skip before threshold
	evs := play(tk, b, 0, 95, 50, 5)
	if got := kinds(evs); len(got) != 1 || got[0] != EventNowPlaying || evs[0].Track.ID != "b" {
		t.Fatalf("got %v", got)
	}
}

func TestStreamWithoutDurationNeverScrobbles(t *testing.T) {
	tr := &Track{ID: "radio", Duration: 0}
	tk := &Tracker{}
	for _, e := range play(tk, tr, 0, 0, 1000, 5) {
		if e.Kind == EventScrobble {
			t.Fatal("scrobbled a stream")
		}
	}
}
