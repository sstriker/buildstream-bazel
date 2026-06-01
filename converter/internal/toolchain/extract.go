package toolchain

import (
	"fmt"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// FromReply turns one cmake File API reply into a Model. Pure
// function — unit tests can drive it from pre-recorded fixtures
// without invoking cmake.
//
// The reply must include cache-v2 and toolchains-v1 (codemodel-v2
// is helpful but not required at this layer; the probe project's
// targets are uninteresting from a derived-toolchain perspective).
func FromReply(r *fileapi.Reply) (*Model, error) {
	if r == nil {
		return nil, fmt.Errorf("toolchain.FromReply: nil reply")
	}
	if len(r.Toolchains.Toolchains) == 0 {
		return nil, fmt.Errorf("toolchain.FromReply: reply has no toolchains-v1 data")
	}
	m := &Model{
		HostPlatform: Platform{
			OS:  cacheValue(r.Cache, "CMAKE_HOST_SYSTEM_NAME"),
			CPU: cacheValue(r.Cache, "CMAKE_HOST_SYSTEM_PROCESSOR"),
		},
		TargetPlatform: Platform{
			OS:  cacheValueOr(r.Cache, "CMAKE_SYSTEM_NAME", cacheValue(r.Cache, "CMAKE_HOST_SYSTEM_NAME")),
			CPU: cacheValueOr(r.Cache, "CMAKE_SYSTEM_PROCESSOR", cacheValue(r.Cache, "CMAKE_HOST_SYSTEM_PROCESSOR")),
		},
		BuildType: cacheValue(r.Cache, "CMAKE_BUILD_TYPE"),
		Languages: map[string]Language{},
		Tools: Tools{
			AR:      cacheValue(r.Cache, "CMAKE_AR"),
			Ranlib:  cacheValue(r.Cache, "CMAKE_RANLIB"),
			Strip:   cacheValue(r.Cache, "CMAKE_STRIP"),
			NM:      cacheValue(r.Cache, "CMAKE_NM"),
			Objcopy: cacheValue(r.Cache, "CMAKE_OBJCOPY"),
			Objdump: cacheValue(r.Cache, "CMAKE_OBJDUMP"),
			Linker:  cacheValue(r.Cache, "CMAKE_LINKER"),
		},
	}
	for _, ent := range r.Toolchains.Toolchains {
		lang := Language{
			CompilerID:           ent.Compiler.Id,
			CompilerPath:         ent.Compiler.Path,
			Version:              ent.Compiler.Version,
			Target:               ent.Compiler.Target,
			BuiltinIncludeDirs:   append([]string(nil), ent.Compiler.Implicit.IncludeDirectories...),
			BuiltinLinkDirs:      append([]string(nil), ent.Compiler.Implicit.LinkDirectories...),
			SourceFileExtensions: append([]string(nil), ent.SourceFileExtensions...),
			BaseFlags:            tokenizeCacheFlags(r.Cache, "CMAKE_"+ent.Language+"_FLAGS"),
		}
		if m.BuildType != "" {
			lang.BuildTypeFlags = tokenizeCacheFlags(r.Cache,
				"CMAKE_"+ent.Language+"_FLAGS_"+strings.ToUpper(m.BuildType),
			)
			lang.LinkBuildTypeFlags = mergeFlags(
				tokenizeCacheFlags(r.Cache, "CMAKE_EXE_LINKER_FLAGS_"+strings.ToUpper(m.BuildType)),
				tokenizeCacheFlags(r.Cache, "CMAKE_SHARED_LINKER_FLAGS_"+strings.ToUpper(m.BuildType)),
			)
		}
		lang.LinkFlags = tokenizeCacheFlags(r.Cache, "CMAKE_"+ent.Language+"_LINK_FLAGS")
		m.Languages[ent.Language] = lang
	}
	return m, nil
}

// cacheValue returns the value of a cache entry, or "" when missing.
func cacheValue(c fileapi.Cache, name string) string {
	if e := c.Get(name); e != nil {
		return e.Value
	}
	return ""
}

// cacheValueOr returns the value of a cache entry, falling back to
// dflt when the entry is missing OR has an empty value.
func cacheValueOr(c fileapi.Cache, name, dflt string) string {
	if v := cacheValue(c, name); v != "" {
		return v
	}
	return dflt
}

// tokenizeCacheFlags splits a CMAKE_<LANG>_FLAGS-style cache value
// into individual flags. cmake stores them as a single space-
// separated string; we tokenize on whitespace, ignoring empty
// fragments.
//
// Quoted arguments (rare in CMAKE_*_FLAGS) are NOT preserved as
// single tokens — the de-facto convention is that compile flags
// don't contain spaces, and any project that does is already
// outside cmake's official portability story. Document and revisit
// if a real project breaks.
func tokenizeCacheFlags(c fileapi.Cache, name string) []string {
	v := cacheValue(c, name)
	if v == "" {
		return nil
	}
	return strings.Fields(v)
}

// mergeFlags concatenates two slices and dedupes preserving order.
func mergeFlags(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, x := range a {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	for _, x := range b {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// stripDuplicates returns items minus anything in exclude,
// preserving order. Used by Observe to compute per-variant
// deltas as "this variant's flags - baseline flags".
func stripDuplicates(items []string, exclude map[string]bool) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, x := range items {
		if !exclude[x] {
			out = append(out, x)
		}
	}
	return out
}
