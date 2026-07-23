package main

import "encoding/json"

func stdJSONUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func stdJSONMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
