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
