package fileapi

// Cache is <reply>/cache-v2-*.json.
//
// Mirror of CMakeCache.txt at end-of-configure. We don't need most entries,
// but specific ones (e.g. CMAKE_<LANG>_COMPILER_ID, BUILD_SHARED_LIBS) drive
// downstream decisions in lower/.
//
// Schema reference: cmake-file-api(7), "cache" object kind.
type Cache struct {
	Kind    string        `json:"kind"`
	Version ObjectVersion `json:"version"`
	Entries []CacheEntry  `json:"entries"`
	index   map[string]int
}

// CacheEntry is one cache variable. Properties carry HELPSTRING, ADVANCED, etc.
type CacheEntry struct {
	Name       string           `json:"name"`
	Value      string           `json:"value"`
	Type       string           `json:"type"`
	Properties []CacheEntryProp `json:"properties,omitempty"`
}

// CacheEntryProp is one named property attached to a CacheEntry
// (e.g. HELPSTRING, ADVANCED).
type CacheEntryProp struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (c *Cache) buildIndex() {
	if len(c.Entries) == 0 {
		c.index = nil
		return
	}
	idx := make(map[string]int, len(c.Entries))
	for i := range c.Entries {
		if _, seen := idx[c.Entries[i].Name]; seen {
			continue
		}
		idx[c.Entries[i].Name] = i
	}
	c.index = idx
}

// Get returns the entry with the given name, or nil if not present. Names are
// case-sensitive (matching CMake).
func (c Cache) Get(name string) *CacheEntry {
	if i, ok := c.index[name]; ok {
		return &c.Entries[i]
	}
	for i := range c.Entries {
		if c.Entries[i].Name == name {
			return &c.Entries[i]
		}
	}
	return nil
}
