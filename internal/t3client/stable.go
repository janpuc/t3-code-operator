package t3client

import (
	"context"
	"errors"
	"sync"
	"time"
)

const DefaultStableIdleWindow = 5 * time.Second

type ActivitySource interface {
	ActiveInstances(context.Context, []string) ([]string, error)
}

type StableActivityReader struct {
	mutex  sync.Mutex
	source ActivitySource
	window time.Duration
	now    func() time.Time

	idleSince time.Time
}

func NewStableActivityReader(source ActivitySource, window time.Duration) (*StableActivityReader, error) {
	if source == nil {
		return nil, errors.New("activity source is required")
	}
	if window <= 0 {
		return nil, errors.New("stable idle window must be positive")
	}
	return &StableActivityReader{source: source, window: window, now: time.Now}, nil
}

func (reader *StableActivityReader) ActiveInstances(
	ctx context.Context,
	instanceIDs []string,
) ([]string, error) {
	reader.mutex.Lock()
	defer reader.mutex.Unlock()
	active, err := reader.source.ActiveInstances(ctx, instanceIDs)
	if err != nil {
		reader.idleSince = time.Time{}
		return nil, err
	}
	if len(active) != 0 {
		reader.idleSince = time.Time{}
		return active, nil
	}
	now := reader.now()
	if reader.idleSince.IsZero() {
		reader.idleSince = now
		return []string{"stable-idle-window"}, nil
	}
	if now.Sub(reader.idleSince) < reader.window {
		return []string{"stable-idle-window"}, nil
	}
	return nil, nil
}
