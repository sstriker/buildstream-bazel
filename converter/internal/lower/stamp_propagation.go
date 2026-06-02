package lower

import "github.com/sstriker/buildstream-bazel/internal/shadow"

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
