package main

import (
	"bytes"
	"encoding/json"
)

// stdJSONUnmarshal decodes with UseNumber so 64-bit integers in client
// payloads survive the unmarshal/re-encode round trip (float64 would
// silently corrupt values > 2^53 when message arrays are rebuilt).
func stdJSONUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
