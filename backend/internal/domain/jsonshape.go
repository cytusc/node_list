package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// jsonShape distinguishes an absent property from an explicit zero or null.
// Value semantics keep decoded node copies independent without copying a map.
type jsonShape struct {
	present uint64
	nulls   uint64
	decoded bool
}

type jsonField struct {
	name      string
	index     int
	omitEmpty bool
}

func jsonFields(t reflect.Type) []jsonField {
	var fields []jsonField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")
		if f.PkgPath != "" || tag[0] == "-" {
			continue
		}
		name := tag[0]
		if name == "" {
			name = f.Name
		}
		fields = append(fields, jsonField{name, i, len(tag) > 1 && tag[1] == "omitempty"})
	}
	if len(fields) > 64 {
		panic("domain JSON shape exceeds 64 fields")
	}
	return fields
}

var nodeJSONFields = jsonFields(reflect.TypeOf(Node{}))
var ippureJSONFields = jsonFields(reflect.TypeOf(IPPureResult{}))

func readJSONShape(data []byte, fields []jsonField) (jsonShape, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return jsonShape{}, err
	}
	shape := jsonShape{decoded: true}
	for i, f := range fields {
		if value, ok := raw[f.name]; ok {
			shape.present |= uint64(1) << i
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				shape.nulls |= uint64(1) << i
			}
		}
	}
	return shape, nil
}

func jsonEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	default:
		return v.IsZero()
	}
}

// marshalJSONShape keeps the ordinary encoder path for normal payloads. Only
// exceptional shapes need a second pass. Fields retain declaration order so
// serialized nodes do not depend on map iteration or sorted JSON keys.
func marshalJSONShape(value any, shape jsonShape, fields []jsonField) ([]byte, error) {
	encoded, err := marshalJSON(value)
	if err != nil || !shape.decoded {
		return encoded, err
	}
	v := reflect.ValueOf(value)
	var changed uint64
	for i, f := range fields {
		bit := uint64(1) << i
		fv := v.Field(f.index)
		if jsonEmpty(fv) && ((shape.present&bit != 0 && f.omitEmpty) ||
			(shape.present&bit == 0 && !f.omitEmpty) ||
			(shape.nulls&bit != 0 && fv.IsZero())) {
			changed |= bit
		}
	}
	if changed == 0 {
		return encoded, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for i, f := range fields {
		bit := uint64(1) << i
		field, exists := raw[f.name]
		if changed&bit != 0 {
			if shape.present&bit == 0 {
				continue
			}
			exists = true
			fv := v.Field(f.index)
			if shape.nulls&bit != 0 && fv.IsZero() {
				field = json.RawMessage("null")
			} else {
				field, err = marshalJSON(fv.Interface())
				if err != nil {
					return nil, err
				}
			}
		}
		if !exists {
			continue
		}
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		key, _ := json.Marshal(f.name)
		out.Write(key)
		out.WriteByte(':')
		out.Write(field)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
