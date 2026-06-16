package lower

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// warnStampKeyCollisions emits one aggregated warning naming every
// workspace-status key two DISTINCT stamp commands collided on (the flat
// status-key namespace kept the first; see codegenContext.StampKeyCollisions).
// Surfacing it keeps the dropped command auditable rather than a silent loss.
// No-op on a nil writer or no collisions.
func warnStampKeyCollisions(w io.Writer, collisions map[string]bool) {
	if w == nil || len(collisions) == 0 {
		return
	}
	keys := make([]string, 0, len(collisions))
	for k := range collisions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "lower: %d workspace-status key(s) had colliding stamp commands "+
		"(distinct cmake stamp variables sanitized to the same key); kept the first per key: %s\n",
		len(keys), strings.Join(keys, ", "))
}

// populateWorkspaceStatusSink fills sink (status key -> producing shell
// command) with one entry per workspace-status key an EMITTED configure_file /
// file(WRITE) actually reads via its stamp_values — intersected with the
// recovered stampCommands so only keys with a known producer are emitted. This
// is the data the CLI turns into the --workspace_status_command helper script.
// No-op on a nil sink. Reset first so a reused sink (the driver runs ToIR more
// than once) reflects only the final pass.
func populateWorkspaceStatusSink(sink map[string]string, pkg *ir.Package, stampCommands map[string]string) {
	if sink == nil {
		return
	}
	for k := range sink {
		delete(sink, k)
	}
	if pkg == nil || len(stampCommands) == 0 {
		return
	}
	for i := range pkg.Targets {
		spec := pkg.Targets[i].CMakeConfigureFile
		if spec == nil {
			continue
		}
		for _, statusKey := range spec.StampValues {
			if cmd, ok := stampCommands[statusKey]; ok {
				sink[statusKey] = cmd
			}
		}
	}
}

// propagateStampVars expands the stamp-variable set transitively through
// verbatim `set(X ${Y})` copies recovered from the non-expanded trace.
//
// recoverExecuteProcess seeds stampVars with the DIRECT VCS-stamp output
// variables (git/hg/svn rev-parse → OUTPUT_VARIABLE), each mapped to its
// workspace-status key. Many projects don't feed that variable straight
// into a configure_file, though — they copy it first
// (`set(VERSION ${GIT_SHA})`) and the configure_file references the copy
// (`@VERSION@`). This walks the recovered assignment graph to a fixpoint:
// any variable assigned a verbatim copy of a (transitively) stamp variable
// becomes a stamp variable too, INHERITING the original's status key — so
// `@VERSION@` re-reads the same `STABLE_<GIT_SHA>` the original `@GIT_SHA@`
// would. Chains (`set(Z ${VERSION})` after `set(VERSION ${GIT_SHA})`)
// resolve via the fixpoint loop.
//
// Mutates stampVars in place. A direct stamp variable is never overwritten
// (its own status key wins over an inherited one). Cycles terminate: a
// variable only ever transitions absent→present, so each var flips at most
// once and the loop makes no further changes once every reachable var is
// marked.
func propagateStampVars(stampVars map[string]string, assignments []shadow.SetAssignment) {
	if len(stampVars) == 0 || len(assignments) == 0 {
		return
	}
	for {
		changed := false
		for _, a := range assignments {
			if _, dstAlready := stampVars[a.Dst]; dstAlready {
				continue
			}
			if key, srcIsStamp := stampVars[a.SrcVar]; srcIsStamp {
				stampVars[a.Dst] = key
				changed = true
			}
		}
		if !changed {
			return
		}
	}
}

// applyParentScopeForwards marks the caller-scope variable a helper function
// forwards a stamp value into (`set(${_var} "${out}" PARENT_SCOPE)` then
// `get_git_sha(GIT_SHA)`), so a configure_file referencing that variable
// (`@GIT_SHA@`) lifts to stamp_values instead of baking. recoverExecuteProcess
// seeds stampVars with the function-LOCAL OUTPUT_VARIABLE (`out`); each
// forward whose SrcVar is that local promotes its resolved Dst (the call
// argument, `GIT_SHA`) to a stamp var.
//
// Unlike propagateStampVars (which INHERITS the source var's key — the right
// call for a verbatim same-name-ish copy), the forwarded consumer is RE-KEYED
// to its own name: the source is a generic function-local (`out`) the operator
// never names in their --workspace_status_command, so the consumer reads
// STABLE_GIT_SHA, not STABLE_OUT. The STABLE_/VOLATILE_ prefix is preserved
// from the source key (a forwarded `date` stamp stays volatile).
//
// Mutates stampVars in place. Runs BEFORE propagateStampVars so a further
// verbatim copy of the now-marked consumer (`set(VERSION ${GIT_SHA})`)
// propagates from it. A variable already carrying a (direct) key is never
// overwritten.
//
// stampCommands (status key -> producing shell command) is re-keyed in step:
// the forwarded consumer's NEW key inherits the source key's command, since
// the same execute_process (the helper's `git describe`) produces the value
// the operator's --workspace_status_command must emit under the consumer key.
// The generic function-local source key is dropped — no configure_file reads
// it — so the emitted status script names only the consumer key. nil is
// tolerated (callers without a command map).
func applyParentScopeForwards(stampVars, stampCommands map[string]string, forwards []shadow.ParentScopeForward) {
	if len(stampVars) == 0 || len(forwards) == 0 {
		return
	}
	for _, f := range forwards {
		srcKey, srcIsStamp := stampVars[f.SrcVar]
		if !srcIsStamp {
			continue
		}
		if _, dstAlready := stampVars[f.Dst]; dstAlready {
			continue
		}
		prefix := "STABLE_"
		if strings.HasPrefix(srcKey, "VOLATILE_") {
			prefix = "VOLATILE_"
		}
		dstKey := statusKeyWithPrefix(prefix, f.Dst)
		stampVars[f.Dst] = dstKey
		if stampCommands != nil {
			if cmd, ok := stampCommands[srcKey]; ok && dstKey != srcKey {
				stampCommands[dstKey] = cmd
				delete(stampCommands, srcKey)
			}
		}
	}
}
