// Package probejson defines the on-disk format for one probe cell.
//
// A probe cell is one (variant, platform) pair: cmake configure runs
// against the probe project for that variant under that platform's
// exec environment, producing a fileapi.Reply. probejson serializes
// that pair into a JSON document the cross-host unifier (Stage 5)
// reads back to fold into a ResolvedToolchain.
//
// The format is symmetric: Marshal(variant, reply) writes a
// ProbeJSON; Unmarshal reads one back. Path inside fileapi.Reply is
// dropped on serialization — it points at the cmake build dir on
// the cell's executor, which has no meaning to the unifier.
//
// SchemaVersion lets us evolve the format. Loaders reject documents
// whose version they don't recognize so a stale unifier doesn't
// silently misread a newer probe-cell's output.
package probejson

import (
	"encoding/json"
	"fmt"

	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
)

// SchemaVersion is the on-disk format version. Bump when the
// embedded fileapi.Reply / Variant shape changes incompatibly.
const SchemaVersion = 1

// ProbeJSON is the top-level document. SchemaVersion validation is
// done explicitly by Unmarshal so callers don't need to inspect it.
type ProbeJSON struct {
	SchemaVersion int               `json:"schemaVersion"`
	Variant       toolchain.Variant `json:"variant"`
	Reply         ReplyJSON         `json:"reply"`
}

// ReplyJSON mirrors fileapi.Reply minus Path (which is meaningless
// across hosts). Each field is the same JSON-tagged struct fileapi
// already exposes, so Marshal/Unmarshal round-trips through
// encoding/json without custom logic.
type ReplyJSON struct {
	Index       fileapi.Index                `json:"index"`
	Codemodel   fileapi.Codemodel            `json:"codemodel"`
	Toolchains  fileapi.Toolchains           `json:"toolchains"`
	CMakeFiles  fileapi.CMakeFiles           `json:"cmakeFiles"`
	Cache       fileapi.Cache                `json:"cache"`
	Targets     map[string]fileapi.Target    `json:"targets,omitempty"`
	Directories map[string]fileapi.Directory `json:"directories,omitempty"`
}

// Marshal serializes one (variant, reply) pair to a pretty-printed
// JSON document. Pretty printing is chosen for human-readable
// per-cell artifacts during debugging — the unifier doesn't care
// about the formatting; reviewers do.
func Marshal(variant toolchain.Variant, reply *fileapi.Reply) ([]byte, error) {
	if reply == nil {
		return nil, fmt.Errorf("probejson: nil reply")
	}
	p := ProbeJSON{
		SchemaVersion: SchemaVersion,
		Variant:       variant,
		Reply: ReplyJSON{
			Index:       reply.Index,
			Codemodel:   reply.Codemodel,
			Toolchains:  reply.Toolchains,
			CMakeFiles:  reply.CMakeFiles,
			Cache:       reply.Cache,
			Targets:     reply.Targets,
			Directories: reply.Directories,
		},
	}
	return json.MarshalIndent(p, "", "  ")
}

// Unmarshal parses a ProbeJSON document. Returns the Variant + a
// *fileapi.Reply with Path unset. SchemaVersion mismatches are a
// hard error so a stale tool can't silently misread a newer probe
// artifact.
func Unmarshal(data []byte) (toolchain.Variant, *fileapi.Reply, error) {
	var p ProbeJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return toolchain.Variant{}, nil, fmt.Errorf("probejson: parse: %w", err)
	}
	if p.SchemaVersion != SchemaVersion {
		return toolchain.Variant{}, nil, fmt.Errorf("probejson: schemaVersion %d not supported (this build expects %d)", p.SchemaVersion, SchemaVersion)
	}
	return p.Variant, &fileapi.Reply{
		Index:       p.Reply.Index,
		Codemodel:   p.Reply.Codemodel,
		Toolchains:  p.Reply.Toolchains,
		CMakeFiles:  p.Reply.CMakeFiles,
		Cache:       p.Reply.Cache,
		Targets:     p.Reply.Targets,
		Directories: p.Reply.Directories,
	}, nil
}
