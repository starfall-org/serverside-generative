package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

var ErrMalformed = errors.New("malformed RPC request")

// Decode rejects duplicate fields and excessive nesting consistently across transports.
func Decode(raw []byte) (Request, error) {
	if len(raw) == 0 || len(raw) > 4<<20 {
		return Request{}, ErrMalformed
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if checkValue(d, 0) != nil {
		return Request{}, ErrMalformed
	}
	if _, err := d.Token(); err != io.EOF {
		return Request{}, ErrMalformed
	}
	var req Request
	if json.Unmarshal(raw, &req) != nil {
		return Request{}, ErrMalformed
	}
	return req, nil
}
func checkValue(d *json.Decoder, depth int) error {
	if depth > 128 {
		return ErrMalformed
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			value, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := value.(string)
			if !ok || seen[key] {
				return ErrMalformed
			}
			seen[key] = true
			if err = checkValue(d, depth+1); err != nil {
				return err
			}
		}
		t, err := d.Token()
		if err != nil || t != json.Delim('}') {
			return ErrMalformed
		}
	case '[':
		for d.More() {
			if err = checkValue(d, depth+1); err != nil {
				return err
			}
		}
		t, err := d.Token()
		if err != nil || t != json.Delim(']') {
			return ErrMalformed
		}
	default:
		return ErrMalformed
	}
	return nil
}
