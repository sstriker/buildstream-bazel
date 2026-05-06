package fileapi

import "encoding/json"

// Codemodel is <reply>/codemodel-v2-*.json.
//
// Schema reference: cmake-file-api(7), "codemodel" object kind.
type Codemodel struct {
	Kind           string          `json:"kind"`
	Version        ObjectVersion   `json:"version"`
	Paths          CodemodelPaths  `json:"paths"`
	Configurations []Configuration `json:"configurations"`
}

// CodemodelPaths records source and build root absolute paths. The build path
// is non-deterministic across recordings (CMake creates per-invocation tmp
// dirs); avoid asserting on it in tests.
type CodemodelPaths struct {
	Source string `json:"source"`
	Build  string `json:"build"`
}

// Configuration is one build config (Release / Debug / etc.). Single-config
// generators (Ninja, Make) emit exactly one; multi-config (Xcode, MSVC) emit
// many.
type Configuration struct {
	Name        string            `json:"name"`
	Directories []ConfigDirectory `json:"directories"`
	Projects    []ConfigProject   `json:"projects"`
	Targets     []ConfigTargetRef `json:"targets"`
}

// ConfigDirectory is one entry in Configuration.Directories[]. It
// identifies a source/build directory pair within the configuration
// and carries install-rule and project membership metadata.
type ConfigDirectory struct {
	Source              string `json:"source"`
	Build               string `json:"build"`
	JSONFile            string `json:"jsonFile"`
	HasInstallRule      bool   `json:"hasInstallRule"`
	ProjectIndex        int    `json:"projectIndex"`
	TargetIndexes       []int  `json:"targetIndexes"`
	MinimumCMakeVersion struct {
		String string `json:"string"`
	} `json:"minimumCMakeVersion"`
}

// Directory mirrors the contents of one directory-*.json file
// (referenced by ConfigDirectory.JSONFile). Exposes per-directory
// install rules — install(FILES ...), install(DIRECTORY ...),
// install(EXPORT ...), install(TARGETS ...) — as a single
// Installers slice.
type Directory struct {
	Paths struct {
		Source string `json:"source"`
		Build  string `json:"build"`
	} `json:"paths"`
	Installers []DirectoryInstaller `json:"installers"`
}

// DirectoryInstaller is one install() invocation. Type
// distinguishes shapes:
//
//   - "target": install(TARGETS ...) — already covered by
//     Target.Install.
//   - "directory": install(DIRECTORY ...) — bulk source-tree
//     copy.
//   - "file": install(FILES ...) — explicit per-file copy.
//   - "export": install(EXPORT ...) — synthesized
//     <Pkg>Targets.cmake; covered by cmakecfg.
//
// Paths is a heterogeneous list: most installer types record
// plain string paths, but install(DIRECTORY) entries are
// {"from": ..., "to": ...} objects. We keep paths in a
// json.RawMessage so callers can decode whichever shape they
// need.
type DirectoryInstaller struct {
	Component   string            `json:"component"`
	Destination string            `json:"destination"`
	Type        string            `json:"type"`
	Paths       []json.RawMessage `json:"paths"`
}

// ConfigProject is one entry in Configuration.Projects[]. It groups
// directories and targets belonging to the same cmake project()
// declaration.
type ConfigProject struct {
	Name             string `json:"name"`
	DirectoryIndexes []int  `json:"directoryIndexes"`
	TargetIndexes    []int  `json:"targetIndexes"`
}

// ConfigTargetRef is a per-config index entry pointing at a target's full JSON.
type ConfigTargetRef struct {
	Name           string `json:"name"`
	Id             string `json:"id"`
	JSONFile       string `json:"jsonFile"`
	DirectoryIndex int    `json:"directoryIndex"`
	ProjectIndex   int    `json:"projectIndex"`
}

// Target is <reply>/target-<name>-<config>-*.json.
//
// This is the principal input to the lowering stage: it carries the source
// list, per-language compile groups (with includes/defines/flags), link
// information, and install rules for one target in one configuration.
type Target struct {
	Name           string             `json:"name"`
	Id             string             `json:"id"`
	Type           string             `json:"type"`
	NameOnDisk     string             `json:"nameOnDisk"`
	Backtrace      int                `json:"backtrace"`
	Folder         TargetFolder       `json:"folder"`
	Paths          TargetPaths        `json:"paths"`
	Sources        []TargetSource     `json:"sources"`
	SourceGroups   []SourceGroup      `json:"sourceGroups"`
	CompileGroups  []CompileGroup     `json:"compileGroups"`
	Artifacts      []TargetArtifact   `json:"artifacts"`
	Archive        *TargetArchive     `json:"archive,omitempty"`
	Link           *TargetLink        `json:"link,omitempty"`
	Dependencies   []TargetDependency `json:"dependencies,omitempty"`
	Install        *TargetInstall     `json:"install,omitempty"`
	BacktraceGraph BacktraceGraph     `json:"backtraceGraph"`
}

// TargetFolder is the FOLDER property value assigned to a target via
// set_target_properties(... PROPERTIES FOLDER ...). Purely cosmetic in
// cmake; recorded here so higher-level tools can group targets.
type TargetFolder struct {
	Name string `json:"name"`
}

// TargetPaths carries the source and build directory roots for a target.
// Both paths are absolute on the recording machine.
type TargetPaths struct {
	Source string `json:"source"`
	Build  string `json:"build"`
}

// TargetSource is one entry in target.sources[]. Path is relative to the
// project source root.
type TargetSource struct {
	Path              string `json:"path"`
	Backtrace         int    `json:"backtrace"`
	CompileGroupIndex int    `json:"compileGroupIndex"`
	SourceGroupIndex  int    `json:"sourceGroupIndex"`
	IsGenerated       bool   `json:"isGenerated"`
	FileSetIndex      *int   `json:"fileSetIndex,omitempty"`
}

// SourceGroup clusters source files under a named IDE filter (e.g. "Header
// Files", "Source Files"). Indexes refer into Target.Sources[].
type SourceGroup struct {
	Name          string `json:"name"`
	SourceIndexes []int  `json:"sourceIndexes"`
}

// CompileGroup aggregates sources sharing a language + flags + includes set.
type CompileGroup struct {
	Language                string             `json:"language"`
	SourceIndexes           []int              `json:"sourceIndexes"`
	CompileCommandFragments []CommandFragment  `json:"compileCommandFragments"`
	Includes                []CompileInclude   `json:"includes"`
	Defines                 []CompileDefine    `json:"defines"`
	Frameworks              []CompileFramework `json:"frameworks,omitempty"`
	PrecompileHeaders       []CompilePCH       `json:"precompileHeaders,omitempty"`
	LanguageStandard        *LanguageStandard  `json:"languageStandard,omitempty"`
}

// CommandFragment is one --flag or "-DFOO=bar" chunk passed to the compiler.
// Role is empty for compile fragments; for link fragments it's "flags",
// "libraries", "libraryPath", or "frameworkPath".
type CommandFragment struct {
	Fragment string `json:"fragment"`
	Role     string `json:"role,omitempty"`
}

// CompileInclude is one entry in CompileGroup.Includes[].
// IsSystem marks -isystem / -I (system) includes.
type CompileInclude struct {
	Path      string `json:"path"`
	IsSystem  bool   `json:"isSystem,omitempty"`
	Backtrace int    `json:"backtrace,omitempty"`
}

// CompileDefine is one -D entry in CompileGroup.Defines[]. The string
// is in the "NAME" or "NAME=VALUE" form cmake records.
type CompileDefine struct {
	Define    string `json:"define"`
	Backtrace int    `json:"backtrace,omitempty"`
}

// CompileFramework is one -F entry in CompileGroup.Frameworks[] (Apple only).
type CompileFramework struct {
	Path     string `json:"path"`
	IsSystem bool   `json:"isSystem,omitempty"`
}

// CompilePCH is one precompiled-header entry in
// CompileGroup.PrecompileHeaders[].
type CompilePCH struct {
	Header    string `json:"header"`
	Backtrace int    `json:"backtrace,omitempty"`
}

// LanguageStandard carries the CMAKE_<LANG>_STANDARD value
// (e.g. "17" for C++17) for a compile group.
type LanguageStandard struct {
	Standard   string `json:"standard"`
	Backtraces []int  `json:"backtraces,omitempty"`
}

// TargetArtifact is one on-disk output of a target (the built binary,
// shared library, archive, etc.). Path is build-dir-relative.
type TargetArtifact struct {
	Path string `json:"path"`
}

// TargetArchive is present for STATIC_LIBRARY targets.
type TargetArchive struct {
	CommandFragments []CommandFragment `json:"commandFragments,omitempty"`
	LTO              bool              `json:"lto,omitempty"`
}

// TargetLink is present for EXECUTABLE / SHARED_LIBRARY / MODULE_LIBRARY
// targets.
type TargetLink struct {
	Language         string            `json:"language"`
	CommandFragments []CommandFragment `json:"commandFragments,omitempty"`
	LTO              bool              `json:"lto,omitempty"`
	Sysroot          *struct {
		Path string `json:"path"`
	} `json:"sysroot,omitempty"`
}

// TargetDependency is one entry in Target.Dependencies[]. Id is the
// cmake target id of the depended-upon target (matches ConfigTargetRef.Id).
type TargetDependency struct {
	Id        string `json:"id"`
	Backtrace int    `json:"backtrace,omitempty"`
}

// TargetInstall lists DESTINATION entries declared via install(TARGETS ...).
// Each destination's path is relative to install.prefix.
type TargetInstall struct {
	Prefix struct {
		Path string `json:"path"`
	} `json:"prefix"`
	Destinations []TargetInstallDest `json:"destinations"`
}

// TargetInstallDest is one destination entry within a TargetInstall.
// Path is relative to TargetInstall.Prefix.
type TargetInstallDest struct {
	Path      string `json:"path"`
	Backtrace int    `json:"backtrace,omitempty"`
}

// BacktraceGraph is a deduplicated CST trace shared by all backtrace fields in
// a target file. Indices in the graph are referenced by the integer
// "backtrace" fields elsewhere in the same JSON object.
type BacktraceGraph struct {
	Commands []string        `json:"commands"`
	Files    []string        `json:"files"`
	Nodes    []BacktraceNode `json:"nodes"`
}

// BacktraceNode is one node in a BacktraceGraph. File and Command are
// indices into BacktraceGraph.Files and BacktraceGraph.Commands respectively;
// Parent is an optional index into BacktraceGraph.Nodes for the caller frame.
type BacktraceNode struct {
	File    int  `json:"file"`
	Line    int  `json:"line,omitempty"`
	Command int  `json:"command,omitempty"`
	Parent  *int `json:"parent,omitempty"`
}
