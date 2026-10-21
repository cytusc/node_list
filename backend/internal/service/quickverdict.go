package service

import (
	"context"
	"time"

	"github.com/cytusc/node_list/backend/internal/ipcheck"
)

// residentialQuickVerdict re-scores the exit IP from the ip-api batch evidence
// subset only (PrimeIPAPIBatch seeds the shared cache). Residential is accepted
// as a strong signal only when the fresh weighted result is residential without
// disagreement; weak or datacenter outcomes fall back to the full attribute
// pipeline, and a misjudgment is absorbed by the attributes() backfill gate
// once the ippure evidence refreshes.
func (r *Runner) residentialQuickVerdict(ctx context.Context, ip string) bool {
	if r.Classification == nil {
		return false
	}
	if err := r.Classification.PrimeIPAPIBatch(ctx, []string{ip}); err != nil {
		return false
	}
	verdict := ipcheck.Score(ip, r.Classification.BatchEvidence([]string{ip}), time.Now())
	return verdict.State == "residential" && !verdict.HasDisagreement
}
