package ipcheck

import "time"

const VotingVersion = 8

type Vote struct {
	Provider string   `json:"provider"`
	Points   int      `json:"points"`
	Reason   string   `json:"reason"`
	Evidence Evidence `json:"evidence"`
}

// Result persists network types and the recovery decision. Vote details stay in memory.
type Result struct {
	Version          int            `json:"version"`
	IP               string         `json:"ip"`
	CheckedAt        string         `json:"checkedAt"`
	State            string         `json:"state"`
	Types            []TypeEvidence `json:"types,omitempty"`
	Evidence         []Evidence     `json:"evidence,omitempty"`
	Score            int            `json:"-"`
	ResidentialVotes int            `json:"-"`
	DatacenterVotes  int            `json:"-"`
	UnknownVotes     int            `json:"-"`
	HasDisagreement  bool           `json:"-"`
	Votes            []Vote         `json:"-"`
}

// answered reports whether a row carries an upstream answer. A row that only
// records a refusal, a cooldown or a timeout knows nothing about the address,
// so it may never stand in for one that does. Rows written before QueryStatus
// existed are treated as answers.
func answered(e Evidence) bool {
	return e.QueryStatus == "" || e.QueryStatus == "success"
}

// Score gives each named service at most one vote for this exact IP. Unknown
// responses abstain; tied leaders prefer residential, then business, then hosting.
//
// A failed lookup is a fact about the attempt, not about the address. The vote
// basis is therefore the newest answered row inside the window, even when a
// later attempt failed: picking the newest row of any kind let one timeout
// erase a residential verdict, and because this Result is what gets persisted,
// erase it permanently. The newest attempt still travels with the result so the
// failure stays visible without displacing the answer.
func Score(ip string, evidence []Evidence, now time.Time) Result {
	r := Result{Version: VotingVersion, IP: ip, CheckedAt: now.UTC().Format(time.RFC3339), State: "unknown"}
	providers := []string{"ippure", "ipinfo", "ip-api"}
	for _, optional := range []string{"ipregistry", "abuseipdb", "ipapi", "ip2location"} {
		for _, e := range evidence {
			if e.Provider == optional {
				providers = append(providers, optional)
				break
			}
		}
	}
	kept := map[string]bool{}
	for _, provider := range providers {
		selected := Evidence{Provider: provider, IP: ip, Outcome: "unknown"}
		latest := Evidence{}
		var answeredAt, latestAt time.Time
		for _, e := range evidence {
			if e.Provider != provider || e.IP != ip {
				continue
			}
			stamp, err := time.Parse(time.RFC3339Nano, e.CheckedAt)
			if err != nil || stamp.After(now.Add(5*time.Minute)) || now.Sub(stamp) > 24*time.Hour {
				continue
			}
			if latest.CheckedAt == "" || stamp.After(latestAt) {
				latest, latestAt = e, stamp
			}
			if !answered(e) {
				continue
			}
			if selected.CheckedAt == "" || stamp.After(answeredAt) {
				selected, answeredAt = e, stamp
			}
		}
		v := Vote{Provider: provider, Evidence: selected, Reason: "unknown-or-unavailable"}
		for _, e := range []Evidence{selected, latest} {
			key := e.Provider + "|" + e.CheckedAt
			if e.CheckedAt == "" || kept[key] {
				continue
			}
			kept[key] = true
			r.Evidence = append(r.Evidence, e)
		}
		types := selected.networkTypes()
		if types.UsageType != "" || types.CompanyType != "" {
			r.Types = append(r.Types, types)
		}
		if selected.Outcome != "unknown" {
			switch {
			case selected.Hosting != nil && *selected.Hosting, selected.Outcome == "provider-conflict", selected.Outcome == "datacenter":
				v.Points, v.Reason = -1, "hosting"
			case selected.Outcome == "supports-residential":
				v.Points, v.Reason = 1, "explicit-isp"
			}
		}
		r.Score += v.Points
		switch v.Points {
		case 1:
			r.ResidentialVotes++
		case -1:
			r.DatacenterVotes++
		default:
			r.UnknownVotes++
		}
		r.Votes = append(r.Votes, v)
	}
	r.HasDisagreement = r.ResidentialVotes > 0 && r.DatacenterVotes > 0
	r.Reclassify()
	return r
}

// Reclassify applies the current policy to stored types without changing source
// evidence, query timestamps or freshness. Old records need no network refresh.
func (r *Result) Reclassify() {
	switch r.summaryType() {
	case "住宅IP":
		r.State = "residential"
	case "机房IP":
		r.State = "datacenter"
	default:
		r.State = "unknown"
	}
}

func IPPureEvidence(ip string, attributes []string, checkedAt string) Evidence {
	e := Evidence{Provider: "ippure", IP: ip, CheckedAt: checkedAt, Outcome: "unknown"}
	for _, a := range attributes {
		for kind, label := range typeLabels {
			if a == label {
				e.ASNType = kind
				e.classifyUsage()
				return e
			}
		}
	}
	return e
}

// Attributes summarizes explicit usage types by independent provider. Detailed
// source disagreements remain in Types; unrelated attributes are preserved.
func (r Result) Attributes(original []string) []string {
	attributes := []string{}
	seen := map[string]bool{}
	appendLabel := func(label string) {
		if label != "" && !seen[label] {
			attributes = append(attributes, label)
			seen[label] = true
		}
	}
	for _, a := range original {
		if a == "性质争议" || a == "性质未知" || a == "类型未知" || a == "类型待确认" {
			continue
		}
		isType := false
		for _, label := range typeLabels {
			if a == label {
				isType = true
				break
			}
		}
		if !isType {
			appendLabel(a)
		}
	}
	appendLabel(r.summaryType())
	return attributes
}

// Each provider has one type vote: usage type first, company type as fallback.
// Failed lookups abstain. Only tied leaders qualify for the residential/business
// preference; all remaining ties resolve to hosting.
func (r Result) summaryType() string {
	counts := map[string]int{}
	providers := map[string]bool{}
	for _, t := range r.Types {
		if providers[t.Provider] {
			continue
		}
		providers[t.Provider] = true
		label := t.UsageType
		if label == "" {
			label = t.CompanyType
		}
		if label != "" {
			counts[label]++
		}
	}
	// Older in-memory callers may carry explicit outcomes without type fields.
	if len(counts) == 0 {
		for _, vote := range r.Votes {
			if vote.Points == 1 {
				counts["住宅IP"]++
			}
			if vote.Points == -1 {
				counts["机房IP"]++
			}
		}
	}
	winner, best, tied := "", 0, false
	for label, count := range counts {
		if count > best {
			winner, best, tied = label, count, false
		} else if count == best {
			tied = true
		}
	}
	if best == 0 {
		return "类型未知"
	}
	if tied {
		if counts["住宅IP"] == best {
			return "住宅IP"
		}
		if counts["商业宽带"] == best {
			return "商业宽带"
		}
		return "机房IP"
	}
	return winner
}
