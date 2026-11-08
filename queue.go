package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Pending is a play that has not been confirmed written to the PDS yet.
type Pending struct {
	Collection string          `json:"collection"`
	Rkey       string          `json:"rkey"`
	Record     json.RawMessage `json:"record"`
}

// Queue persists unsent plays so being offline or asleep doesn't lose them.
type Queue struct {
	path  string
	mu    sync.Mutex
	items []Pending
}

func openQueue(path string) (*Queue, error) {
	q := &Queue{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &q.items); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *Queue) Add(p Pending) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, p)
	return q.saveLocked()
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Drain tries send on each item in order, stopping at the first transient
// failure. Items that succeed or fail permanently are removed.
func (q *Queue) Drain(send func(Pending) error, permanent func(error) bool) (sent int, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) > 0 {
		e := send(q.items[0])
		if e != nil && !permanent(e) {
			err = e
			break
		}
		if e == nil {
			sent++
		}
		q.items = q.items[1:]
	}
	if serr := q.saveLocked(); serr != nil && err == nil {
		err = serr
	}
	return sent, err
}

func (q *Queue) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(q.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(q.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, q.path)
}
