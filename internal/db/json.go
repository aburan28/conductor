package db

import "encoding/json"

// jsonb columns are read as []byte and decoded explicitly rather than relying on the driver
// to infer a destination type. It costs a few lines per scan and removes a whole class of
// "worked in one pgx version, silently changed in the next" surprises.

func marshalJSON(v any) ([]byte, error) {
	if v == nil {
		return []byte("null"), nil
	}
	return json.Marshal(v)
}

// decodeJSON unmarshals a jsonb column into dst, tolerating SQL NULL and empty payloads so a
// row written before a field existed still loads.
func decodeJSON(raw []byte, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dst)
}
