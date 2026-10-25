package domain

import (
	"encoding/json"
	"os"
	"testing"
)

// These persisted identities were captured from the retired JavaScript runtime.
// Newer Go share-link normalization is covered separately by sharelink tests.
func TestPersistedIdentityContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/identity-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Node     Node   `json:"node"`
		Identity string `json:"identity"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for i, f := range fixtures {
		if got := AggregateIdentity(f.Node); got != f.Identity {
			t.Errorf("fixture %d: got %s, want %s", i, got, f.Identity)
		}
	}
}
