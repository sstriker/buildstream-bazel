package fileapi

// Index is the parsed contents of <reply>/index-*.json.
//
// Schema reference: cmake-file-api(7), section "Object Kinds".
type Index struct {
	CMake   IndexCMake    `json:"cmake"`
	Objects []IndexObject `json:"objects"`
}

// IndexCMake is the cmake metadata block.
type IndexCMake struct {
	Generator IndexGenerator `json:"generator"`
	Paths     IndexPaths     `json:"paths"`
	Version   IndexVersion   `json:"version"`
}

// IndexGenerator describes the cmake generator used for the build
// (e.g. "Ninja", "Unix Makefiles").
type IndexGenerator struct {
	Name        string `json:"name"`
	MultiConfig bool   `json:"multiConfig"`
}

// IndexPaths records the absolute paths to cmake, ctest, cpack, and
// the cmake root directory as reported in the index file.
type IndexPaths struct {
	CMake string `json:"cmake"`
	CTest string `json:"ctest"`
	CPack string `json:"cpack"`
	Root  string `json:"root"`
}

// IndexVersion is the cmake version reported in the index file.
type IndexVersion struct {
	Major   int    `json:"major"`
	Minor   int    `json:"minor"`
	Patch   int    `json:"patch"`
	String  string `json:"string"`
	Suffix  string `json:"suffix"`
	IsDirty bool   `json:"isDirty"`
}

// IndexObject points to one per-kind JSON object file.
type IndexObject struct {
	Kind     string        `json:"kind"`
	Version  ObjectVersion `json:"version"`
	JSONFile string        `json:"jsonFile"`
}

// ObjectVersion is the per-kind schema version.
type ObjectVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

// SupportedObjectMajors records the codemodel-v2 / cache-v2 / toolchains-v1
// / cmakeFiles-v1 / configureLog-v1 schema majors this loader knows how to
// parse. Per the File API contract a client must reject unknown majors;
// minor bumps are additive and parsed best-effort (unknown new fields drop
// silently via json.Unmarshal). Tested against CMake 3.20 through 4.x.
//
// configureLog-v1 was added in cmake 3.26 (see Phase 2 of the
// generator-parity uplift in ROADMAP.md); cmake < 3.26 silently ignores
// the staged query, so the entry below is benign on older versions —
// the reply simply omits the object.
var SupportedObjectMajors = map[string]int{
	"codemodel":    2,
	"cache":        2,
	"toolchains":   1,
	"cmakeFiles":   1,
	"configureLog": 1,
}
