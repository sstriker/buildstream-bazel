// Package toolchain derives Bazel cc_toolchain / platform definitions
// from cmake File API output. The flow is:
//
//  1. Probe: run `cmake -S probe -B build` once per variant (build
//     type, optional toolchain file).
//  2. Extract: turn each File API reply into a typed Model.
//  3. Diff: compare Models across variants to derive per-build-type
//     flag deltas.
//  4. Emit (separate package): render Model + variants to Bazel
//     cc_toolchain_config / cc_toolchain / platform / toolchain rules.
//
// The extract step is a pure function over fileapi.Reply, so unit
// tests run without invoking cmake.
package toolchain

// Model is what one cmake configure tells us about the toolchain.
// One Model corresponds to one variant (build type + toolchain file
// combination); the variant matrix yields N Models that we diff in
// `diff.go`.
type Model struct {
	// HostPlatform reflects CMAKE_HOST_SYSTEM_NAME / _PROCESSOR — the
	// machine cmake itself ran on.
	HostPlatform Platform

	// TargetPlatform reflects CMAKE_SYSTEM_NAME / _PROCESSOR — same as
	// HostPlatform when not cross-compiling (no CMAKE_TOOLCHAIN_FILE).
	TargetPlatform Platform

	// BuildType is CMAKE_BUILD_TYPE: "Debug", "Release", "RelWithDebInfo",
	// "MinSizeRel", or "" when not set.
	BuildType string

	// Sysroot is CMAKE_SYSROOT — the compile-time sysroot a (cross)
	// toolchain file pins. "" for a host build that takes the compiler's
	// default sysroot. Emitted as the cc_toolchain_config builtin_sysroot
	// so Bazel passes --sysroot= to compile + link actions.
	Sysroot string

	// Languages is keyed by CMake language name ("C", "CXX", ...).
	// We populate at minimum C + CXX when the probe project exercises
	// both.
	Languages map[string]Language

	// Tools collects the binutils-class tools cmake located. Each
	// field is the absolute path or "" if cmake didn't find / set it.
	Tools Tools
}

// Platform is the (OS, CPU) pair Bazel `platform` rules carry as
// constraint_values. We don't model libc here — that requires a
// separate heuristic at emit time.
type Platform struct {
	OS  string // "Linux", "Darwin", "Windows"
	CPU string // "x86_64", "aarch64", ...
}

// Language is everything we need to render one (language,
// build_type) tuple's contribution to a cc_toolchain_config.
type Language struct {
	// Identifying info. CompilerID is cmake's normalized vendor name
	// ("GNU", "Clang", "MSVC", ...).
	CompilerID   string
	CompilerPath string
	Version      string
	Target       string // e.g. "x86_64-linux-gnu"; "" if cmake didn't set it

	// Built-in include / link search paths the compiler adds without
	// being told. Bazel needs these so cc_library consumers can
	// resolve <stdio.h> etc.
	BuiltinIncludeDirs []string
	BuiltinLinkDirs    []string

	// SourceFileExtensions lists the extensions cmake recognizes for
	// this language ([".c", ".m"] for C; [".cpp", ".cxx", ...] for
	// CXX). Used when emitting Bazel feature gating per source kind.
	SourceFileExtensions []string

	// BaseFlags is CMAKE_<LANG>_FLAGS — applies to every compile
	// unit regardless of build type. Tokenized.
	BaseFlags []string

	// BuildTypeFlags is CMAKE_<LANG>_FLAGS_<BUILD_TYPE>; only
	// populated when BuildType != "". Tokenized.
	BuildTypeFlags []string

	// LinkFlags is CMAKE_<LANG>_LINK_FLAGS (rare; usually empty).
	LinkFlags []string
	// LinkBuildTypeFlags is CMAKE_EXE_LINKER_FLAGS_<BUILD_TYPE> +
	// CMAKE_SHARED_LINKER_FLAGS_<BUILD_TYPE>. Bazel doesn't
	// distinguish exe vs shared at toolchain-config level; we
	// concatenate dedup'd.
	LinkBuildTypeFlags []string
}

// Tools collects binutils-style tool paths. Empty string means
// cmake didn't set the variable; the emitter falls back to PATH
// names ("ar", "ld", ...) in that case.
type Tools struct {
	AR      string
	Ranlib  string
	Strip   string
	NM      string
	Objcopy string
	Objdump string
	Linker  string
}
