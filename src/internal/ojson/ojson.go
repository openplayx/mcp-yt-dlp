// Package ojson is a JSON object that keeps its key order, so responses
// read in a stable, meaningful order (Go maps would sort the keys).
package ojson

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Object is an ordered JSON object. The zero value is an empty object.
type Object struct {
	keys []string
	vals map[string]any
}

// New builds an object from alternating key, value arguments.
func New(kv ...any) *Object {
	o := &Object{}
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Set adds a key at the end, or replaces its value in place.
func (o *Object) Set(k string, v any) *Object {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

func (o *Object) Get(k string) (any, bool) {
	if o == nil || o.vals == nil {
		return nil, false
	}
	v, ok := o.vals[k]
	return v, ok
}

func (o *Object) Delete(k string) {
	if o == nil || o.vals == nil {
		return
	}
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Clone copies the top level.
func (o *Object) Clone() *Object {
	c := &Object{}
	if o == nil {
		return c
	}
	for _, k := range o.keys {
		c.Set(k, o.vals[k])
	}
	return c
}

func (o *Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	if o != nil {
		for i, k := range o.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			vb, err := marshal(o.vals[k])
			if err != nil {
				return nil, err
			}
			buf.Write(vb)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// UnmarshalJSON keeps the key order of the input; nested objects become
// *Object too, numbers stay json.Number-free float64 like encoding/json.
func (o *Object) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	v, err := decodeValue(dec)
	if err != nil {
		return err
	}
	obj, ok := v.(*Object)
	if !ok {
		return errors.New("ojson: not an object")
	}
	*o = *obj
	return nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &Object{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			list := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, errors.New("ojson: unexpected delimiter")
	default:
		return t, nil
	}
}
