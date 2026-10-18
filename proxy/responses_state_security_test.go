//go:build !windows

package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingResponsesStateIsHardenedBeforeTrustUnix(t *testing.T) {
	resetResponseStateHealthForTest()
	t.Cleanup(resetResponseStateHealthForTest)

	dir := t.TempDir()
	namespacePath := filepath.Join(dir, responseInstanceNamespaceFile)
	wantNamespace := strings.Repeat("a", responseInstanceNamespaceSize*2)
	if err := os.WriteFile(namespacePath, []byte(wantNamespace+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readResponseInstanceNamespace(namespacePath)
	if err != nil || got != wantNamespace {
		t.Fatalf("read namespace=%q err=%v", got, err)
	}
	if info, err := os.Stat(namespacePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("legacy .instance-id mode=%v err=%v", func() os.FileMode {
			if info != nil {
				return info.Mode().Perm()
			}
			return 0
		}(), err)
	}

	doc := validStoredResponseDocV1ForTest()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	responsePath := filepath.Join(dir, "response.json")
	if err := os.WriteFile(responsePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readStoredResponseDoc(responsePath, doc.ID); err != nil {
		t.Fatalf("read legacy response state: %v", err)
	}
	if info, err := os.Stat(responsePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("legacy response mode=%v err=%v", func() os.FileMode {
			if info != nil {
				return info.Mode().Perm()
			}
			return 0
		}(), err)
	}
}
