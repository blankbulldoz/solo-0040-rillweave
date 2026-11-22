package walk

import (
	"encoding/base64"
	"encoding/json"

	"rillweave/graph"
	"rillweave/index"
)

// pageTokenVersion tags the on-wire cursor layout so an incompatible cursor
// can be rejected instead of misread.
const pageTokenVersion = 1

// pageToken is the opaque continuation of an adjacency read. It pins the
// logical scan (vertex, direction, canonical kind selection), the revision
// that produced the page and the position immediately after the last emitted
// arc. offset is the number of matched rows already delivered; it lets the
// next page report truncated without probing an extra row.
type pageToken struct {
	Version   int             `json:"v"`
	Vertex    string          `json:"vertex"`
	Direction index.Direction `json:"direction"`
	Kinds     []string        `json:"kinds"`
	Revision  uint64          `json:"revision"`
	After     string          `json:"after"`
	Offset    int             `json:"offset"`
}

func encodeToken(t pageToken) string {
	t.Version = pageTokenVersion
	raw, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeToken(cursor string) (pageToken, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return pageToken{}, graph.Fail(graph.Invalid, "cursor is not a valid adjacency continuation token")
	}
	var t pageToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return pageToken{}, graph.Fail(graph.Invalid, "cursor is not a valid adjacency continuation token")
	}
	if t.Version != pageTokenVersion {
		return pageToken{}, graph.Fail(graph.Invalid, "cursor version %d is not supported", t.Version)
	}
	return t, nil
}

// canonicalKinds sorts and de-duplicates the requested kinds so the same
// logical selection always serializes identically inside a cursor.
func canonicalKinds(kinds []string) []string {
	if len(kinds) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(kinds))
	canonical := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		canonical = append(canonical, kind)
	}
	for i := 1; i < len(canonical); i++ {
		for j := i; j > 0 && canonical[j-1] > canonical[j]; j-- {
			canonical[j-1], canonical[j] = canonical[j], canonical[j-1]
		}
	}
	return canonical
}

func sameKinds(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
