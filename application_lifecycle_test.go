package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeShutdownServer struct {
	shutdown func(context.Context) error
	close    func() error
}

func (s fakeShutdownServer) Shutdown(ctx context.Context) error {
	if s.shutdown == nil {
		return nil
	}
	return s.shutdown(ctx)
}

func (s fakeShutdownServer) Close() error {
	if s.close == nil {
		return nil
	}
	return s.close()
}

type fakeShutdownHandler struct {
	shutdown func(context.Context) error
}

func (h fakeShutdownHandler) Shutdown(ctx context.Context) error {
	if h.shutdown == nil {
		return nil
	}
	return h.shutdown(ctx)
}

type fakeShutdownPool struct {
	close func(context.Context) error
}

func (p fakeShutdownPool) Close(ctx context.Context) error {
	if p.close == nil {
		return nil
	}
	return p.close(ctx)
}

func TestApplicationShutdownWaitsForRequestBeforeHandlerAndPool(t *testing.T) {
	tracker := newRequestTracker()
	if !tracker.enter() {
		t.Fatal("initial request was not admitted")
	}

	var mu sync.Mutex
	var order []string
	appendOrder := func(value string) {
		mu.Lock()
		order = append(order, value)
		mu.Unlock()
	}

	app := &application{
		tracker: tracker,
		server: fakeShutdownServer{shutdown: func(context.Context) error {
			appendOrder("server")
			tracker.leave()
			return nil
		}},
		handler: fakeShutdownHandler{shutdown: func(context.Context) error {
			appendOrder("handler")
			return nil
		}},
		pool: fakeShutdownPool{close: func(context.Context) error {
			appendOrder("pool")
			return nil
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := app.shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	want := []string{"server", "handler", "pool"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v want=%v", got, want)
	}
}

func TestApplicationShutdownForceClosesAndCancelsBeforeTeardown(t *testing.T) {
	tracker := newRequestTracker()
	if !tracker.enter() {
		t.Fatal("initial request was not admitted")
	}

	requestCanceled := make(chan struct{})
	var cancelOnce sync.Once
	cancelRequests := func() {
		cancelOnce.Do(func() {
			close(requestCanceled)
			tracker.leave()
		})
	}

	var closeCalls atomic.Int32
	var handlerCalled atomic.Bool
	var poolCalled atomic.Bool
	app := &application{
		tracker:        tracker,
		cancelRequests: cancelRequests,
		server: fakeShutdownServer{
			shutdown: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			close: func() error {
				closeCalls.Add(1)
				return nil
			},
		},
		handler: fakeShutdownHandler{shutdown: func(context.Context) error {
			select {
			case <-requestCanceled:
			default:
				t.Fatal("handler closed before request cancellation")
			}
			handlerCalled.Store(true)
			return nil
		}},
		pool: fakeShutdownPool{close: func(context.Context) error {
			if !handlerCalled.Load() {
				t.Fatal("pool closed before handler")
			}
			poolCalled.Store(true)
			return nil
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	err := app.shutdown(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected graceful timeout to be reported, got %v", err)
	}
	if closeCalls.Load() == 0 {
		t.Fatal("Server.Close was not called")
	}
	if !handlerCalled.Load() || !poolCalled.Load() {
		t.Fatalf("teardown incomplete: handler=%t pool=%t", handlerCalled.Load(), poolCalled.Load())
	}
}

func TestRequestTrackerRejectsNewAdmissionAfterDrain(t *testing.T) {
	tracker := newRequestTracker()
	tracker.startDrain()
	called := false
	handler := tracker.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", recorder.Code)
	}
	if called {
		t.Fatal("draining tracker called downstream handler")
	}
}

func TestApplicationShutdownUsesOneSharedDeadline(t *testing.T) {
	var handlerDeadline, poolDeadline time.Time
	app := &application{
		tracker: newRequestTracker(),
		server: fakeShutdownServer{
			shutdown: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		},
		handler: fakeShutdownHandler{shutdown: func(ctx context.Context) error {
			handlerDeadline, _ = ctx.Deadline()
			return nil
		}},
		pool: fakeShutdownPool{close: func(ctx context.Context) error {
			poolDeadline, _ = ctx.Deadline()
			return nil
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Millisecond)
	parentDeadline, _ := ctx.Deadline()
	defer cancel()
	started := time.Now()
	if err := app.shutdown(ctx); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected HTTP grace timeout, got %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 230*time.Millisecond {
		t.Fatalf("HTTP phase consumed the whole deadline: %s", elapsed)
	}
	if !handlerDeadline.Equal(parentDeadline) || !poolDeadline.Equal(parentDeadline) {
		t.Fatalf("phase deadlines handler=%v pool=%v parent=%v", handlerDeadline, poolDeadline, parentDeadline)
	}
}

func TestApplicationShutdownDoesNotClosePoolWhenHandlerFails(t *testing.T) {
	handlerErr := errors.New("handler still running")
	var poolCalled atomic.Bool
	app := &application{
		tracker: newRequestTracker(),
		server:  fakeShutdownServer{},
		handler: fakeShutdownHandler{shutdown: func(context.Context) error {
			return handlerErr
		}},
		pool: fakeShutdownPool{close: func(context.Context) error {
			poolCalled.Store(true)
			return nil
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := app.shutdown(ctx)
	if !errors.Is(err, handlerErr) {
		t.Fatalf("shutdown error=%v", err)
	}
	if poolCalled.Load() {
		t.Fatal("pool was closed under a handler that failed to stop")
	}
}

func TestApplicationShutdownDoesNotTeardownIfRequestsMissFinalDeadline(t *testing.T) {
	tracker := newRequestTracker()
	if !tracker.enter() {
		t.Fatal("initial request was not admitted")
	}
	var handlerCalled, poolCalled atomic.Bool
	app := &application{
		tracker: tracker,
		server: fakeShutdownServer{shutdown: func(context.Context) error {
			return nil
		}},
		handler: fakeShutdownHandler{shutdown: func(context.Context) error {
			handlerCalled.Store(true)
			return nil
		}},
		pool: fakeShutdownPool{close: func(context.Context) error {
			poolCalled.Store(true)
			return nil
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := app.shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected request drain timeout, got %v", err)
	}
	if handlerCalled.Load() || poolCalled.Load() {
		t.Fatalf("unsafe teardown after undrained request: handler=%t pool=%t", handlerCalled.Load(), poolCalled.Load())
	}
	tracker.leave()
}
