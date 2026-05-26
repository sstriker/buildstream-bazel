package fileapi

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ConfigureLog is the parsed contents of a configureLog-v1 object.
//
// cmake's configureLog object (added in cmake 3.26) is two-level: the
// reply directory carries a small JSON sidecar pointing at the actual
// log YAML, which lives outside the reply directory. Load() decodes
// the sidecar; the YAML is read on demand via LoadConfigureLogYAML
// so the fileapi package's "no I/O outside the supplied directory"
// invariant stays intact.
//
// Schema reference: cmake-file-api(7), section "Object Kind 'configureLog'".
type ConfigureLog struct {
	// Path is the absolute path to CMakeConfigureLog.yaml on the
	// recording machine. The file may not exist on disk (per cmake
	// docs) — only events from the most recent configure are
	// persisted, and a configure with no try_compile / find_package /
	// message events writes no log at all.
	Path string `json:"path"`

	// EventKindNames lists the event types the log carries
	// (e.g., "find_package-v1", "try_compile-v1", "try_run-v1",
	// "message-v1"). Each entry is the full versioned kind string
	// that appears as Event.Kind in the YAML.
	EventKindNames []string `json:"eventKindNames"`
}

// Event is one record in CMakeConfigureLog.yaml.
//
// Each kind populates a different subset of the struct's fields;
// callers branch on Kind. The full yaml.Node for the record is
// preserved in Raw for forward-compat with event kinds the typed
// accessors don't yet cover.
type Event struct {
	// Kind is the versioned event kind string (e.g., "try_compile-v1",
	// "find_package-v1", "try_run-v1", "message-v1").
	Kind string `yaml:"kind"`

	// Backtrace is the cmake call stack at the event site, formatted
	// "<file>:<line> (<command>)" per cmake's convention.
	Backtrace []string `yaml:"backtrace,omitempty"`

	// Checks is the chain of human-readable check descriptions from
	// the enclosing message(CHECK_START) / message(CHECK_PASS) /
	// message(CHECK_FAIL) blocks, outermost first.
	Checks []string `yaml:"checks,omitempty"`

	// Description, when set, carries an event-specific description
	// (try_compile / try_run record one).
	Description string `yaml:"description,omitempty"`

	// Directories is set on try_compile-v1 / try_run-v1: the
	// per-event source and binary directories cmake materialised the
	// probe into.
	Directories *EventDirectories `yaml:"directories,omitempty"`

	// CMakeVariables is the set of cmake variable values cmake bound
	// into the probe environment (typically the
	// CMAKE_TRY_COMPILE_PLATFORM_VARIABLES projection).
	CMakeVariables map[string]string `yaml:"cmakeVariables,omitempty"`

	// BuildResult is set on try_compile-v1 / try_run-v1: outcome of
	// the compile-and-link step.
	BuildResult *EventBuildResult `yaml:"buildResult,omitempty"`

	// RunResult is set on try_run-v1 only: outcome of executing the
	// built artifact (absent under cmake's cross-compile gate).
	RunResult *EventRunResult `yaml:"runResult,omitempty"`

	// Components is the list of COMPONENTS / OPTIONAL_COMPONENTS the
	// find_package() call requested. Set on find_package-v1.
	Components []string `yaml:"components,omitempty"`

	// Found is set on find_package-v1: the resolved package metadata
	// (or absence when the package wasn't located).
	Found *EventFindPackageFound `yaml:"found,omitempty"`

	// Mode is the message() severity ("STATUS", "WARNING", etc.) on
	// message-v1 events. message-v1 records every message() call
	// once cmake 3.26+ has the configureLog active, so this is a
	// high-cardinality field — consumers typically filter to
	// CHECK_* / DEPRECATION / etc.
	Mode string `yaml:"mode,omitempty"`

	// Message is the message body on message-v1 events.
	Message string `yaml:"message,omitempty"`

	// Raw preserves the entire YAML mapping for fields the typed
	// accessors above don't surface yet. Empty when the event was
	// decoded from a typed accessor and the caller didn't request
	// the raw node.
	Raw yaml.Node `yaml:"-"`
}

// EventDirectories carries the source and build directories cmake set
// up for a try_compile / try_run probe.
type EventDirectories struct {
	Source string `yaml:"source"`
	Binary string `yaml:"binary"`
}

// EventBuildResult is the compile-and-link outcome of a try_compile /
// try_run probe. Variable is the cmake cache variable cmake bound the
// result to; Cached indicates whether cmake found a pre-existing
// cached answer rather than executing the probe.
type EventBuildResult struct {
	Variable string `yaml:"variable"`
	Cached   bool   `yaml:"cached"`
	Stdout   string `yaml:"stdout,omitempty"`
	ExitCode int    `yaml:"exitCode"`
}

// EventRunResult is the artifact-execution outcome of a try_run probe.
// Absent when cmake skipped the run step (CMAKE_CROSSCOMPILING with
// no emulator).
type EventRunResult struct {
	Variable string `yaml:"variable"`
	Cached   bool   `yaml:"cached"`
	Stdout   string `yaml:"stdout,omitempty"`
	Stderr   string `yaml:"stderr,omitempty"`
	ExitCode int    `yaml:"exitCode"`
}

// EventFindPackageFound is the resolved-package payload on a
// find_package-v1 (or find-v1) event. Absent when the field wasn't
// emitted.
//
// The `found` key in cmake's CMakeConfigureLog.yaml is polymorphic
// across cmake versions. cmake 3.26–4.2 emitted a mapping for
// `find_package-v1` (IsFound + Package + Version + ConfigFile +
// VersionFile). cmake 4.3 added a new `find-v1` event kind (sibling
// to `find_package-v1`) that records every find_program / find_file /
// find_library call, sharing the same `found` key but with a scalar
// payload:
//
//   - string — absolute path of the located item (`found: "/usr/bin/cc"`).
//   - bool false — looked up but not found (`found: false`).
//   - null — looked up, no candidate considered (`found: null`).
//   - bool true — defensive forward-compat; no cmake emits this today
//     but the unmarshaler treats it as "found, path unknown".
//
// The custom UnmarshalYAML normalises all four shapes onto this one
// struct so downstream consumers (lower's find_package attribution,
// find-package header comments) don't have to branch on event kind.
// Existing callers that read IsFound / Package / Version / ConfigFile
// continue to work unchanged; the new Path field surfaces the scalar
// payload when present.
type EventFindPackageFound struct {
	IsFound     bool   `yaml:"isFound"`
	Package     string `yaml:"package,omitempty"`
	Version     string `yaml:"version,omitempty"`
	ConfigFile  string `yaml:"configFile,omitempty"`
	VersionFile string `yaml:"versionFile,omitempty"`

	// Path carries the located absolute path on the cmake 4.3+
	// scalar-string shape (find-v1 events: `found: "/usr/bin/cc"`).
	// Empty on the mapping shape, on the bool/null shapes, and on
	// find_package-v1 events that record a mapping.
	Path string `yaml:"-"`
}

// UnmarshalYAML decodes the polymorphic `found` value into
// EventFindPackageFound. See the type's docstring for the full
// shape matrix.
func (e *EventFindPackageFound) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		switch value.Tag {
		case "!!str":
			// `found: "/usr/bin/cc"` (cmake 4.3 find-v1 shape).
			// A non-empty string is a successful resolution.
			e.Path = value.Value
			e.IsFound = value.Value != ""
			return nil
		case "!!bool":
			// `found: false` (or, defensively, `true`) on a
			// find-v1 event that ran the search but is recording
			// the outcome as a plain bool.
			var b bool
			if err := value.Decode(&b); err != nil {
				return err
			}
			e.IsFound = b
			return nil
		case "!!null":
			// `found: null` — searched, no candidate considered.
			// Same semantic effect as `found: false` for callers:
			// IsFound stays its zero value (false).
			return nil
		default:
			// Unknown scalar tag — fall through to a permissive
			// decode so a future cmake reshaping doesn't fail
			// the whole configureLog parse over one record.
			var s string
			if err := value.Decode(&s); err == nil && s != "" {
				e.Path = s
				e.IsFound = true
				return nil
			}
			return nil
		}
	case yaml.MappingNode:
		// `found: { isFound: ..., package: ..., ... }` — the
		// cmake 3.26–4.2 find_package-v1 mapping shape. Decode
		// via a typedef alias to bypass the custom UnmarshalYAML
		// (otherwise we'd recurse forever).
		type plain EventFindPackageFound
		var p plain
		if err := value.Decode(&p); err != nil {
			return err
		}
		*e = EventFindPackageFound(p)
		return nil
	case yaml.AliasNode:
		// Aliases resolve transparently — defer to the
		// referenced node.
		if value.Alias == nil {
			return nil
		}
		return e.UnmarshalYAML(value.Alias)
	default:
		// SequenceNode / DocumentNode — no cmake version emits
		// these for `found`, but be permissive rather than fail.
		return nil
	}
}

// configureLogYAML is the top-level YAML shape of CMakeConfigureLog.yaml.
type configureLogYAML struct {
	Events []Event `yaml:"events"`
}

// LoadConfigureLogYAML reads CMakeConfigureLog.yaml at path and
// returns the parsed event list. Returns an empty slice and nil error
// when the file does not exist (cmake writes no log if no
// configureLog-aware event fired during configure); other I/O or
// parse errors are returned as-is.
func LoadConfigureLogYAML(path string) ([]Event, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fileapi: read configure log %s: %w", path, err)
	}
	var doc configureLogYAML
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("fileapi: parse configure log %s: %w", path, err)
	}
	return doc.Events, nil
}
