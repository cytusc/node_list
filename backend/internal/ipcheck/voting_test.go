package ipcheck

import (
	"fmt"
	"testing"
	"time"
)

func evidence(provider string, points int, at time.Time) Evidence {
	e := Evidence{Provider: provider, IP: "8.8.8.8", CheckedAt: at.UTC().Format(time.RFC3339Nano), Outcome: "unknown"}
	if points == 1 {
		e.Outcome = "supports-residential"
	}
	if points == -1 {
		e.Outcome = "datacenter"
	}
	return e
}

func TestAllThreeSourceVoteCombinations(t *testing.T) {
	now := time.Now()
	for _, a := range []int{-1, 0, 1} {
		for _, b := range []int{-1, 0, 1} {
			for _, c := range []int{-1, 0, 1} {
				t.Run(fmt.Sprint(a, b, c), func(t *testing.T) {
					positive, negative := 0, 0
					for _, v := range []int{a, b, c} {
						if v == 1 {
							positive++
						}
						if v == -1 {
							negative++
						}
					}
					want := "unknown"
					switch {
					case positive >= 2:
						want = "residential"
					case negative >= 2:
						want = "datacenter"
					case positive > 0 && negative > 0:
						want = "disputed"
					}
					r := Score("8.8.8.8", []Evidence{evidence("ippure", a, now), evidence("ipinfo", b, now), evidence("ip-api", c, now)}, now)
					if r.State != want || r.Score != a+b+c || r.ResidentialVotes != positive || r.DatacenterVotes != negative || len(r.Votes) != 3 {
						t.Fatalf("wrong votes: %+v", r)
					}
					if r.HasDisagreement != (positive > 0 && negative > 0) {
						t.Fatal("minority opinion was lost")
					}
				})
			}
		}
	}
}

func TestVotesCannotBeMultipliedOrTransferred(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		extra Evidence
	}{
		{"same provider", evidence("ippure", 1, now)},
		{"unrequested fourth provider", evidence("ipapi.is", 1, now)},
		{"stale", evidence("ipinfo", 1, now.Add(-25*time.Hour))},
		{"future", evidence("ipinfo", 1, now.Add(time.Hour))},
		{"different IP", Evidence{Provider: "ipinfo", IP: "1.1.1.1", CheckedAt: now.Format(time.RFC3339Nano), Outcome: "supports-residential"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Score("8.8.8.8", []Evidence{evidence("ippure", 1, now), tc.extra}, now)
			if r.State != "unknown" || r.ResidentialVotes != 1 {
				t.Fatalf("invalid evidence counted: %+v", r)
			}
		})
	}
	old := evidence("ipinfo", 1, now.Add(-time.Minute))
	failed := evidence("ipinfo", 0, now)
	r := Score("8.8.8.8", []Evidence{old, failed, evidence("ippure", 1, now)}, now)
	if r.State != "unknown" {
		t.Fatal("a failed fresh lookup reused an older positive vote")
	}
}

func TestAttributesAgreeWithMajority(t *testing.T) {
	r := Result{State: "residential"}
	a := r.Attributes([]string{"机房IP", "商业用途"})
	if fmt.Sprint(a) != "[商业用途 住宅IP]" {
		t.Fatalf("stale label retained: %v", a)
	}
}
