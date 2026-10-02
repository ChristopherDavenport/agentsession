package wake

import (
	"testing"
	"time"
)

func TestHubWakesEveryFollowerOnce(t *testing.T) {
	var h Hub
	c1, d1 := h.Watch("s")
	c2, d2 := h.Watch("s")
	defer d1()
	defer d2()
	a, b := c1(), c2()
	h.Notify("s")
	for _, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		default:
			t.Fatal("a follower was not woken")
		}
	}
	select {
	case <-c1():
		t.Fatal("the replacement channel is already closed")
	default:
	}
	h.Notify("other") // nobody follows it
}

func TestHubReleaseForgetsTheSession(t *testing.T) {
	var h Hub
	_, done := h.Watch("s")
	done()
	done() // twice is harmless
	if len(h.m) != 0 {
		t.Fatalf("%d sessions still watched", len(h.m))
	}
}

func TestPollBacksOffAndResets(t *testing.T) {
	p := Poll{Min: 100 * time.Millisecond, Max: time.Second}
	var got []time.Duration
	for range 6 {
		got = append(got, p.Next())
	}
	want := []time.Duration{100, 200, 400, 800, 1000, 1000}
	for i, w := range want {
		if got[i] != w*time.Millisecond {
			t.Fatalf("wait %d is %v, want %v", i, got[i], w*time.Millisecond)
		}
	}
	p.Reset()
	if d := p.Next(); d != 100*time.Millisecond {
		t.Fatalf("after Reset the wait is %v", d)
	}
}

func TestWait(t *testing.T) {
	done := make(chan struct{})
	if !Wait(done, nil, time.Millisecond) {
		t.Fatal("a timer ended the wait as if done were closed")
	}
	c := make(chan struct{})
	close(c)
	if !Wait(done, c, time.Hour) {
		t.Fatal("a ring did not end the wait")
	}
	close(done)
	if Wait(done, nil, time.Hour) {
		t.Fatal("done did not end the wait")
	}
}
