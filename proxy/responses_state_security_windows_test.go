//go:build windows

package proxy

import (
	"encoding/json"
	"kiro-go/internal/privatefile"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingResponsesStateIsHardenedBeforeTrustWindows(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	dir := t.TempDir()
	namespacePath := filepath.Join(dir, responseInstanceNamespaceFile)
	wantNamespace := strings.Repeat("a", responseInstanceNamespaceSize*2)
	if err := os.WriteFile(namespacePath, []byte(wantNamespace+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	got, err := readResponseInstanceNamespace(namespacePath)
	if err != nil || got != wantNamespace {
		t.Fatalf("read namespace=%q err=%v", got, err)
	}
	ok, err := privatefile.InspectOwnerPrivate(namespacePath, 0o600)
	if err != nil || !ok {
		t.Fatalf("legacy .instance-id was not hardened: ok=%v err=%v", ok, err)
	}

	doc := validStoredResponseDocV1ForTest()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	responsePath := filepath.Join(dir, "response.json")
	if err := os.WriteFile(responsePath, data, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := readStoredResponseDoc(responsePath, doc.ID); err != nil {
		t.Fatalf("read legacy response state: %v", err)
	}
	ok, err = privatefile.InspectOwnerPrivate(responsePath, 0o600)
	if err != nil || !ok {
		t.Fatalf("legacy response JSON was not hardened: ok=%v err=%v", ok, err)
	}
}
