// Package domain contains shared node JSON models and legacy share-link decoding.
package domain

import (
	"encoding/json"

	"github.com/cytusc/node_list/backend/internal/ipcheck"
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
	Classification *ipcheck.Result `json:"classification,omitempty"`
	shape          jsonShape

	CheckedAt string `json:"checkedAt"`
	Attempts  int    `json:"attempts"`

	Status             string   `json:"status,omitempty"`
	IP                 string   `json:"ip,omitempty"`
	ASN                string   `json:"asn,omitempty"`
	Organization       string   `json:"asnOrganization,omitempty"`
	Source             string   `json:"source,omitempty"`
	Attributes         []string `json:"attributes,omitempty"`
	HumanPercent       *float64 `json:"humanPercent,omitempty"`
	BotPercent         *float64 `json:"botPercent,omitempty"`
	TrafficRatioStatus string   `json:"trafficRatioStatus,omitempty"`
	TrafficRatioError  string   `json:"trafficRatioError,omitempty"`
	// RiskQueryStatus is the outcome of the last attempt; RiskQueryError is its
	// reason. The verdict below keeps its own timestamps, so a refusal never
	// makes an older answer look freshly checked: RiskQueryCheckedAt is when a
	// risk answer was actually obtained, RiskQueryAttemptAt is when the last
	// attempt — successful or not — finished.
	RiskQueryStatus       string  `json:"riskQueryStatus,omitempty"`
	RiskQueryError        string  `json:"riskQueryError,omitempty"`
	RiskQueryCheckedAt    string  `json:"riskQueryCheckedAt,omitempty"`
	RiskQueryAttemptAt    string  `json:"riskQueryAttemptAt,omitempty"`
	TrafficRatioCheckedAt string  `json:"trafficRatioCheckedAt,omitempty"`
	Score                 float64 `json:"score,omitempty"`
	Label                 string  `json:"label,omitempty"`

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
	shape jsonShape

	Name     string `json:"name,omitempty"`
	BaseName string `json:"baseName,omitempty"`
	Type     string `json:"type,omitempty"`
	Server   string `json:"server,omitempty"`
	Port     string `json:"port,omitempty"`
	SNI      string `json:"sni,omitempty"`
	// Programmatically created nodes render TLS/UDP/Speed by default. Decoded
	// nodes additionally preserve the source's absent/zero/null distinctions.
	TLS            bool     `json:"tls"`
	Reality        bool     `json:"reality,omitempty"`
	UDP            bool     `json:"udp"`
	Network        string   `json:"network,omitempty"`
	Speed          float64  `json:"speed"`
	Country        string   `json:"country,omitempty"`
	ClaimedCountry string   `json:"claimedCountry,omitempty"`
	IP             string   `json:"ip,omitempty"`
	IPRisk         string   `json:"ipRisk,omitempty"`
	Media          []Media  `json:"media,omitempty"`
	SubTag         string   `json:"subTag,omitempty"`
	Details        []Detail `json:"details,omitempty"`
	RawLink        string   `json:"rawLink,omitempty"`
	// GeneratedLink is converted from config only when no upstream URI exists.
	GeneratedLink string      `json:"generatedLink,omitempty"`
	RawConfig     ProxyConfig `json:"rawConfig,omitempty"`

	// Enrichment and presentation state. Never part of a snapshot fingerprint.
	IPPure       *IPPureResult      `json:"ippure,omitempty"`
	Connectivity *ConnectivityState `json:"connectivity,omitempty"`
	Exit         *ExitState         `json:"exit,omitempty"`
	Lifecycle    *LifecycleState    `json:"lifecycle,omitempty"`

	// Runtime coordinates. The aggregate view publishes the batch each node
	// came from, so those three are pointers and stay absent from a
	// single-batch view — the JavaScript store only attaches them while folding.
	SnapshotID *int   `json:"snapshotId,omitempty"`
	Index      int    `json:"-"`
	AddedAt    string `json:"addedAt,omitempty"`
	CheckedAt  string `json:"checkedAt,omitempty"`
	SpeedTest  *bool  `json:"speedTest,omitempty"`
	MediaCheck *bool  `json:"mediaCheck,omitempty"`
}

// ProxyConfig is the upstream proxy configuration.
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
	shape, err := readJSONShape(data, nodeJSONFields)
	if err != nil {
		return err
	}
	*n = Node(decoded.plain)
	n.shape = shape
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

// MarshalJSON preserves explicitly supplied zero values without adding absent
// optional properties. Typed fields still supply any subsequently updated value.
func (n Node) MarshalJSON() ([]byte, error) {
	type plain Node
	return marshalJSONShape(plain(n), n.shape, nodeJSONFields)
}

// ClearCredentials removes both the values and their JSON presence. Public
func (r *IPPureResult) UnmarshalJSON(data []byte) error {
	type plain IPPureResult
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	shape, err := readJSONShape(data, ippureJSONFields)
	if err != nil {
		return err
	}
	*r = IPPureResult(decoded)
	r.shape = shape
	if r.Classification != nil && r.Classification.IP == r.IP && len(r.Classification.Types) > 0 {
		r.Classification.Reclassify()
		r.Attributes = r.Classification.Attributes(r.Attributes)
	}
	return nil
}

func (r IPPureResult) MarshalJSON() ([]byte, error) {
	type plain IPPureResult
	shape := r.shape
	if !shape.decoded && r.Status == "success" {
		// A newly collected successful verdict always has a score, including
		// the valid value 0. Failed/unsupported results must not gain one.
		shape.decoded = true
		for i, f := range ippureJSONFields {
			if !f.omitEmpty || f.name == "score" {
				shape.present |= uint64(1) << i
			}
		}
	}
	return marshalJSONShape(plain(r), shape, ippureJSONFields)
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
}
