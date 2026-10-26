package domain

import (
	"bytes"
	"encoding/json"
)

// marshalJSON renders a value the way JSON.stringify does. Go's encoder
// escapes HTML characters, which upstream JSON does not.
func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
