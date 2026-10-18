package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const defaultHTTPShutdownShare = 2.0 / 3.0

type shutdownServer interface {
	Shutdown(context.Context) error
	Close() error
}

type shutdownHandler interface {
	Shutdown(context.Context) error
}

type shutdownPool interface {
	Close(context.Context) error
}

// requestTracker closes admission before waiting, so no new request can enter
// after shutdown begins. The original ResponseWriter is passed through unchanged,
// preserving optional net/http interfaces such as Flusher and Hijacker.
type requestTracker struct {
	mu       sync.Mutex
	draining bool
	active   int
	drained  chan struct{}
}

func newRequestTracker() *requestTracker {
	return &requestTracker{}
}

func (t *requestTracker) enter() bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return false
	}
	t.active++
	return true
}

func (t *requestTracker) leave() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active <= 0 {
		panic("requestTracker.leave without matching enter")
	}
	t.active--
	if t.draining && t.active == 0 && t.drained != nil {
		close(t.drained)
		t.drained = nil
	}
}

func (t *requestTracker) startDrain() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return
	}
	t.draining = true
	if t.active != 0 {
		t.drained = make(chan struct{})
	}
}

func (t *requestTracker) wait(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	t.mu.Lock()
	if !t.draining {
		t.mu.Unlock()
		return errors.New("request drain has not started")
	}
	if t.active == 0 {
		t.mu.Unlock()
		return nil
	}
	drained := t.drained
	t.mu.Unlock()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *requestTracker) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !t.enter() {
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		defer t.leave()
		next.ServeHTTP(w, r)
	})
}

type application struct {
	server         shutdownServer
	handler        shutdownHandler
	pool           shutdownPool
	tracker        *requestTracker
	cancelRequests context.CancelFunc
}

// httpShutdownContext reserves part of the single application deadline for
// forced cancellation, request drain, Handler shutdown, and the final stats
// flush. It never extends the parent's deadline.
func httpShutdownContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	deadline, ok := parent.Deadline()
	if !ok {
		return context.WithCancel(parent)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithCancel(parent)
	}
	grace := time.Duration(float64(remaining) * defaultHTTPShutdownShare)
	if grace <= 0 || grace >= remaining {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, grace)
}

// shutdown uses one shared deadline for every phase. Handler-owned state and the
// process-global AccountPool are never torn down while an admitted request may
// still be using them.
func (a *application) shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if a == nil {
		return nil
	}

	var shutdownErrors []error
	if a.tracker != nil {
		a.tracker.startDrain()
	}

	if a.server != nil {
		httpCtx, cancelHTTP := httpShutdownContext(ctx)
		err := a.server.Shutdown(httpCtx)
		cancelHTTP()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("graceful HTTP shutdown: %w", err))
			if a.cancelRequests != nil {
				a.cancelRequests()
			}
			if closeErr := a.server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("force-close HTTP server: %w", closeErr))
			}
		}
	}

	if a.tracker != nil {
		if err := a.tracker.wait(ctx); err != nil {
			if a.cancelRequests != nil {
				a.cancelRequests()
			}
			if a.server != nil {
				if closeErr := a.server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
					shutdownErrors = append(shutdownErrors, fmt.Errorf("force-close HTTP server after drain timeout: %w", closeErr))
				}
			}
			shutdownErrors = append(shutdownErrors, fmt.Errorf("wait for active requests: %w", err))
			// Active Handler work still exists. Do not close Handler/Pool beneath it;
			// process exit is the final cleanup boundary after the deadline.
			return errors.Join(shutdownErrors...)
		}
	}

	if a.cancelRequests != nil {
		a.cancelRequests()
	}

	if a.handler != nil {
		if err := a.handler.Shutdown(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("shutdown handler: %w", err))
			// A Handler that did not stop may still use the process-global pool.
			return errors.Join(shutdownErrors...)
		}
	}

	if a.pool != nil {
		if err := a.pool.Close(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("close account pool: %w", err))
		}
	}
	return errors.Join(shutdownErrors...)
}
