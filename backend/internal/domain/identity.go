package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// CompareValues reproduces the JavaScript result comparator: numbers compare
// numerically, anything else falls back to zh-CN numeric collation.
func CompareValues(left, right any) int {
	lf, lOK := number(left)
	rf, rOK := number(right)
	if lOK && rOK {
		switch {
		case lf < rf:
			return -1
		case lf > rf:
			return 1
		default:
			return 0
		}
	}
	return CompareNumeric(toString(left), toString(right))
}

func number(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// marshalJSON renders a value the way JSON.stringify does. Go's encoder
// escapes "<", ">" and "&" for HTML safety, which JSON.stringify does not, and
// the identity hash is taken over the exact bytes.
func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, _ := marshalJSON(t)
		return string(b)
	}
}

// canonicalJSON renders a decoded JSON value with object keys sorted at every
// level, matching the JavaScript canonical() helper used by the identity hash.
// Go already marshals map[string]any with sorted keys, so this only has to walk
// slices. Numbers keep their decoded float64 form, which round-trips the same
// digits JSON.stringify produced.
func canonicalJSON(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		return marshalJSON(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			b, err := canonicalJSON(item)
			if err != nil {
				return nil, err
			}
			out[i] = json.RawMessage(b)
		}
		return marshalJSON(out)
	default:
		return marshalJSON(t)
	}
}

// canonicalHash is sha256 over the canonical rendering, hex encoded.
func canonicalHash(v any) (string, error) {
	b, err := canonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ShareLinkParseFailures counts share links that could not be parsed into a
// connection identity and fell back to the summary identity (REFACTOR_PLAN
// §6.1). A rising count means protocol coverage regressed; tests pin the
// supported protocols.
var ShareLinkParseFailures atomic.Uint64

// NodeIdentity is the summary-level identity: it describes the endpoint as the
// upstream summary reported it, before any credential is known.
func NodeIdentity(n Node) (string, error) {
	details := make([][2]string, len(n.Details))
	for i, d := range n.Details {
		details[i] = [2]string{d.K, d.V}
	}
	sort.SliceStable(details, func(i, j int) bool {
		return CompareStrings(marshalPair(details[i]), marshalPair(details[j])) < 0
	})
	tail := ""
	if n.Server == "" {
		if n.BaseName != "" {
			tail = n.BaseName
		} else {
			tail = n.Name
		}
	}
	parts := []any{
		strings.ToLower(n.Type),
		strings.ToLower(n.Server),
		n.PortString(),
		strings.ToLower(orDefault(n.SNI, "")),
		strings.ToLower(orDefault(n.Network, "")),
		n.TLS,
		n.Reality,
		n.UDP,
		details,
		tail,
	}
	b, err := marshalJSON(parts)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func marshalPair(p [2]string) string {
	b, _ := marshalJSON([2]string{p[0], p[1]})
	return string(b)
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// PortString reproduces String(node.port ?? "").
func (n Node) PortString() string {
	return n.Port
}

// AggregateIdentity is the connection identity used for dedupe, enrichment,
// retirement and revival. It is persisted in the database, so the exact hashing
// input must stay stable across implementations.
//
// Share links carry their own credentials, so the link alone identifies the
// connection. Clash configurations are canonicalised to ignore cosmetic
// differences between equivalent subscriptions. vmess share links are
// normalised by normalizeVmessConnection: two spellings of the same connection
// collapse into one identity. This intentionally changed the hash input for
// affected vmess links relative to the pre-normalisation build; identities
// re-converge on the next full registry rebuild, and the legacy oracle at
// legacy/node-server/node-sources.js keeps the unnormalised form (accepted
// drift, Go is authoritative since modularisation).
func AggregateIdentity(n Node) string {
	if len(n.RawConfig) == 0 && n.RawLink != "" {
		if id, ok := shareLinkIdentity(n.RawLink); ok {
			return "share-link:" + id
		}
		// Incomplete or unsupported links keep the summary-based identity; the
		// missed coverage stays observable instead of silent (§6.1).
		ShareLinkParseFailures.Add(1)
	}
	if len(n.RawConfig) == 0 {
		id, err := NodeIdentity(n)
		if err != nil {
			return ""
		}
		return id
	}
	hash, err := canonicalHash(connectionConfig(n.RawConfig))
	if err != nil {
		return ""
	}
	id, err := NodeIdentity(n)
	if err != nil {
		return ""
	}
	return id + hash
}

// shareLinkIdentity hashes the connection described by a share link. It returns
// false when the link cannot be parsed, so callers fall back to the summary.
func shareLinkIdentity(link string) (string, bool) {
	connection, ok := ShareLinkConnection(link)
	if !ok {
		return "", false
	}
	hash := sha256.Sum256([]byte(connection))
	return hex.EncodeToString(hash[:]), true
}

// ShareLinkConnection renders the canonical connection string that
// AggregateIdentity hashes for a share link. It exists so the parity checker
// can diff the hashing input itself instead of only the digest.
//
// vmess links are further normalised by normalizeVmessConnection: upstream
// aggregators emit the same effective connection with different spellings of
// no-op fields (numeric vs string port, absent vs empty transport fields,
// "---"/"auto" noise). The oracle at legacy/node-server/node-sources.js does
// not normalise these; Go is the authoritative implementation since the
// modularisation, so already-stored identities converge on the next full
// registry rebuild while variants collapse into one row.
func ShareLinkConnection(link string) (string, bool) {
	if strings.HasPrefix(link, "vmess://") {
		raw, err := decodeBase64(link[len("vmess://"):])
		if err != nil {
			return "", false
		}
		var config map[string]any
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			return "", false
		}
		// Display name and remarks describe a check, not a connection.
		delete(config, "ps")
		delete(config, "name")
		normalizeVmessConnection(config)
		b, err := canonicalJSON(config)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	return whatwgSerialize(link)
}

// vmessNoOpFields lists optional link fields whose absent, empty, default
// ("none"/"auto"), zero and false spellings all mean the same connection.
var vmessNoOpFields = []string{
	"sni", "host", "path", "fp", "alpn", "scy", "insecure",
	"skip-cert-verify", "headerType", "packetEncoding", "type",
}

// normalizeVmessConnection mutates the decoded vmess config in place, merging
// spellings that describe the same connection before it is hashed:
//
//   - numeric vs string scalars are unified on the numeric form for fields
//     that are semantically numeric (port, aid, v);
//   - no-op optional fields carrying unset values are removed;
//   - tls ""/"none"/false and udp false are removed; net "tcp"/"raw" is
//     removed, matching connectionConfig's default-folding rules.
//
// Everything else is preserved verbatim, so a real credential or transport
// difference still produces a distinct identity.
func normalizeVmessConnection(config map[string]any) {
	if config == nil {
		return
	}
	// Numeric/string unification for semantically numeric fields.
	for _, field := range []string{"port", "aid", "v"} {
		switch t := config[field].(type) {
		case string:
			if n, err := strconv.ParseFloat(t, 64); err == nil {
				if b, err := marshalJSON(n); err == nil {
					config[field] = json.RawMessage(b)
				}
			}
		}
	}
	for _, field := range vmessNoOpFields {
		switch t := config[field].(type) {
		case string:
			if t == "" || t == "none" || t == "auto" || t == "---" || t == "0" {
				delete(config, field)
			}
		case bool:
			if !t {
				delete(config, field)
			}
		case float64:
			if t == 0 {
				delete(config, field)
			}
		case nil:
			delete(config, field)
		}
	}
	switch t := config["tls"].(type) {
	case bool:
		if !t {
			delete(config, "tls")
		}
	case string:
		if t == "" || t == "none" {
			delete(config, "tls")
		}
	case nil:
		delete(config, "tls")
	}
	if v, ok := config["udp"].(bool); ok && !v {
		delete(config, "udp")
	}
	if s, ok := config["net"].(string); ok && (s == "tcp" || s == "raw") {
		delete(config, "net")
	}
}

// ConnectionConfig is the exported form of connectionConfig, used by the
// parity checker to compare hashing inputs.
func ConnectionConfig(proxy ProxyConfig) map[string]any {
	return connectionConfig(proxy)
}

// decorativeConfigFields lists config keys that name a subscription entry
// rather than describing the connection (display-name class). They are
// stripped before the canonical hash so equivalent subscriptions differing
// only cosmetically collapse (REFACTOR_PLAN §6.2).
var decorativeConfigFields = map[string]bool{"name": true, "ps": true}

// DecorativeConfigField reports whether a Clash YAML key is display-only and
// must stay out of every canonical hash. transport.ConfigDigest shares this
// table so the digest and the identity never disagree on a key.
func DecorativeConfigField(key string) bool {
	return decorativeConfigFields[key]
}

func connectionConfig(proxy ProxyConfig) map[string]any {
	config := make(map[string]any, len(proxy))
	for k, v := range proxy {
		if decorativeConfigFields[k] {
			continue
		}
		config[k] = v
	}
	if s, ok := config["network"].(string); ok && s == "tcp" {
		delete(config, "network")
	}
	if b, ok := config["tls"].(bool); ok && !b {
		delete(config, "tls")
	}
	if b, ok := config["udp"].(bool); ok && !b {
		delete(config, "udp")
	}
	if t, ok := config["type"].(string); ok && t == "vless" {
		if e, ok := config["encryption"].(string); ok && e == "none" {
			delete(config, "encryption")
		}
	}
	// Mihomo sends HTTP Basic auth only when both fields are non-empty; unused
	// upstream passwords sometimes hold the literal "None".
	if t, ok := config["type"].(string); ok && t == "http" {
		if emptyString(config["username"]) || emptyString(config["password"]) {
			delete(config, "username")
			delete(config, "password")
		}
	}
	return config
}

func emptyString(v any) bool {
	s, ok := v.(string)
	return !ok || s == ""
}

// SourceKey pairs the summary identity with the display name. Provenance is
// attached per (endpoint, name), because a reused name must not borrow another
// endpoint's credentials.
func SourceKey(n Node) string {
	id, err := NodeIdentity(n)
	if err != nil {
		return ""
	}
	pair := []any{id, n.Name}
	b, err := marshalJSON(pair)
	if err != nil {
		return ""
	}
	return string(b)
}

// SnapshotFingerprint identifies a detection batch by its archived content.
// Credentials are excluded: they are provenance, not part of what was detected.
func SnapshotFingerprint(s Snapshot) (string, error) {
	original := Snapshot{
		CheckedAt:  s.CheckedAt,
		SpeedTest:  s.SpeedTest,
		MediaCheck: s.MediaCheck,
		Nodes:      make([]Node, len(s.Nodes)),
	}
	for i, n := range s.Nodes {
		copy := n
		copy.ClearCredentials()
		original.Nodes[i] = copy
	}
	b, err := marshalJSON(original)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
