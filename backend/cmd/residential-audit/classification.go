package main

import (
	"context"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/store"
	"os"
	"strings"
	"time"
)

type crossCheck = ipcheck.Evidence

// External providers share a host-side cache and respect service quotas.
func crossCheckReport(ctx context.Context, r *report) {
	sources := ipcheck.NewClient(os.Getenv("IPINFO_TOKEN"), os.Getenv("IP_API_PRO_KEY"), os.Getenv("IPREGISTRY_API_KEY"), os.Getenv("ABUSEIPDB_API_KEY"))
	sources.Providers = ipcheck.ProvidersFromEnv()
	for i := range r.Records {
		v := &r.Records[i]
		v.Residential = false
		v.RecoveryEligible = false
		v.IPPureResidential = false
		v.Classification = "unknown"
		if !v.Reachable || v.VerifiedIP == "" {
			continue
		}
		if v.CrossCheck != nil {
			v.AdditionalEvidence = append(v.AdditionalEvidence, *v.CrossCheck)
		}
		v.CrossCheck = nil
		v.AdditionalEvidence = append(v.AdditionalEvidence, sources.Lookup(ctx, v.VerifiedIP)...)
		classifyRecord(v, time.Now())
	}
	recount(r)
}

func hasAttribute(attributes []string, target string) bool {
	for _, a := range attributes {
		if a == target {
			return true
		}
	}
	return false
}

// Classification is the same provider policy used by the collector and recovery.
func classifyRecord(v *record, now time.Time) {
	v.Residential = false
	v.IPPureResidential = false
	v.EvidenceConflict = false
	v.Classification = "unknown"
	v.ClassificationReason = "insufficient-evidence"
	if !v.Reachable || v.VerifiedIP == "" {
		return
	}
	evidence := append([]crossCheck{}, v.AdditionalEvidence...)
	if v.CrossCheck != nil {
		evidence = append(evidence, *v.CrossCheck)
	}
	if v.Verdict != nil && v.Verdict.IP == v.VerifiedIP {
		v.IPPureResidential = hasAttribute(v.Verdict.Attributes, "住宅IP")
		evidence = append(evidence, ipcheck.IPPureEvidence(v.VerifiedIP, v.Verdict.Attributes, v.ProbedAt))
	}
	result := ipcheck.Score(v.VerifiedIP, evidence, now)
	v.Classification = result.State
	v.ClassificationReason = result.State
	v.EvidenceConflict = result.HasDisagreement
	checked, err := time.Parse(time.RFC3339Nano, v.ProbedAt)
	v.Residential = result.State == "residential" && err == nil && !checked.After(now.Add(5*time.Minute)) && now.Sub(checked) <= 24*time.Hour
}

func recount(r *report) {
	r.Residential, r.RetiredResidential, r.IPPureResidential = 0, 0, 0
	ips := map[string]bool{}
	for _, v := range r.Records {
		if v.IPPureResidential {
			r.IPPureResidential++
		}
		if !v.Residential {
			continue
		}
		r.Residential++
		ips[v.VerifiedIP] = true
		if v.Retired {
			r.RetiredResidential++
		}
	}
	r.UniqueResidentialIPs = len(ips)
}

// This flag describes evidence readiness; the auditor never changes retirements.
// A reclassification of the same probe cannot become a second recovery round.
func markRecoveryEligibility(r *report, previous *report) {
	byID := map[string]record{}
	if previous != nil {
		for _, old := range previous.Records {
			classifyRecord(&old, time.Now())
			byID[old.ID] = old
		}
	}
	for i := range r.Records {
		v := &r.Records[i]
		v.RecoveryEligible = false
		old, ok := byID[v.ID]
		if !ok || !recoverableReason(v.Reason) || old.Reason != v.Reason || !v.Retired || !old.Retired || v.RetiredAt == "" || old.RetiredAt != v.RetiredAt || !v.Residential || !old.Residential || v.VerifiedIP != old.VerifiedIP {
			continue
		}
		before, err1 := time.Parse(time.RFC3339, old.ProbedAt)
		after, err2 := time.Parse(time.RFC3339, v.ProbedAt)
		retired, err3 := time.Parse(time.RFC3339, v.RetiredAt)
		v.RecoveryEligible = err1 == nil && err2 == nil && err3 == nil && !before.Before(retired) && after.Sub(before) >= 30*time.Minute
	}
}

// Preserve timestamped evidence for the same exit. Score selects the latest
// response per provider, including unknown responses, without duplicating votes.
func mergeClassificationEvidence(r *report, previous *report) {
	byIP := map[string][]crossCheck{}
	if previous != nil {
		for _, v := range previous.Records {
			if c := v.CrossCheck; c != nil && c.IP == v.VerifiedIP {
				byIP[c.IP] = append(byIP[c.IP], *c)
			}
			for _, c := range v.AdditionalEvidence {
				if c.IP == v.VerifiedIP {
					byIP[c.IP] = append(byIP[c.IP], c)
				}
			}
		}
	}

	for i := range r.Records {
		v := &r.Records[i]
		all := append(v.AdditionalEvidence, byIP[v.VerifiedIP]...)
		v.AdditionalEvidence = nil
		v.EvidenceConflict = false
		seen := map[string]bool{}
		for _, c := range all {
			if c.IP != v.VerifiedIP {
				continue
			}
			key := c.Provider + ":" + c.CheckedAt + ":" + c.Outcome
			if seen[key] {
				continue
			}
			seen[key] = true
			v.AdditionalEvidence = append(v.AdditionalEvidence, c)
			if c.Outcome == "provider-conflict" {
				v.EvidenceConflict = true
			}
		}
		classifyRecord(v, time.Now())
	}
	recount(r)
}

func parseIPAPICrossCheck(raw []byte, ip, checkedAt string) *crossCheck {
	e := ipcheck.ParseIPAPI(raw, ip, checkedAt)
	return &e
}
func parseCrossCheck(raw []byte, ip, checkedAt string) *crossCheck {
	e := ipcheck.ParseIPinfo(raw, ip, checkedAt)
	return &e
}

func recoverableReason(reason string) bool {
	return reason == store.RetireRestoreReason || strings.HasPrefix(reason, "IPPure:")
}
