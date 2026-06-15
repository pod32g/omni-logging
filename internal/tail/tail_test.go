package tail

import (
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

func ev(level model.Level, msg string) model.LogEvent {
	return model.LogEvent{Timestamp: time.Now(), Level: level, Message: msg, Service: "api"}
}

func TestHub_PublishOnlyMatching(t *testing.T) {
	hub := NewHub()
	q, _ := query.Parse("level=error")
	sub := hub.Subscribe(q, 8)
	defer sub.Close()

	hub.Publish(ev(model.LevelInfo, "ignored"))
	hub.Publish(ev(model.LevelError, "boom"))

	select {
	case got := <-sub.C:
		if got.Message != "boom" {
			t.Fatalf("got %q, want boom", got.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a matching event")
	}

	select {
	case got := <-sub.C:
		t.Fatalf("unexpected second event: %q", got.Message)
	default:
	}
}

func TestHub_DropsWhenFull(t *testing.T) {
	hub := NewHub()
	q, _ := query.Parse("")
	sub := hub.Subscribe(q, 2)
	defer sub.Close()

	for i := 0; i < 10; i++ {
		hub.Publish(ev(model.LevelInfo, "x"))
	}
	if sub.Dropped() == 0 {
		t.Fatal("expected some events to be dropped when buffer is full")
	}
	if got := len(sub.C); got != 2 {
		t.Fatalf("buffer len = %d, want 2", got)
	}
}

func TestHub_DroppedTotalAggregates(t *testing.T) {
	hub := NewHub()
	q, _ := query.Parse("")
	sub := hub.Subscribe(q, 2)
	defer sub.Close()

	for i := 0; i < 10; i++ {
		hub.Publish(ev(model.LevelInfo, "x"))
	}
	if hub.DroppedTotal() == 0 {
		t.Fatal("expected hub DroppedTotal > 0 when a subscriber buffer fills")
	}
	if hub.DroppedTotal() != sub.Dropped() {
		t.Fatalf("hub DroppedTotal = %d, want = sub.Dropped() = %d", hub.DroppedTotal(), sub.Dropped())
	}
}

func TestHub_UnsubscribeStopsDelivery(t *testing.T) {
	hub := NewHub()
	q, _ := query.Parse("")
	sub := hub.Subscribe(q, 4)
	if hub.SubscriberCount() != 1 {
		t.Fatalf("subscriber count = %d, want 1", hub.SubscriberCount())
	}
	sub.Close()
	if hub.SubscriberCount() != 0 {
		t.Fatalf("subscriber count after close = %d, want 0", hub.SubscriberCount())
	}
	// Publishing after close must not panic.
	hub.Publish(ev(model.LevelInfo, "x"))
}
