package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The warning code reaches the browser. The old service reported a results file
// that had not been published yet as NO_RESULTS and an unreadable one as
// READ_FAILED, and the two must stay distinct for the payloads to match.
func TestSourceWarningFor(t *testing.T) {
	_, statErr := os.Stat(filepath.Join(t.TempDir(), "absent.json"))
	if statErr == nil {
		t.Fatal("expected the probe file to be missing")
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, SourceWarningNone},
		{"missing file", statErr, SourceWarningNoResults},
		{"wrapped missing file", fmt.Errorf("read results: %w", statErr), SourceWarningNoResults},
		{"sentinel", fs.ErrNotExist, SourceWarningNoResults},
		{"permission", errors.New("permission denied"), SourceWarningReadFailed},
		{"broken json", errors.New("unexpected end of JSON input"), SourceWarningReadFailed},
	}
	for _, tc := range cases {
		if got := SourceWarningFor(tc.err); got != tc.want {
			t.Fatalf("%s: SourceWarningFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The history view compares importedAt against the JavaScript ISO shape, so the
// writer must always emit exactly three fractional digits and a Z suffix.
func TestNowTimestampMatchesJavaScriptISO(t *testing.T) {
	raw := NowTimestamp()
	if len(raw) != 24 {
		t.Fatalf("NowTimestamp() = %q, want 24 characters", raw)
	}
	if _, err := time.Parse(jsTimestampLayout, raw); err != nil {
		t.Fatalf("NowTimestamp() = %q: %v", raw, err)
	}
}
