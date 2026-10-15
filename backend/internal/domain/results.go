package domain

import (
	"sort"
	"strings"
	"unicode"
)

// ExitIPVersion and ExitIPProbeVersion mark the shape of a stored enrichment.
// Bumping either re-queues results collected under the previous shape without
// discarding the data that is still valid.
const (
	ExitIPVersion      = 3
	ExitIPProbeVersion = 1
)

// HasVerifiedExitIP reports whether an enrichment carries an exit address that
// was observed through the current echo-probe contract.
func HasVerifiedExitIP(r *IPPureResult) bool {
	if r == nil {
		return false
	}
	return r.ExitIPSource == "ip.sb" ||
		(r.ExitIPMode == "proxy-echo" && r.ExitIPVersion == ExitIPVersion)
}

// ResultSortKeys are the columns the results view can sort by. They also drive
// the persisted rank columns, so the order is part of the schema.
var ResultSortKeys = []string{
	"country", "name", "type", "speed", "asnOrganization",
	"ipSource", "ipAttributes", "ippure", "trafficRatio",
}

// SortColumn maps a sort key to its persisted rank column.
func SortColumn(key string) string {
	return "rank_" + strings.ToLower(key)
}

// SearchValues are the lower-cased strings a free-text query matches against.
func SearchValues(n Node) []string {
	raw := []string{n.Name, n.BaseName, n.Server, n.SNI, n.Country, CountryName(n.Country)}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if v == "" {
			continue
		}
		out = append(out, strings.ToLower(v))
	}
	return out
}

// SortValue extracts the ranking value for one sort key. A nil result means the
// node sorts last, matching the JavaScript comparator's null handling.
func SortValue(n Node, key string) any {
	switch key {
	case "name":
		base := n.BaseName
		if base == "" {
			base = n.Name
		}
		trimmed := strings.TrimSpace(stripRegionalIndicators(base))
		if trimmed == "" {
			if n.Name == "" {
				return nil
			}
			return n.Name
		}
		return trimmed
	case "country", "type":
		if v := fieldString(n, key); v != "" {
			return v
		}
		return nil
	case "speed":
		return n.Speed
	}
	if key == "ipAttributes" && n.IPPure != nil && n.IPPure.Classification != nil {
		if len(n.IPPure.Attributes) == 0 {
			return nil
		}
		return strings.Join(n.IPPure.Attributes, " · ")
	}
	if n.IPPure == nil || n.IPPure.Status != "success" {
		return nil
	}
	switch key {
	case "asnOrganization":
		if n.IPPure.Organization == "" {
			return nil
		}
		return n.IPPure.Organization
	case "ipSource":
		if n.IPPure.Source == "" {
			return nil
		}
		return n.IPPure.Source
	case "ipAttributes":
		if len(n.IPPure.Attributes) == 0 {
			return nil
		}
		return strings.Join(n.IPPure.Attributes, " · ")
	case "ippure":
		return n.IPPure.Score
	case "trafficRatio":
		if n.IPPure.HumanPercent == nil {
			return nil
		}
		return *n.IPPure.HumanPercent
	}
	return nil
}

func fieldString(n Node, key string) string {
	switch key {
	case "country":
		return n.Country
	case "type":
		return n.Type
	case "network":
		return n.Network
	}
	return ""
}

// stripRegionalIndicators removes flag emoji so country flags in a display name
// do not dominate the alphabetical ordering.
func stripRegionalIndicators(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x1F1E6 && r <= 0x1F1FF {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Facet is one filter option with its visible count.
type Facet struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Summary carries the headline numbers for the results view.
type Summary struct {
	Countries int     `json:"countries"`
	MaxSpeed  float64 `json:"maxSpeed"`
	Median    float64 `json:"median"`
}

// Metadata is the persisted projection header: everything except the page of
// nodes, which is queried separately.
type Metadata struct {
	CheckedAt  string  `json:"checkedAt,omitempty"`
	BatchCount int     `json:"batchCount,omitempty"`
	SpeedTest  bool    `json:"speedTest"`
	MediaCheck bool    `json:"mediaCheck"`
	TotalNodes int     `json:"totalNodes"`
	Summary    Summary `json:"summary"`
	Facets     Facets  `json:"facets"`
}

// Facets are the filter option counts for the visible node set.
type Facets struct {
	Country      []Facet `json:"country"`
	Type         []Facet `json:"type"`
	Network      []Facet `json:"network"`
	IPSource     []Facet `json:"ipSource"`
	IPAttributes []Facet `json:"ipAttributes"`
	Features     []Facet `json:"features"`
	Media        []Facet `json:"media"`
}

// ResultsMetadata computes the persisted header for a node set. Filter counts
// always describe the same set the list shows, so counts never disagree with
// the page the user is looking at.
func ResultsMetadata(s Snapshot) Metadata {
	nodes := s.Nodes
	counts := func(key string) []Facet {
		values := make(map[string]int, 32)
		order := make([]string, 0, 32)
		for _, n := range nodes {
			value := fieldString(n, key)
			if value == "" {
				value = "unknown"
			}
			if _, ok := values[value]; !ok {
				order = append(order, value)
			}
			values[value]++
		}
		out := make([]Facet, 0, len(order))
		for _, value := range order {
			label := strings.ToUpper(value)
			if key == "country" {
				label = CountryName(value)
			}
			out = append(out, Facet{Value: value, Label: label, Count: values[value]})
		}
		sortFacets(out)
		return out
	}
	ipCounts := func(key string) []Facet {
		values := make(map[string]int, 16)
		order := make([]string, 0, 16)
		for _, n := range nodes {
			if n.IPPure == nil || (n.IPPure.Status != "success" && (key != "attributes" || n.IPPure.Classification == nil)) {
				continue
			}
			items := n.IPPure.Attributes
			if key == "source" {
				items = []string{n.IPPure.Source}
			}
			seen := make(map[string]struct{}, len(items))
			for _, value := range items {
				if value == "" {
					continue
				}
				if _, ok := seen[value]; ok {
					continue
				}
				seen[value] = struct{}{}
				if _, ok := values[value]; !ok {
					order = append(order, value)
				}
				values[value]++
			}
		}
		out := make([]Facet, 0, len(order))
		for _, value := range order {
			out = append(out, Facet{Value: value, Label: value, Count: values[value]})
		}
		sortFacets(out)
		return out
	}
	speeds := make([]float64, 0, len(nodes))
	for _, n := range nodes {
		if n.Speed > 0 {
			speeds = append(speeds, n.Speed)
		}
	}
	sort.Float64s(speeds)
	summary := Summary{}
	if len(speeds) > 0 {
		middle := len(speeds) / 2
		summary.MaxSpeed = speeds[len(speeds)-1]
		if len(speeds)%2 == 1 {
			summary.Median = speeds[middle]
		} else {
			summary.Median = (speeds[middle-1] + speeds[middle]) / 2
		}
	}
	countries := make(map[string]struct{}, 32)
	for _, n := range nodes {
		if n.Country != "" {
			countries[n.Country] = struct{}{}
		}
	}
	summary.Countries = len(countries)

	return Metadata{
		CheckedAt:  s.CheckedAt,
		BatchCount: s.BatchCount,
		SpeedTest:  s.SpeedTest,
		MediaCheck: s.MediaCheck,
		TotalNodes: len(nodes),
		Summary:    summary,
		Facets: Facets{
			Country:      counts("country"),
			Type:         counts("type"),
			Network:      counts("network"),
			IPSource:     ipCounts("source"),
			IPAttributes: ipCounts("attributes"),
			Features: []Facet{
				featureCount(nodes, "tls", "TLS"),
				featureCount(nodes, "reality", "Reality"),
				featureCount(nodes, "udp", "UDP"),
			},
			Media: mediaFacets(nodes),
		},
	}
}

func featureCount(nodes []Node, key, label string) Facet {
	count := 0
	for _, n := range nodes {
		switch key {
		case "tls":
			if n.TLS {
				count++
			}
		case "reality":
			if n.Reality {
				count++
			}
		case "udp":
			if n.UDP {
				count++
			}
		}
	}
	return Facet{Value: key, Label: label, Count: count}
}

func mediaFacets(nodes []Node) []Facet {
	platforms := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	for _, n := range nodes {
		for _, p := range MediaPlatforms(n) {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			platforms = append(platforms, p)
		}
	}
	out := make([]Facet, 0, len(platforms))
	for _, p := range platforms {
		count := 0
		for _, n := range nodes {
			if HasMedia(n, p) {
				count++
			}
		}
		out = append(out, Facet{Value: p, Label: p, Count: count})
	}
	return out
}

// sortFacets mirrors the JavaScript comparator exactly: counts descend and
// nothing else. Array.prototype.sort is stable in V8 and the input Map keeps
// first-seen order, so ties stay in first-appearance order — any tie-break here
// would silently reorder facets the front end already renders.
func sortFacets(out []Facet) {
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Count > out[j].Count
	})
}

// ResultsSort resolves the effective sort for a request. Sorting by speed is
// meaningless for a batch that never ran a speed test, so it degrades to name.
func ResultsSort(m Metadata, key, direction string) (string, string) {
	switch {
	case key == "speed" && !m.SpeedTest:
		return "name", "asc"
	case key == "":
		if m.SpeedTest {
			return "speed", direction
		}
		return "name", direction
	}
	return key, direction
}

// NormalizeQuery trims and lower-cases the free-text term the same way the
// stored search values were built.
func NormalizeQuery(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}

var _ = unicode.IsLetter
