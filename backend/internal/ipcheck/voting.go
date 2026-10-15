package ipcheck

import "time"

const VotingVersion = 2

type Vote struct {
	Provider string   `json:"provider"`
	Points   int      `json:"points"`
	Reason   string   `json:"reason"`
	Evidence Evidence `json:"evidence"`
}

// Result keeps voting details in memory only; JSON stores the final classification.
type Result struct {
	Version          int    `json:"version"`
	IP               string `json:"ip"`
	CheckedAt        string `json:"checkedAt"`
	State            string `json:"state"`
	Score            int    `json:"-"`
	ResidentialVotes int    `json:"-"`
	DatacenterVotes  int    `json:"-"`
	UnknownVotes     int    `json:"-"`
	HasDisagreement  bool   `json:"-"`
	Votes            []Vote `json:"-"`
}

// Score gives each named service at most one vote for this exact IP. Unknown
// responses abstain. Two agreeing sources are required, even if the third fails.
func Score(ip string, evidence []Evidence, now time.Time) Result {
	r := Result{Version: VotingVersion, IP: ip, CheckedAt: now.UTC().Format(time.RFC3339), State: "unknown"}
	for _, provider := range []string{"ippure", "ipinfo", "ip-api"} {
		selected := Evidence{Provider: provider, IP: ip, Outcome: "unknown"}
		var newest time.Time
		for _, e := range evidence {
			if e.Provider != provider || e.IP != ip {
				continue
			}
			stamp, err := time.Parse(time.RFC3339Nano, e.CheckedAt)
			if err != nil || stamp.After(now.Add(5*time.Minute)) || now.Sub(stamp) > 24*time.Hour || stamp.Before(newest) {
				continue
			}
			newest, selected = stamp, e
		}
		v := Vote{Provider: provider, Evidence: selected, Reason: "unknown-or-unavailable"}
		if selected.Outcome != "unknown" {
			switch {
			case selected.Hosting != nil && *selected.Hosting, selected.Outcome == "provider-conflict", selected.Outcome == "datacenter":
				v.Points, v.Reason = -1, "hosting"
			case selected.Outcome == "supports-residential", selected.Outcome == "non-hosting":
				v.Points, v.Reason = 1, "residential-or-non-hosting"
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
	switch {
	case r.ResidentialVotes >= 2:
		r.State = "residential"
	case r.DatacenterVotes >= 2:
		r.State = "datacenter"
	case r.HasDisagreement:
		r.State = "disputed"
	}
	return r
}

func IPPureEvidence(ip string, attributes []string, checkedAt string) Evidence {
	e := Evidence{Provider: "ippure", IP: ip, CheckedAt: checkedAt, Outcome: "unknown"}
	for _, a := range attributes {
		if a == "机房IP" {
			e.Outcome = "datacenter"
			return e
		}
		if a == "住宅IP" {
			e.Outcome = "supports-residential"
		}
	}
	return e
}

// Attributes keeps unrelated labels while making filtering agree with the vote.
func (r Result) Attributes(original []string) []string {
	attributes := []string{}
	for _, a := range original {
		if a != "住宅IP" && a != "机房IP" && a != "性质争议" && a != "性质未知" {
			attributes = append(attributes, a)
		}
	}
	switch r.State {
	case "residential":
		attributes = append(attributes, "住宅IP")
	case "datacenter":
		attributes = append(attributes, "机房IP")
	case "disputed":
		attributes = append(attributes, "性质争议")
	default:
		attributes = append(attributes, "性质未知")
	}
	return attributes
}
