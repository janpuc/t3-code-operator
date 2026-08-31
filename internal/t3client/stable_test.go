package t3client

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStableActivityReaderRequiresContinuousIdle(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	source := &sequenceActivitySource{results: []activityResult{
		{},
		{},
		{active: []string{"codex"}},
		{},
		{},
	}}
	reader, err := NewStableActivityReader(source, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reader.now = func() time.Time { return now }
	assertStableActivity(t, reader, true)
	now = now.Add(5 * time.Second)
	assertStableActivity(t, reader, false)
	assertStableActivity(t, reader, true)
	now = now.Add(time.Second)
	assertStableActivity(t, reader, true)
	now = now.Add(5 * time.Second)
	assertStableActivity(t, reader, false)
}

func TestStableActivityReaderResetsAfterReadFailure(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	source := &sequenceActivitySource{results: []activityResult{{}, {err: errors.New("unavailable")}, {}, {}}}
	reader, err := NewStableActivityReader(source, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reader.now = func() time.Time { return now }
	assertStableActivity(t, reader, true)
	now = now.Add(10 * time.Second)
	if _, err := reader.ActiveInstances(context.Background(), nil); err == nil {
		t.Fatal("expected activity read failure")
	}
	assertStableActivity(t, reader, true)
	now = now.Add(5 * time.Second)
	assertStableActivity(t, reader, false)
}

func assertStableActivity(t *testing.T, reader *StableActivityReader, expectedActive bool) {
	t.Helper()
	active, err := reader.ActiveInstances(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if (len(active) != 0) != expectedActive {
		t.Fatalf("stable activity mismatch: active=%#v expectedActive=%v", active, expectedActive)
	}
}

type activityResult struct {
	active []string
	err    error
}

type sequenceActivitySource struct {
	results []activityResult
	index   int
}

func (source *sequenceActivitySource) ActiveInstances(context.Context, []string) ([]string, error) {
	result := source.results[source.index]
	if source.index < len(source.results)-1 {
		source.index++
	}
	return result.active, result.err
}
