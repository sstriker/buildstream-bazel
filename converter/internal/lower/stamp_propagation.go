package lower

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

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
func applyParentScopeForwards(stampVars map[string]string, forwards []shadow.ParentScopeForward) {
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
		stampVars[f.Dst] = statusKeyWithPrefix(prefix, f.Dst)
	}
}
