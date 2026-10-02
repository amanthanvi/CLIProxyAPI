package usage

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type retainingQueueTestPlugin struct {
	entered       chan queueItem
	releaseFirst  chan struct{}
	releaseSecond chan struct{}
}

func (p *retainingQueueTestPlugin) HandleUsage(ctx context.Context, record Record) {
	p.entered <- queueItem{ctx: ctx, record: record}
	if record.RequestID == "first" {
		<-p.releaseFirst
	} else {
		<-p.releaseSecond
	}
}

func TestManagerClearsDispatchedQueueSlot(t *testing.T) {
	plugin := &retainingQueueTestPlugin{
		entered:       make(chan queueItem, 2),
		releaseFirst:  make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
	manager := NewManager(1)
	manager.Register(plugin)
	t.Cleanup(func() {
		select {
		case <-plugin.releaseFirst:
		default:
			close(plugin.releaseFirst)
		}
		select {
		case <-plugin.releaseSecond:
		default:
			close(plugin.releaseSecond)
		}
		manager.Stop()
	})
	waitDispatch := func() queueItem {
		t.Helper()
		select {
		case item := <-plugin.entered:
			return item
		case <-time.After(5 * time.Second):
			t.Fatal("usage dispatch timed out")
			return queueItem{}
		}
	}

	first := Record{RequestID: "first", TraceID: "first-trace"}
	manager.Publish(context.Background(), first)
	if got := waitDispatch(); !reflect.DeepEqual(got.record, first) {
		t.Fatalf("first dispatch = %#v, want %#v", got.record, first)
	}

	secondCtx := context.WithValue(context.Background(), struct{}{}, "retained value")
	second := Record{RequestID: "second", TraceID: "second-trace", Model: "synthetic-model"}
	manager.Publish(secondCtx, second)
	manager.mu.Lock()
	if len(manager.queue) != 1 {
		queued := len(manager.queue)
		manager.mu.Unlock()
		t.Fatalf("queued records = %d, want 1", queued)
	}
	backing := manager.queue
	manager.mu.Unlock()

	close(plugin.releaseFirst)
	got := waitDispatch()
	if got.ctx != secondCtx || !reflect.DeepEqual(got.record, second) {
		t.Fatalf("second dispatch changed context or record: same_context=%t record=%#v", got.ctx == secondCtx, got.record)
	}
	manager.mu.Lock()
	remaining := len(manager.queue)
	stale := backing[0]
	manager.mu.Unlock()
	if remaining != 0 || stale.ctx != nil || !reflect.DeepEqual(stale.record, Record{}) {
		t.Fatalf("dispatched slot retained data: remaining=%d context_retained=%t record=%#v", remaining, stale.ctx != nil, stale.record)
	}
	close(plugin.releaseSecond)
}
