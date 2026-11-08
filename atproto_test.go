package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestTID(t *testing.T) {
	a := newTID(time.UnixMicro(1_700_000_000_000_000), 5)
	b := newTID(time.UnixMicro(1_700_000_000_000_001), 5)
	if len(a) != 13 || a >= b {
		t.Fatalf("bad TIDs %q %q", a, b)
	}
	if c := a[0]; !(c >= '2' && c <= '7' || c >= 'a' && c <= 'j') {
		t.Fatalf("high bit set: %q", a)
	}
}

// fakePDS issues tokens, expires the first access token, and stores records.
func fakePDS(t *testing.T) (*httptest.Server, map[string]json.RawMessage) {
	records := map[string]json.RawMessage{}
	gen := 0
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["password"] != "pw" {
			write(w, 401, map[string]string{"error": "AuthenticationRequired", "message": "bad password"})
			return
		}
		write(w, 200, map[string]string{"did": "did:plc:test", "accessJwt": "expired", "refreshJwt": "r1"})
	})
	mux.HandleFunc("/xrpc/com.atproto.server.refreshSession", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer r1" {
			write(w, 400, map[string]string{"error": "ExpiredToken"})
			return
		}
		gen++
		write(w, 200, map[string]string{"accessJwt": "good", "refreshJwt": "r1"})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.putRecord", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			write(w, 400, map[string]string{"error": "ExpiredToken", "message": "Token has expired"})
			return
		}
		var in struct {
			Repo, Collection, Rkey string
			Record                 json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&in)
		uri := "at://" + in.Repo + "/" + in.Collection + "/" + in.Rkey
		records[uri] = in.Record
		write(w, 200, map[string]string{"uri": uri, "cid": "x"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, records
}

func TestLoginRefreshAndPut(t *testing.T) {
	srv, records := fakePDS(t)
	c := NewClient("alice.test", "pw", srv.URL)
	ctx := context.Background()
	if err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	uri, err := c.PutRecord(ctx, "fm.teal.feed.play", "3abc", map[string]any{"trackName": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if uri != "at://did:plc:test/fm.teal.feed.play/3abc" || len(records) != 1 {
		t.Fatalf("uri %q records %v", uri, records)
	}
}

func TestBadPassword(t *testing.T) {
	srv, _ := fakePDS(t)
	err := NewClient("alice.test", "nope", srv.URL).Login(context.Background())
	if xe, ok := err.(*xrpcError); !ok || xe.Status != 401 {
		t.Fatalf("got %v", err)
	}
}

func TestQueueDrainAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	q, _ := openQueue(path)
	for _, k := range []string{"a", "b", "c"} {
		if err := q.Add(Pending{Collection: "c", Rkey: k, Record: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	// "b" fails transiently: a is sent, b and c stay.
	sent, err := q.Drain(func(p Pending) error {
		if p.Rkey == "b" {
			return &xrpcError{Status: 502}
		}
		return nil
	}, isPermanent)
	if sent != 1 || err == nil || q.Len() != 2 {
		t.Fatalf("sent=%d err=%v len=%d", sent, err, q.Len())
	}
	q2, _ := openQueue(path)
	if q2.Len() != 2 {
		t.Fatalf("reloaded len %d", q2.Len())
	}
	// Permanent rejection is dropped.
	sent, err = q2.Drain(func(p Pending) error {
		if p.Rkey == "b" {
			return &xrpcError{Status: 400, Code: "InvalidRequest"}
		}
		return nil
	}, isPermanent)
	if sent != 1 || err != nil || q2.Len() != 0 {
		t.Fatalf("sent=%d err=%v len=%d", sent, err, q2.Len())
	}
}

func TestPlayRecordShape(t *testing.T) {
	tr := Track{Name: "Song", Artist: "Artist", Album: "Album", Duration: 201.6}
	m := playFields(tr, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "a/1")
	if m["duration"] != 202 || m["playedTime"] != "2026-01-01T00:00:00Z" || m["releaseName"] != "Album" {
		t.Fatalf("%v", m)
	}
	if clip("héllo", 2) != "h" {
		t.Fatal("clip split a rune")
	}
}
