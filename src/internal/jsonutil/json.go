// Package jsonutil supplies the strict JSON boundary shared by configuration and Messages.
package jsonutil

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
)

var (
	ErrTooLarge = errors.New("JSON input exceeds the byte limit")
	ErrInvalid  = errors.New("invalid JSON object or field type")
	ErrUnknown  = errors.New("unsupported JSON field")
)

// Decode requires one object, exact field names, valid UTF-8 and unique object
// keys, including inside raw values. Errors deliberately omit input contents.
func Decode(r io.Reader, limit int64, out any) error {
	if limit < 1 || limit >= 1<<62 {
		return errors.New("invalid JSON byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return errors.New("could not read JSON input")
	}
	if int64(len(data)) > limit {
		return ErrTooLarge
	}
	return Object(data, out)
}

func Object(data []byte, out any) error {
	value := jsontext.Value(data)
	if value.Kind() != '{' || !value.IsValid() {
		return ErrInvalid
	}
	if err := json.Unmarshal(data, out, json.RejectUnknownMembers(true)); err != nil {
		if errors.Is(err, json.ErrUnknownName) {
			return ErrUnknown
		}
		return ErrInvalid
	}
	return nil
}

// Equal ignores object key order and JSON encoding whitespace, but preserves
// array order, text whitespace and number spellings without float conversion.
func Equal(a, b any) bool {
	x, err := Canonical(a)
	if err != nil {
		return false
	}
	y, err := Canonical(b)
	return err == nil && bytes.Equal(x, y)
}

// Canonical preserves the same comparison semantics for keyed fingerprints.
func Canonical(v any) (jsontext.Value, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	value := jsontext.Value(data)
	err = value.Format(jsontext.ReorderRawObjects(true), jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false))
	return value, err
}
