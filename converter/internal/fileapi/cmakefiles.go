package fileapi

// CMakeFiles is <reply>/cmakeFiles-v1-*.json.
//
// Lists every file consumed during configure: CMakeLists, included .cmake
// modules (project-internal and bundled), generated configure-time outputs.
// We use this to drive shadow-tree allowlist augmentation (M3) and to compute
// the "configure inputs" content fingerprint (M3 cache key).
//
// Schema reference: cmake-file-api(7), "cmakeFiles" object kind.
type CMakeFiles struct {
	Kind    string         `json:"kind"`
	Version ObjectVersion  `json:"version"`
	Paths   CMakeFilePaths `json:"paths"`
	Inputs  []CMakeFileIn  `json:"inputs"`
	// GlobsDependent lists every file(GLOB ... CONFIGURE_DEPENDS)
	// expression and the files it matched at configure time. Added in
	// cmakeFiles-v1 minor 1 (cmake 3.29+); empty on older cmake or when
	// no CONFIGURE_DEPENDS glob fired. cmake re-evaluates these globs at
	// build time and reconfigures when the match set changes — the ninja
	// RERUN_CMAKE edge depends on the glob *stamp*, not the matched
	// files, so this is the only authoritative record of the resolved
	// set. convert-element-cmake folds the matched Paths into its
	// configure-inputs oracle so the converter's cache invalidates the
	// same way cmake's configure does.
	GlobsDependent []CMakeFileGlob `json:"globsDependent,omitempty"`
}

type CMakeFilePaths struct {
	Source string `json:"source"`
	Build  string `json:"build"`
}

// CMakeFileGlob is one file(GLOB ... CONFIGURE_DEPENDS) record in
// CMakeFiles.GlobsDependent. Expression is the cmake glob pattern;
// Paths are the files it matched at configure time (absolute on the
// recording machine, unless Relative is set, in which case they are
// relative to it). Recurse / ListDirectories / FollowSymlinks mirror
// the file(GLOB_RECURSE ...) / LIST_DIRECTORIES / FOLLOW_SYMLINKS
// options; they're informational — only Paths drives the configure-
// inputs fold.
type CMakeFileGlob struct {
	Expression      string   `json:"expression"`
	Recurse         bool     `json:"recurse,omitempty"`
	ListDirectories bool     `json:"listDirectories,omitempty"`
	FollowSymlinks  bool     `json:"followSymlinks,omitempty"`
	Relative        string   `json:"relative,omitempty"`
	Paths           []string `json:"paths,omitempty"`
}

// CMakeFileIn flags whether each consumed file is a generated artifact, an
// external file (outside the source root, e.g. CMake bundled modules), or a
// .cmake-language script.
type CMakeFileIn struct {
	Path        string `json:"path"`
	IsGenerated bool   `json:"isGenerated,omitempty"`
	IsExternal  bool   `json:"isExternal,omitempty"`
	IsCMake     bool   `json:"isCMake,omitempty"`
}
