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
					case positive > 0 && positive >= negative:
						want = "residential"
					case negative > positive:
						want = "datacenter"
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
			if r.State != "residential" || r.ResidentialVotes != 1 {
				t.Fatalf("invalid evidence counted: %+v", r)
			}
		})
	}
	old := evidence("ipinfo", 1, now.Add(-time.Minute))
	failed := evidence("ipinfo", 0, now)
	r := Score("8.8.8.8", []Evidence{old, failed, evidence("ippure", 1, now)}, now)
	if r.State != "residential" {
		t.Fatal("a failed fresh lookup reused an older positive vote")
	}
}

func TestAttributesReplaceStaleLabelsWithSourceTypes(t *testing.T) {
	r := Result{State: "residential", Types: []TypeEvidence{{Provider: "ipinfo", UsageType: "住宅IP"}}}
	a := r.Attributes([]string{"机房IP", "商业用途"})
	if fmt.Sprint(a) != "[商业用途 住宅IP]" {
		t.Fatalf("stale label retained: %v", a)
	}
}

// A refusal says nothing about the address. It must not outvote, replace or
// outlive the last answer the same provider gave, because the persisted Result
// is the only copy of that answer.
func TestFailedLookupCannotEvictAnAnswer(t *testing.T) {
	now := time.Now()
	answer := evidence("ippure", 1, now.Add(-2*time.Hour))
	for _, status := range []string{"request-failed", "quota-cooldown", "http-error"} {
		t.Run(status, func(t *testing.T) {
			failed := Evidence{Provider: "ippure", IP: "8.8.8.8", CheckedAt: now.UTC().Format(time.RFC3339Nano), Outcome: "unknown", QueryStatus: status}
			r := Score("8.8.8.8", []Evidence{answer, failed}, now)
			if r.State != "residential" || r.ResidentialVotes != 1 {
				t.Fatalf("refusal evicted the answer: %+v", r)
			}
			if len(r.Evidence) != 2 {
				t.Fatalf("the refusal was not kept beside the answer: %+v", r.Evidence)
			}
			// The result is re-read from storage on the next merge, so the
			// answer has to survive a round trip through it.
			if again := Score("8.8.8.8", r.Evidence, now); again.State != "residential" {
				t.Fatalf("answer lost on reload: %+v", again)
			}
		})
	}
}

// A newer answer is still allowed to correct an older one.
func TestNewerAnswerReplacesOlderAnswer(t *testing.T) {
	now := time.Now()
	old := evidence("ippure", 1, now.Add(-2*time.Hour))
	fresh := evidence("ippure", -1, now)
	r := Score("8.8.8.8", []Evidence{old, fresh}, now)
	if r.State != "datacenter" || r.DatacenterVotes != 1 {
		t.Fatalf("correction was ignored: %+v", r)
	}
	if len(r.Evidence) != 1 {
		t.Fatalf("superseded answer retained: %+v", r.Evidence)
	}
}
