package httpserver

import "encoding/json"

// UnmarshalJSON exists in test builds only, so tests can decode a response into the DTO types.
// Production code never decodes a staffRef: the only constructors are the staffViewer methods.
func (s *staffRef) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		s.id = ""
		return nil
	}
	return json.Unmarshal(b, &s.id)
}

// String is the visible id, or "" when redacted (test convenience).
func (s *staffRef) String() string {
	if s == nil {
		return ""
	}
	return s.id
}
