// Package domain holds the node model, the stable connection identities and the
// shared result metadata. It mirrors the semantics of the previous JavaScript
// server exactly: identities are persisted across deployments, so any change in
// hashing or ordering would orphan the existing enrichment and retirement rows.
package domain

import (
	"encoding/json"
	"sort"
	"strings"
)

// Detail is one upstream key/value summary row.
type Detail struct {
	K string `json:"k"`
	V string `json:"v"`
}

// Media is one media-unlock check result. Upstream sources disagree on the key
// casing, so both spellings are accepted and the original bytes are preserved.
type Media struct {
	raw      json.RawMessage
	Platform string
	Tag      string
}

// UnmarshalJSON accepts platform/Platform and tag/Tag while keeping the original
// payload so snapshots round-trip byte for byte.
func (m *Media) UnmarshalJSON(data []byte) error {
	m.raw = append(json.RawMessage(nil), data...)
	var lower struct {
		Platform string `json:"platform"`
		Tag      string `json:"tag"`
	}
	if err := json.Unmarshal(data, &lower); err == nil {
		m.Platform, m.Tag = lower.Platform, lower.Tag
	}
	var upper struct {
		Platform string `json:"Platform"`
		Tag      string `json:"Tag"`
	}
	if err := json.Unmarshal(data, &upper); err == nil {
		if m.Platform == "" {
			m.Platform = upper.Platform
		}
		if m.Tag == "" {
			m.Tag = upper.Tag
		}
	}
	return nil
}

// MarshalJSON re-emits the original bytes; snapshots must not be normalised.
func (m Media) MarshalJSON() ([]byte, error) {
	if len(m.raw) > 0 {
		return m.raw, nil
	}
	return json.Marshal(struct {
		Platform string `json:"platform"`
		Tag      string `json:"tag"`
	}{m.Platform, m.Tag})
}

// IPPureResult is the enrichment attached to a probed node.
type IPPureResult struct {
	CheckedAt string `json:"checkedAt"`
	Attempts  int    `json:"attempts"`

	Status       string   `json:"status,omitempty"`
	IP           string   `json:"ip,omitempty"`
	ASN          string   `json:"asn,omitempty"`
	Organization string   `json:"asnOrganization,omitempty"`
	Source       string   `json:"source,omitempty"`
	Attributes   []string `json:"attributes,omitempty"`
	HumanPercent *float64 `json:"humanPercent,omitempty"`
	BotPercent   *float64 `json:"botPercent,omitempty"`
	Score        float64  `json:"score,omitempty"`
	Label        string   `json:"label,omitempty"`

	Country       string `json:"country,omitempty"`
	AddressFamily int    `json:"addressFamily,omitempty"`
	QueryTarget   string `json:"queryTarget,omitempty"`

	ExitIPSource       string `json:"exitIPSource,omitempty"`
	ExitIPMode         string `json:"exitIPMode,omitempty"`
	ExitIPVersion      int    `json:"exitIPVersion,omitempty"`
	ExitIPProbeVersion int    `json:"exitIPProbeVersion,omitempty"`
	QueryMode          string `json:"queryMode,omitempty"`
	TransportMode      string `json:"transportMode,omitempty"`
	SignatureVersion   int    `json:"signatureVersion,omitempty"`

	FailureStage          string          `json:"failureStage,omitempty"`
	ShareLinkVersion      int             `json:"shareLinkVersion,omitempty"`
	Error                 string          `json:"error,omitempty"`
	ExitIPAttempts        []ExitIPAttempt `json:"exitIPAttempts,omitempty"`
	OrganizationAttempts  int             `json:"organizationAttempts,omitempty"`
	OrganizationCheckedAt string          `json:"organizationCheckedAt,omitempty"`
	GitHubReachable       bool            `json:"githubReachable,omitempty"`
}

// ExitIPAttempt records one echo-service dial for diagnostics.
type ExitIPAttempt struct {
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint"`
	Round     int    `json:"round"`
	ElapsedMs int64  `json:"elapsedMs"`
	Outcome   string `json:"outcome"`
	Code      string `json:"code,omitempty"`
}

// Node is one detected connection inside a snapshot.
type Node struct {
	Name     string `json:"name,omitempty"`
	BaseName string `json:"baseName,omitempty"`
	Type     string `json:"type,omitempty"`
	Server   string `json:"server,omitempty"`
	Port     string `json:"port,omitempty"`
	SNI      string `json:"sni,omitempty"`
	// TLS/UDP/Speed are always rendered, even when false or zero: the results
	// view reads them directly and the stored payload must stay byte-compatible
	// with what the JavaScript service wrote.
	TLS            bool        `json:"tls"`
	Reality        bool        `json:"reality,omitempty"`
	UDP            bool        `json:"udp"`
	Network        string      `json:"network,omitempty"`
	Speed          float64     `json:"speed"`
	Country        string      `json:"country,omitempty"`
	ClaimedCountry string      `json:"claimedCountry,omitempty"`
	IP             string      `json:"ip,omitempty"`
	IPRisk         string      `json:"ipRisk,omitempty"`
	Media          []Media     `json:"media,omitempty"`
	SubTag         string      `json:"subTag,omitempty"`
	Details        []Detail    `json:"details,omitempty"`
	RawLink        string      `json:"rawLink,omitempty"`
	RawConfig      ProxyConfig `json:"rawConfig,omitempty"`

	// Enrichment and presentation state. Never part of a snapshot fingerprint.
	IPPure *IPPureResult `json:"ippure,omitempty"`

	// Runtime coordinates. The aggregate view publishes the batch each node
	// came from, so those three are pointers and stay absent from a
	// single-batch view — the JavaScript store only attaches them while folding.
	SnapshotID *int   `json:"snapshotId,omitempty"`
	Index      int    `json:"-"`
	CheckedAt  string `json:"checkedAt,omitempty"`
	SpeedTest  *bool  `json:"speedTest,omitempty"`
	MediaCheck *bool  `json:"mediaCheck,omitempty"`
}

// Batch reports the originating snapshot id, or 0 outside the aggregate view.
func (n Node) Batch() int {
	if n.SnapshotID == nil {
		return 0
	}
	return *n.SnapshotID
}

// SpeedTested reports whether the originating batch ran a speed test. Outside
// the aggregate view the flag is unknown and reported as false.
func (n Node) SpeedTested() bool { return n.SpeedTest != nil && *n.SpeedTest }

// MediaChecked reports whether the originating batch ran a media check.
func (n Node) MediaChecked() bool { return n.MediaCheck != nil && *n.MediaCheck }

// ProxyConfig is a Mihomo outbound definition. Keeping it as an ordered-free
// map is deliberate: Go marshals map keys sorted, which reproduces the
// canonicalisation the JavaScript identity used.
type ProxyConfig map[string]any

// UnmarshalJSON accepts a port published as either a string or a number.
// subs-check and the Qilan panel disagree, and the port is part of the
// connection identity, so it must normalise to one spelling before hashing.
func (n *Node) UnmarshalJSON(data []byte) error {
	type plain Node
	var decoded struct {
		plain
		Port json.RawMessage `json:"port"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*n = Node(decoded.plain)
	if len(decoded.Port) > 0 && string(decoded.Port) != "null" {
		if decoded.Port[0] == '"' {
			var s string
			if err := json.Unmarshal(decoded.Port, &s); err != nil {
				return err
			}
			n.Port = s
		} else {
			var num json.Number
			if err := json.Unmarshal(decoded.Port, &num); err != nil {
				return err
			}
			n.Port = num.String()
		}
	}
	return nil
}

// Snapshot is one detection batch.
type Snapshot struct {
	ID         int    `json:"-"`
	CheckedAt  string `json:"checkedAt,omitempty"`
	SpeedTest  bool   `json:"speedTest"`
	MediaCheck bool   `json:"mediaCheck"`
	Nodes      []Node `json:"nodes"`

	// SourceWarning is surfaced to the UI when the upstream files were unreadable.
	SourceWarning string `json:"sourceWarning,omitempty"`
	// BatchCount is filled by the aggregate projection.
	BatchCount int `json:"batchCount,omitempty"`
}

// Clone returns a shallow copy with independent node backing storage so callers
// can attach enrichment without mutating a shared cache.
func (s Snapshot) Clone() Snapshot {
	out := s
	out.Nodes = make([]Node, len(s.Nodes))
	copy(out.Nodes, s.Nodes)
	return out
}

// Fields is the archived field whitelist: results files carry extra upstream
// columns that must not become part of a stored snapshot.
var Fields = []string{
	"name", "baseName", "type", "server", "port", "sni", "tls", "reality",
	"udp", "network", "speed", "country", "ip", "ipRisk", "media", "subTag",
	"details", "rawLink", "rawConfig",
}

// MediaPlatform mirrors the upstream accessor that tolerates both key casings.
func MediaPlatform(m Media) string {
	if m.Platform != "" {
		return m.Platform
	}
	return m.Tag
}

func MediaValue(m Media) string {
	if m.Tag != "" {
		return m.Tag
	}
	return m.Platform
}

// MediaPlatforms lists the distinct platforms with at least one successful tag.
func MediaPlatforms(n Node) []string {
	seen := make(map[string]struct{}, len(n.Media))
	out := make([]string, 0, len(n.Media))
	for _, m := range n.Media {
		p := MediaPlatform(m)
		if p == "" || MediaValue(m) == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// HasMedia reports whether the node unlocked the given platform.
func HasMedia(n Node, platform string) bool {
	for _, m := range n.Media {
		if MediaPlatform(m) == platform && MediaValue(m) != "" {
			return true
		}
	}
	return false
}

// ApplyExitCountry replaces the source-claimed region with the verified exit
// country, preserving the original claim for the mismatch badge.
func ApplyExitCountry(n *Node) {
	country := ""
	if n.IPPure != nil {
		country = n.IPPure.Country
	}
	if country == "" || len(country) != 2 || !isAlpha(country) {
		return
	}
	verified := strings.ToUpper(country)
	if n.Country != "" && strings.ToUpper(n.Country) != verified {
		n.ClaimedCountry = n.Country
		n.Country = verified
	} else if n.Country == "" {
		n.Country = verified
	}
}

func isAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
