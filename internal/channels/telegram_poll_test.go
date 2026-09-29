package channels

import (
	"context"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
)

func TestTgNextBackoff(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{16 * time.Second, 30 * time.Second},
		{30 * time.Second, 30 * time.Second}, // capped
	}
	for _, c := range cases {
		if got := tgNextBackoff(c.in); got != c.want {
			t.Errorf("tgNextBackoff(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDeliverInboundDelivers(t *testing.T) {
	hub := chat.NewHub(4)
	in := chat.Inbound{Channel: "telegram", ChatID: "1", Content: "hi"}
	if !deliverInbound(context.Background(), hub, in) {
		t.Fatal("expected delivery into empty hub")
	}
	select {
	case got := <-hub.In:
		if got.Content != "hi" {
			t.Errorf("got %q", got.Content)
		}
	default:
		t.Fatal("message not in hub")
	}
}

// The poller must never block forever on a full hub — that is the permanent
// wedge this guards against (bot unresponsive until restart).
func TestDeliverInboundTimesOutOnFullHub(t *testing.T) {
	old := tgHubSendTimeout
	tgHubSendTimeout = 50 * time.Millisecond
	defer func() { tgHubSendTimeout = old }()

	hub := chat.NewHub(1)
	hub.In <- chat.Inbound{Channel: "telegram", ChatID: "0", Content: "filler"}

	done := make(chan bool, 1)
	go func() {
		done <- deliverInbound(context.Background(), hub, chat.Inbound{Channel: "telegram", ChatID: "1", Content: "x"})
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("expected timeout drop on full hub, got delivery")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deliverInbound blocked forever on full hub — permanent wedge regression")
	}
}

func TestDeliverInboundRespectsContextCancel(t *testing.T) {
	hub := chat.NewHub(1)
	hub.In <- chat.Inbound{Channel: "telegram", ChatID: "0", Content: "filler"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: must return immediately even with a long send timeout

	done := make(chan bool, 1)
	go func() {
		done <- deliverInbound(ctx, hub, chat.Inbound{Channel: "telegram", ChatID: "1", Content: "x"})
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("expected ctx-cancel drop, got delivery")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deliverInbound ignored context cancellation")
	}
}
