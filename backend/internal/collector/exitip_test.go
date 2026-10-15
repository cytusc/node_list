package collector

import "testing"

func TestBusinessASNDoesNotBecomeDatacenter(t *testing.T) {
	for _, tc := range []struct{ kind, want string }{
		{"business", "商业宽带"}, {"hosting", "机房IP"}, {"isp", "住宅IP"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			basic := map[string]any{
				"ip":     "8.8.8.8",
				"asn":    map[string]any{"number": float64(15169), "organization": "Example", "type": tc.kind},
				"traits": map[string]any{"is_broadcast": false},
			}
			v, err := NormalizeIPPure("8.8.8.8", basic, map[string]any{"risk_score": float64(10)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Attributes) != 1 || v.Attributes[0] != tc.want {
				t.Fatalf("got %v", v.Attributes)
			}
		})
	}
}
