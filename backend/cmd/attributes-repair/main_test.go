package main

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, tt := range []struct {
		name     string
		limit    int
		priority int
		wantErr  bool
	}{
		{name: "defaults", limit: defaultLimit},
		{name: "unlimited", limit: 0},
		{name: "priority lane", limit: 100, priority: 1},
		{name: "negative limit", limit: -1, wantErr: true},
		{name: "negative priority", limit: 10, priority: -1, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := validate(tt.limit, tt.priority); (err != nil) != tt.wantErr {
				t.Fatalf("validate(%d, %d) error = %v, wantErr %t", tt.limit, tt.priority, err, tt.wantErr)
			}
		})
	}
}

// The repair set must never reopen an address that a source positively placed
// in a hosting network, and must never pick up a node that is not available or
// was never residential.
func TestLostResidentialGuards(t *testing.T) {
	for _, guard := range []string{
		"n.available",
		"n.historical_residential",
		"<> 'residential'",
		"NOT EXISTS",
	} {
		if !strings.Contains(lostResidential, guard) {
			t.Fatalf("lostResidential lost %q:\n%s", guard, lostResidential)
		}
	}
	if !strings.Contains(confidentDatacenter, "provider-conflict") || !strings.Contains(confidentDatacenter, "hosting") {
		t.Fatalf("confident datacenter answers are no longer excluded: %s", confidentDatacenter)
	}
	// Several identities can share one exit IP; a duplicate key would abort the
	// whole ON CONFLICT statement.
	if !strings.Contains(lostResidential, "GROUP BY a.ip") {
		t.Fatalf("selection is no longer deduplicated per address:\n%s", lostResidential)
	}
}

func TestEnqueueLeavesLiveWorkAlone(t *testing.T) {
	if !strings.Contains(enqueueLost, "WHERE node_explorer_work.state='done'") {
		t.Fatalf("enqueue would reopen a running task:\n%s", enqueueLost)
	}
}
