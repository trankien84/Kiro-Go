package proxy

import (
	"testing"
	"time"
)

func TestImportWatcherStartupDelayStopsWithHandlerLifecycle(t *testing.T) {
	t.Setenv("KIRO_IMPORT_WATCH", "1")
	t.Setenv("KIRO_IMPORT_DIR", t.TempDir())

	h := &Handler{stopRefresh: make(chan struct{})}
	h.startImportWatcher()
	close(h.stopRefresh)

	done := make(chan struct{})
	go func() {
		h.backgroundWG.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("import watcher did not stop promptly with handler lifecycle")
	}
}
