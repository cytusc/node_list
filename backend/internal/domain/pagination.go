package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Paginate filters and pages immutable historical snapshots in memory.
func Paginate(snapshot Snapshot, q *Query) Snapshot {
	if q == nil {
		return snapshot
	}
	matches := make([]Node, 0, len(snapshot.Nodes))
	for _, n := range snapshot.Nodes {
		if !matchNode(n, q, snapshot) {
			continue
		}
		matches = append(matches, n)
	}
	key, direction := ResultsSort(ResultsMetadata(snapshot), q.Sort, q.Direction)
	preferred := key == "preferred"
	if preferred {
		key = "name"
		direction = "asc"
		if snapshot.SpeedTest {
			key = "speed"
			direction = "desc"
		}
	}
	sign := 1
	if direction == "desc" {
		sign = -1
	}
	keys := make([][]byte, len(matches))
	for i, n := range matches {
		v := SortValue(n, key)
		if v == nil {
			continue
		}
		if _, isNum := v.(float64); isNum {
			keys[i] = []byte(fmt.Sprintf("%v", v))
		} else {
			keys[i] = SortKey(fmt.Sprint(v))
		}
	}
	sortNodes(matches, keys, key, sign, preferred)
	page := q.Page
	if max := (len(matches) + q.Limit - 1) / q.Limit; max < 1 {
		page = 1
	} else if page > max {
		page = max
	}
	start := (page - 1) * q.Limit
	end := start + q.Limit
	if start > len(matches) {
		start = len(matches)
	}
	if end > len(matches) {
		end = len(matches)
	}
	out := snapshot
	out.Nodes = matches[start:end]
	return out
}

func sortNodes(nodes []Node, keys [][]byte, key string, direction int, preferred bool) {
	index := make([]int, len(nodes))
	for i := range index {
		index[i] = i
	}
	priorities := make([]bool, len(nodes))
	now := time.Now()
	if preferred {
		for i, n := range nodes {
			priorities[i] = Preferred(n, now)
		}
	}
	numeric := key == "speed" || key == "ippure" || key == "trafficRatio"
	sort.SliceStable(index, func(a, b int) bool {
		ia, ib := index[a], index[b]
		if preferred && priorities[ia] != priorities[ib] {
			return priorities[ia]
		}
		// Nodes without a value always sort last, matching SQL NULLS LAST.
		if keys[ia] == nil && keys[ib] == nil {
			return ia < ib
		}
		if keys[ia] == nil {
			return false
		}
		if keys[ib] == nil {
			return true
		}
		var cmp int
		if numeric {
			fa, fb := toFloat(nodes[ia], key), toFloat(nodes[ib], key)
			switch {
			case fa < fb:
				cmp = -1
			case fa > fb:
				cmp = 1
			}
		} else {
			cmp = strings.Compare(string(keys[ia]), string(keys[ib]))
		}
		if cmp == 0 {
			return ia < ib
		}
		return direction*cmp < 0
	})
	sorted := make([]Node, len(nodes))
	for i, pos := range index {
		sorted[i] = nodes[pos]
	}
	copy(nodes, sorted)
}

func toFloat(n Node, key string) float64 {
	v := SortValue(n, key)
	f, _ := v.(float64)
	return f
}

// Query mirrors the PostgreSQL query shape for the in-memory fallback.
type Query struct {
	Page         int
	Limit        int
	Search       string
	Countries    []string
	Types        []string
	Networks     []string
	Features     []string
	IPSources    []string
	IPAttributes []string
	Media        []string
	MinSpeed     float64
	Sort         string
	Direction    string
}

func matchNode(n Node, q *Query, snapshot Snapshot) bool {
	if q.Search != "" {
		found := false
		for _, v := range SearchValues(n) {
			if strings.Contains(v, q.Search) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(q.Countries) > 0 && !contains(q.Countries, orUnknown(n.Country)) {
		return false
	}
	if len(q.Types) > 0 && !contains(q.Types, orUnknown(n.Type)) {
		return false
	}
	if len(q.Networks) > 0 && !contains(q.Networks, orUnknown(n.Network)) {
		return false
	}
	for _, feature := range q.Features {
		switch feature {
		case "tls":
			if !n.TLS {
				return false
			}
		case "reality":
			if !n.Reality {
				return false
			}
		case "udp":
			if !n.UDP {
				return false
			}
		}
	}
	if len(q.IPSources) > 0 {
		if n.IPPure == nil || n.IPPure.Status != "success" || !contains(q.IPSources, n.IPPure.Source) {
			return false
		}
	}
	if len(q.IPAttributes) > 0 {
		if n.IPPure == nil || (n.IPPure.Status != "success" && n.IPPure.Classification == nil) {
			return false
		}
		found := false
		for _, value := range n.IPPure.Attributes {
			if contains(q.IPAttributes, value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if snapshot.SpeedTest && (n.Speed) < q.MinSpeed*1024 {
		return false
	}
	if snapshot.MediaCheck {
		for _, platform := range q.Media {
			if !HasMedia(n, platform) {
				return false
			}
		}
	}
	return true
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
