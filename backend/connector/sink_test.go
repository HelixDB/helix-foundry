package connector

import (
	"encoding/json"
)

// rowSink collects synced rows as decoded maps.
type rowSink struct{ rows []map[string]any }

func (s *rowSink) Write(row map[string]any) error {
	// Round-trip through JSON so assertions see what lands in the snapshot.
	b, _ := json.Marshal(row)
	return s.WriteJSON(b)
}

func (s *rowSink) WriteJSON(row []byte) error {
	var m map[string]any
	if err := json.Unmarshal(row, &m); err != nil {
		return err
	}
	s.rows = append(s.rows, m)
	return nil
}
