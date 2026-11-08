//go:build !darwin

package main

import (
	"context"
	"errors"
	"runtime"
)

func pollPlayer(ctx context.Context) (Snapshot, error) {
	return Snapshot{}, errors.New("reading Apple Music is only supported on macOS")
}

func platformAgent() string { return runtime.GOOS }
