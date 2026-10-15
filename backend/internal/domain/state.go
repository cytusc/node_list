package domain

import (
	"time"

	"github.com/cytusc/node_list/backend/internal/ipcheck"
)

// These observations belong to a connection, not a subscription batch.
type ConnectivityState struct {
	CheckedAt     string `json:"checkedAt"`
	Reachable     bool   `json:"reachable"`
	ConfirmedDead bool   `json:"confirmedDead,omitempty"`
	Error         string `json:"error,omitempty"`
}

type ExitState struct {
	IP            string `json:"ip,omitempty"`
	CheckedAt     string `json:"checkedAt"`
	Version       int64  `json:"version"`
	Source        string `json:"source,omitempty"`
	Country       string `json:"country,omitempty"`
	AddressFamily int    `json:"addressFamily,omitempty"`
	Verified      bool   `json:"verified"`
	Error         string `json:"error,omitempty"`
}

type LifecycleState struct {
	Status    string `json:"status"`
	RetiredAt string `json:"retiredAt,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type NodeState struct {
	Connectivity *ConnectivityState
	Exit         *ExitState
	Attributes   *IPPureResult
	Lifecycle    *LifecycleState
}

func (s NodeState) Apply(n *Node) {
	n.Connectivity, n.Exit, n.Lifecycle = s.Connectivity, s.Exit, s.Lifecycle
	if s.Exit == nil {
		return
	}
	// An observed new exit must never inherit the previous exit's properties.
	r := IPPureResult{Status: "error", IP: s.Exit.IP, CheckedAt: s.Exit.CheckedAt,
		ExitIPSource: s.Exit.Source, ExitIPMode: "proxy-echo", ExitIPVersion: ExitIPVersion,
		AddressFamily: s.Exit.AddressFamily, Country: s.Exit.Country, FailureStage: "exit-ip", Error: s.Exit.Error}
	if s.Exit.IP != "" {
		r.FailureStage, r.Error = "ippure-query", "IP 属性待查询"
		if s.Attributes != nil && s.Attributes.IP == s.Exit.IP {
			r = *s.Attributes
			r.ExitIPSource, r.ExitIPMode, r.ExitIPVersion = s.Exit.Source, "proxy-echo", ExitIPVersion
			r.AddressFamily = s.Exit.AddressFamily
			if r.Country == "" {
				r.Country = s.Exit.Country
			}
		}
		if s.Exit.AddressFamily == 6 {
			r.Status, r.FailureStage, r.Error = "unsupported", "unsupported-ipv6", "仅 IPv6，暂无评分"
		}
	}
	n.IPPure = &r
	ApplyExitCountry(n)
}

func FreshResidential(r *IPPureResult, ip string, now time.Time) bool {
	if r == nil || r.Classification == nil || r.IP != ip || r.Classification.IP != ip || r.Classification.Version != ipcheck.VotingVersion {
		return false
	}
	c := r.Classification
	stamp, err := time.Parse(time.RFC3339Nano, c.CheckedAt)
	if err != nil || stamp.After(now.Add(time.Minute)) || now.Sub(stamp) > 24*time.Hour {
		return false
	}
	// New records retain provider timestamps; re-evaluate without refreshing them.
	if len(c.Evidence) > 0 {
		return ipcheck.Score(ip, c.Evidence, now).State == "residential"
	}
	return c.State == "residential"
}

func Preferred(n Node, now time.Time) bool {
	if n.Connectivity == nil || !n.Connectivity.Reachable || n.Connectivity.Error != "" || n.Exit == nil || n.Exit.Error != "" || n.Exit.IP == "" {
		return false
	}
	if n.Lifecycle != nil && n.Lifecycle.Status != "active" {
		return false
	}
	for _, raw := range []string{n.Connectivity.CheckedAt, n.Exit.CheckedAt} {
		stamp, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || stamp.After(now.Add(time.Minute)) || now.Sub(stamp) > 10*time.Minute {
			return false
		}
	}
	return FreshResidential(n.IPPure, n.Exit.IP, now)
}
