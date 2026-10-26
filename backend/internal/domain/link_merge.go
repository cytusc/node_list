package domain

import "encoding/json"

// MergeSnapshotLinks enriches provenance without changing observations or node
// identities. A batch fingerprint intentionally excludes credentials; DO NOTHING
// on duplicate fingerprints would silently discard late-arriving original URIs.
func MergeSnapshotLinks(existing, incoming []byte) ([]byte, bool, error) {
	var old, next Snapshot
	if err := json.Unmarshal(existing, &old); err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(incoming, &next); err != nil {
		return nil, false, err
	}
	byID := map[string]Node{}
	for _, n := range next.Nodes {
		byID[AggregateIdentity(n)] = n
	}
	changed := false
	for i, n := range old.Nodes {
		v, ok := byID[AggregateIdentity(n)]
		if !ok {
			continue
		}
		// Never attach another credential variant merely because address/name match.
		if ConnectionKey(n) == "" || ConnectionKey(n) != ConnectionKey(v) {
			continue
		}
		if n.RawLink == "" && v.RawLink != "" {
			old.Nodes[i].RawLink = v.RawLink
			changed = true
		}
		if n.GeneratedLink == "" && v.GeneratedLink != "" {
			old.Nodes[i].GeneratedLink = v.GeneratedLink
			changed = true
		}
	}
	if !changed {
		return existing, false, nil
	}
	raw, err := json.Marshal(old)
	return raw, true, err
}
