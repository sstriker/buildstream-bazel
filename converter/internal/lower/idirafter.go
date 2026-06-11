package lower

import (
	"path/filepath"
	"strings"
)

// reanchorIDirAfterCopts rewrites source-tree `-idirafter` directories
// from convert-time absolute paths to exec-root-relative form, and
// returns the element-relative dirs for the header-discovery walk.
//
// cmake passes -idirafter through as a raw compile option (sdl's
// vendored-Khronos headers: `-idirafter <src>/src/video/khronos`), and
// the lowering used to keep the joined token VERBATIM — a convert-time
// absolute path that bazel >= 8's hermetic sandbox /tmp makes invisible
// at compile time, breaking header resolution (`EGL/egl.h` not found)
// whenever the survey source dir lives under /tmp. The flag must stay a
// COPT (its search position — after the system include chain — is its
// semantics; routing it to `includes` would promote it to -isystem
// position and could shadow real system headers), so the faithful fix is
// the same exec-root re-anchoring verbatim copts get elsewhere (the PCH
// mirror's -include, genrule cmds): compile actions run from the exec
// root, so `<pkgPath>/<element-rel>` resolves hermetically.
//
// The returned walkDirs feed stageTargetHeaders' discoverHeaders walk
// (the privateIncDirs channel) so the dir's headers are DECLARED action
// inputs — the copt alone sets the search path but stages no files,
// exactly the PRIVATE -I precedent.
//
// Shapes handled: the joined token (`-idirafter<dir>`, cmake's usual
// emission) and the separate pair (`-idirafter <dir>`); both re-emit in
// the joined form. Non-source-tree dirs (system /usr/..., build-dir)
// keep their tokens verbatim — a system dir resolves on the host
// toolchain's view, and no corpus member emits a build-dir -idirafter.
func reanchorIDirAfterCopts(copts []string, cmakeSrc string, reanchor func(string) string, pkgPath string) (out, walkDirs []string) {
	const flag = "-idirafter"
	seenWalk := map[string]bool{}
	out = copts[:0]
	for i := 0; i < len(copts); i++ {
		tok := copts[i]
		dir := ""
		consumed := 0
		switch {
		case tok == flag && i+1 < len(copts):
			dir = copts[i+1]
			consumed = 1
		case strings.HasPrefix(tok, flag) && len(tok) > len(flag):
			dir = tok[len(flag):]
		}
		if dir == "" || !filepath.IsAbs(dir) {
			out = append(out, tok)
			continue
		}
		rel, inside := relativeIfInside(cmakeSrc, dir)
		if !inside || rel == "" {
			// System / out-of-tree / build-dir: keep verbatim (incl. a
			// separate-pair value token).
			out = append(out, tok)
			continue
		}
		i += consumed
		if reanchor != nil {
			rel = reanchor(rel)
		}
		exec := rel
		if pkgPath != "" && pkgPath != "." {
			exec = pkgPath + "/" + rel
		}
		out = append(out, flag+exec)
		if !seenWalk[rel] {
			seenWalk[rel] = true
			walkDirs = append(walkDirs, rel)
		}
	}
	return out, walkDirs
}
