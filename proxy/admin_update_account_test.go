package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// TestApiUpdateAccountPersistsBedrockRegionChange is the regression test for
// H2: config.UpdateAccount's preserve-list (config/config.go) force-restores
// a fixed set of credential/endpoint fields from the stored account so a stale
// snapshot can't clobber a concurrent token rotation. Region was unconditionally
// on that list, so apiUpdateAccount's Bedrock region edit (handler.go ~3182,
// which sets existing.Region to the NEW value) got silently reverted back to
// the OLD value by the very next call to config.UpdateAccount — the endpoint
// returned 200 and the UI could toast success while the saved region reverted
// and clearBedrockModelCache/clearBedrockRegionRoutes had already flushed the
// cache for nothing. The fix only falls back to the stored Region when the
// caller left it blank, so an explicit new region must now persist.
func TestApiUpdateAccountPersistsBedrockRegionChange(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "bedrock-1", AuthMethod: "bedrock", Enabled: true,
		BedrockAccessKeyID: "AKIA...", BedrockSecretAccessKey: "secret", Region: "us-east-1",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	restoreBedrockClient := installNoNetworkBedrockHTTPClientForTest()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	cleanupHandlerBeforeRestore(t, h, restoreBedrockClient)

	body := bytes.NewBufferString(`{"region":"eu-west-1"}`)
	req := httptest.NewRequest(http.MethodPut, "/accounts/bedrock-1", body)
	w := httptest.NewRecorder()
	h.apiUpdateAccount(w, req, "bedrock-1")

	if w.Code != 0 && w.Code != http.StatusOK {
		t.Fatalf("apiUpdateAccount status = %d, body=%s", w.Code, w.Body.String())
	}

	got, ok := config.GetAccountByID("bedrock-1")
	if !ok {
		t.Fatal("account vanished")
	}
	if got.Region != "eu-west-1" {
		t.Fatalf("Region = %q, want eu-west-1 (config.UpdateAccount's preserve-list silently reverted the save)", got.Region)
	}
}

// TestUpdateAccountPreservesRegionWhenCallerLeavesItBlank guards the OTHER
// half of the H2 fix: a caller that never touches Region (the common case —
// enabled/nickname/weight-only edits, and every non-Bedrock account, whose
// Region is deliberately not editable via this endpoint) must still see the
// stored Region survive, exactly like every other field on the preserve-list.
func TestUpdateAccountPreservesRegionWhenCallerLeavesItBlank(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "idc-1", AuthMethod: "idc", Enabled: true, AccessToken: "tok", Region: "eu-central-1",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	// A caller snapshot that never set Region (config.UpdateAccount's real
	// call sites read the stored account first, so its Region field is empty
	// only when truly untouched — simulated directly here).
	if err := config.UpdateAccount("idc-1", config.Account{ID: "idc-1", Enabled: false}); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}

	got, ok := config.GetAccountByID("idc-1")
	if !ok {
		t.Fatal("account vanished")
	}
	if got.Region != "eu-central-1" {
		t.Fatalf("Region = %q, want the preserved eu-central-1", got.Region)
	}
}
