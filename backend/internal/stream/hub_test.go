package stream

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readEvents(t *testing.T, url string, header map[string]string, want int, timeout time.Duration) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	n := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			n++
			if n == want {
				return n
			}
		}
	}
	return n
}

func TestPublishReachesSubscriber(t *testing.T) {
	h := NewHub(64, 16)
	srv := httptest.NewServer(h)
	defer srv.Close()
	got := make(chan int)
	go func() { got <- readEvents(t, srv.URL, nil, 3, 3*time.Second) }()
	for h.Clients() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		h.Publish("test", i)
	}
	if n := <-got; n != 3 {
		t.Fatalf("3 olay bekleniyordu, %d", n)
	}
}

func TestReplayWithLastEventID(t *testing.T) {
	h := NewHub(8, 16)
	for i := 0; i < 10; i++ {
		h.Publish("x", i) // halka 8: 3..10 tutulur
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	n := readEvents(t, srv.URL, map[string]string{"Last-Event-ID": "6"}, 4, time.Second)
	if n != 4 {
		t.Fatalf("7..10 tekrar oynatılmalı, %d", n)
	}
}

func TestSlowClientDoesNotBlockPublisher(t *testing.T) {
	h := NewHub(16, 2)
	c := h.subscribe() // hiç okumayan istemci
	defer h.unsubscribe(c)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Publish("x", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("yayıncı yavaş istemci yüzünden bloklandı")
	}
}

// 2000 olay/dk'nın çok üstünde: 2000 olay ~1 sn içinde 5 istemciye ulaşmalı.
func TestThroughput2000Events(t *testing.T) {
	h := NewHub(4096, 4096)
	srv := httptest.NewServer(h)
	defer srv.Close()
	var total int64
	done := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		go func() {
			atomic.AddInt64(&total, int64(readEvents(t, srv.URL, nil, 2000, 5*time.Second)))
			done <- struct{}{}
		}()
	}
	for h.Clients() < 5 {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	for i := 0; i < 2000; i++ {
		h.Publish("load", map[string]int{"i": i})
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	if total != 10000 {
		t.Fatalf("10000 teslim bekleniyordu, %d", total)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("çok yavaş: %s", d)
	}
}

func TestCloseEndsStreams(t *testing.T) {
	h := NewHub(8, 8)
	srv := httptest.NewServer(h)
	defer srv.Close()
	ended := make(chan struct{})
	go func() { readEvents(t, srv.URL, nil, 100, 5*time.Second); close(ended) }()
	for h.Clients() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	h.Close()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("Close akışı sonlandırmalı")
	}
}
