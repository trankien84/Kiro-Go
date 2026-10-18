package proxy

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *blockingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func TestIdleWatchdogInterruptsStalledBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := newIdleWatchdogReadCloser(newBlockingReadCloser(), 30*time.Millisecond, cancel)
	defer body.Close()

	started := time.Now()
	_, err := body.Read(make([]byte, 1))
	if !errors.Is(err, errUpstreamBodyIdleTimeout) {
		t.Fatalf("read error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("watchdog took too long: %s", elapsed)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("watchdog did not cancel request context")
	}
}

func TestCustomAPIClientIsSharedWithoutWallClockTimeout(t *testing.T) {
	first := customApiHTTPClient()
	second := customApiHTTPClient()
	if first != second {
		t.Fatal("custom API client is not shared")
	}
	if first.Timeout != 0 {
		t.Fatalf("custom API client has wall-clock timeout %s", first.Timeout)
	}
	if first.Transport != customAPIDirectTransport {
		t.Fatal("custom API client does not use shared transport")
	}
}

type pacedReadCloser struct {
	delay     time.Duration
	remaining int
}

func (r *pacedReadCloser) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	p[0] = 'x'
	r.remaining--
	return 1, nil
}

func (r *pacedReadCloser) Close() error { return nil }

func TestIdleWatchdogUsesNoProgressRatherThanTotalDuration(t *testing.T) {
	body := newIdleWatchdogReadCloser(
		&pacedReadCloser{delay: 20 * time.Millisecond, remaining: 4},
		50*time.Millisecond,
		func() {},
	)
	defer body.Close()

	started := time.Now()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(data) != "xxxx" {
		t.Fatalf("data=%q", data)
	}
	if elapsed := time.Since(started); elapsed <= 50*time.Millisecond {
		t.Fatalf("test did not exceed one idle window: %s", elapsed)
	}
}
