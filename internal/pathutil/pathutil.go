// Package pathutil holds small path predicates shared across the converter and
// internal trees. Leaf package (stdlib-only), so both trees import it cycle-free.
package pathutil

import (
	"path/filepath"
	"strings"
)

// InsideRoot reports whether rel — a path already made relative to some root
// (e.g. via filepath.Rel) — points at a location strictly inside that root.
// Empty, ".", and ".." are not "inside" (they're the root itself or its parent),
// and any "../"-prefixed path escapes the root.
func InsideRoot(rel string) bool {
	if rel == "" || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
