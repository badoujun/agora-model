package logging

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ok", "ok"},
		{"", ""},
		{"authorization=Bearer sk-abcdef123456", "authorization=Bearer ***REDACTED***"},
		{"key gw-0123456789abcdef leaked", "key ***REDACTED*** leaked"},
		{"短串不误伤：sk-short 原样保留", "短串不误伤：sk-short 原样保留"},
	}
	for _, tc := range cases {
		if got := Redact(tc.in, 0); got != tc.want {
			t.Errorf("Redact(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}

	long := strings.Repeat("x", 100)
	if got := Redact(long, 10); len(got) != 10 {
		t.Errorf("截断长度 = %d, 期望 10", len(got))
	}
}

type fakeWriter struct {
	mu    sync.Mutex
	got   []Entry
	err   error
	delay time.Duration
}

func (f *fakeWriter) InsertLogs(ctx context.Context, entries []Entry) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, entries...)
	return nil
}

func (f *fakeWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func TestRecorderFlushesOnBatchSize(t *testing.T) {
	writer := &fakeWriter{}
	rec := NewRecorder(writer, nil, RecorderOptions{QueueSize: 64, BatchSize: 3, FlushEvery: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec.Start(ctx)

	for i := 0; i < 3; i++ {
		rec.Enqueue(Entry{RequestID: "r", StatusCode: 200})
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if writer.count() == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if writer.count() != 3 {
		t.Fatalf("达到 BatchSize 应立即写入，实际 %d 条", writer.count())
	}
}

func TestRecorderFlushesOnShutdown(t *testing.T) {
	writer := &fakeWriter{}
	rec := NewRecorder(writer, nil, RecorderOptions{QueueSize: 16, BatchSize: 100, FlushEvery: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	rec.Start(ctx)

	rec.Enqueue(Entry{RequestID: "pending-1"})
	rec.Enqueue(Entry{RequestID: "pending-2"})

	cancel() // 模拟优雅关闭

	if !rec.Wait(3 * time.Second) {
		t.Fatal("关闭时应在超时前完成写入")
	}
	if writer.count() != 2 {
		t.Fatalf("关闭时应刷出剩余 %d 条，实际 %d 条", 2, writer.count())
	}
	written, dropped := rec.Stats()
	if written != 2 || dropped != 0 {
		t.Fatalf("Stats = written=%d dropped=%d, 期望 2/0", written, dropped)
	}
}

func TestRecorderDropsWhenQueueFull(t *testing.T) {
	writer := &fakeWriter{}
	// 不启动 loop：队列很快填满，用于验证「丢弃而不阻塞」
	rec := NewRecorder(writer, nil, RecorderOptions{QueueSize: 2, BatchSize: 1000, FlushEvery: time.Hour})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			rec.Enqueue(Entry{RequestID: "r"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue 不应阻塞转发路径")
	}

	_, dropped := rec.Stats()
	if dropped == 0 {
		t.Fatal("队列满时应计入丢弃")
	}
}

func TestRecorderCountsWriteErrorsAsDropped(t *testing.T) {
	writer := &fakeWriter{err: errors.New("db down")}
	rec := NewRecorder(writer, nil, RecorderOptions{QueueSize: 16, BatchSize: 1, FlushEvery: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec.Start(ctx)

	rec.Enqueue(Entry{RequestID: "x"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, dropped := rec.Stats(); dropped == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("写入失败应计入丢弃计数")
}
