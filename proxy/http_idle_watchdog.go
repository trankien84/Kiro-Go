package proxy

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

var errUpstreamBodyIdleTimeout = errors.New("upstream response body made no progress before idle timeout")

type idleWatchdogReadCloser struct {
	body    io.ReadCloser
	cancel  context.CancelFunc
	timeout time.Duration

	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
	done     bool

	timedOut      atomic.Bool
	bodyCloseOnce sync.Once
	bodyCloseErr  error
}

func newIdleWatchdogReadCloser(
	body io.ReadCloser,
	timeout time.Duration,
	cancel context.CancelFunc,
) io.ReadCloser {
	if body == nil {
		if cancel != nil {
			cancel()
		}
		return httpNoBodyReadCloser{}
	}
	if timeout <= 0 {
		return &cancelOnCloseReadCloser{body: body, cancel: cancel}
	}

	watchdog := &idleWatchdogReadCloser{
		body:     body,
		cancel:   cancel,
		timeout:  timeout,
		deadline: time.Now().Add(timeout),
	}
	watchdog.timer = time.AfterFunc(timeout, watchdog.expire)
	return watchdog
}

func (w *idleWatchdogReadCloser) expire() {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return
	}

	// A read may have made progress while this callback was waiting for the
	// mutex. In that case the old timer firing is stale: honor the newer
	// deadline instead of timing out a healthy stream at the boundary.
	if remaining := time.Until(w.deadline); remaining > 0 {
		w.timer.Reset(remaining)
		w.mu.Unlock()
		return
	}

	w.done = true
	w.timedOut.Store(true)
	w.mu.Unlock()

	if w.cancel != nil {
		w.cancel()
	}
	_ = w.closeBody()
}

func (w *idleWatchdogReadCloser) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done || w.timedOut.Load() || w.timer == nil {
		return
	}

	w.deadline = time.Now().Add(w.timeout)
	// If Stop reports false, expire is already running (or about to run). It
	// will observe the renewed deadline under the same mutex and reschedule
	// itself, so resetting here would create competing timer generations.
	if w.timer.Stop() {
		w.timer.Reset(w.timeout)
	}
}

func (w *idleWatchdogReadCloser) finishWatching() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return
	}
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *idleWatchdogReadCloser) closeBody() error {
	w.bodyCloseOnce.Do(func() {
		w.bodyCloseErr = w.body.Close()
	})
	return w.bodyCloseErr
}

func (w *idleWatchdogReadCloser) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	if n > 0 {
		w.reset()
	}
	if err != nil {
		w.finishWatching()
		if w.timedOut.Load() {
			return n, errUpstreamBodyIdleTimeout
		}
	}
	return n, err
}

func (w *idleWatchdogReadCloser) Close() error {
	w.finishWatching()
	if w.cancel != nil {
		w.cancel()
	}
	return w.closeBody()
}

type cancelOnCloseReadCloser struct {
	body      io.ReadCloser
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

func (r *cancelOnCloseReadCloser) Read(p []byte) (int, error) {
	return r.body.Read(p)
}

func (r *cancelOnCloseReadCloser) Close() error {
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		r.closeErr = r.body.Close()
	})
	return r.closeErr
}

type httpNoBodyReadCloser struct{}

func (httpNoBodyReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (httpNoBodyReadCloser) Close() error             { return nil }
