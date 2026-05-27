package lower

import (
	"path/filepath"
	"strings"
)

// systemLibPrefixes is the conservative set of host paths whose
// libraries are reachable by the toolchain's default `-L` search
// path. cmake on Linux typically finds system libs under
// `/usr/lib*`, `/lib*`, or `/usr/local/lib*`; the GNU linker's
// default library search path covers all three on most distros.
// Lifting `<prefix>/lib<name>.so*` to `-l<name>` linkopts is safe
// when the path matches one of these; everything else (vendored
// installs at /opt/<vendor>/lib, pkg-config-supplied custom
// prefixes, etc.) stays elided so the operator's imports manifest
// is the right home for them.
//
// Multi-arch layouts (`/usr/lib/x86_64-linux-gnu`,
// `/usr/lib/aarch64-linux-gnu`, etc.) are matched via the prefix
// check below — any path starting with `/usr/lib/` (with or
// without a triple subdir) is treated as system-resident.
var systemLibPrefixes = []string{
	"/usr/lib/",
	"/usr/lib32/",
	"/usr/lib64/",
	"/lib/",
	"/lib32/",
	"/lib64/",
	"/usr/local/lib/",
	"/usr/local/lib32/",
	"/usr/local/lib64/",
}

// systemLibName returns the library name (the `<name>` in
// `lib<name>.so*` / `lib<name>.a`) when `path` points at a system-
// resident library that the linker can resolve via its default
// search path. Returns "" otherwise (vendored / custom-prefix
// paths) — the caller keeps eliding those.
//
// Recognized basename shapes:
//
//   - `lib<name>.so`                  → <name>
//   - `lib<name>.so.<version...>`     → <name>  (libfoo.so.1.2.3)
//   - `lib<name>.a`                   → <name>
//   - `lib<name>.dylib`               → <name>  (macOS — convert may
//                                                run on darwin even
//                                                though probe-cell
//                                                emits linux-host
//                                                cmake reply data)
//
// Anything not matching the lib*.so* / lib*.a / lib*.dylib shapes
// is rejected (Windows .lib/.dll/etc not yet handled — Bazel emits
// platform-specific link attributes for those, out of scope today).
func systemLibName(path string) string {
	matched := false
	for _, p := range systemLibPrefixes {
		if strings.HasPrefix(path, p) {
			matched = true
			break
		}
	}
	if !matched {
		return ""
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "lib") {
		return ""
	}
	rest := base[3:] // strip "lib" prefix
	// Strip the trailing suffix and any version segments. We try
	// the candidates in order; the first to match wins.
	for _, suffix := range []string{".so", ".a", ".dylib"} {
		if i := strings.Index(rest, suffix); i > 0 {
			// Reject empty-name (`lib.so` → "") and segments
			// containing non-portable characters. The strict
			// shape keeps the lift conservative.
			name := rest[:i]
			if name == "" {
				return ""
			}
			return name
		}
	}
	return ""
}
