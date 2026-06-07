package todos

import (
	"os"
	"strings"
)

// DefaultPreamble is the built-in operator preamble: the standing
// guidance the AI post-pass reads before working the todo list. It
// encodes this repo's transition-to-plain-Bazel intent and ships the
// brotli worked example. Operators override it with
// --conversion-todos-preamble=<file> (see LoadPreamble).
func DefaultPreamble() Preamble {
	return Preamble{
		Intent: "This project is being moved off cmake onto plain, idiomatic Bazel. " +
			"Author the target a Bazel maintainer would write by hand — a native " +
			"Bazel rule driving the *built artifact* — not a wrapper that re-invokes " +
			"`cmake -P` or shells out to the cmake harness. Prefer bazel_skylib " +
			"diff_test / sh_test over re-running cmake.",
		Environment: "Target Bazel 9 (the repo's pinned floor; see CONTRIBUTING.md). " +
			"Author with these rule providers, NOT native rules (Bazel 9 removed the " +
			"native sh rules and deprecated native cc): C/C++ via " +
			"`@rules_cc//cc:defs.bzl` (cc_binary/cc_library/cc_test); shell tests & " +
			"binaries via `@rules_shell//shell:sh_test.bzl` / `sh_binary.bzl` / " +
			"`sh_library.bzl`; file-comparison tests via " +
			"`@bazel_skylib//rules:diff_test.bzl`; install/packaging via " +
			"`@rules_pkg//pkg:mappings.bzl`. The converter already declares rules_cc, " +
			"bazel_skylib and rules_pkg as `bazel_dep`s in MODULE.bazel (read it for " +
			"the pinned versions); if you introduce a provider it doesn't list (e.g. " +
			"rules_shell for an sh_test), add the matching `bazel_dep`. Your authored " +
			"BUILD must be buildifier-canonical (`buildifier -mode=fix` is a no-op) " +
			"and survive a `gazelle` / `gazelle fix` roundtrip — the same gate the " +
			"converter's mechanical output meets (rule 4).",
		Rules: strings.Join([]string{
			"(1) Author into the designated authored-output file — never the " +
				"converter-owned BUILD.bazel.out nor the stage-b-derived BUILD.bazel " +
				"(the converter regenerates BUILD.bazel.out wholesale and stage-b " +
				"overwrites BUILD.bazel from it).",
			"(2) One reusable macro per shared unit, instantiated N times — not N " +
				"near-duplicate targets.",
			"(3) Preserve the recovered `verification` as the test's assertion.",
			"(4) Your output crosses the same trust boundary as mechanical output: " +
				"it must pass the render gates (buildifier -mode=diff no-op, gazelle " +
				"roundtrip, bazel build/test) — it is not trusted on faith.",
			"(5) Carry the construct's source comment onto the authored target " +
				"as a leading comment — read it at the todo's anchor site(s) (a " +
				"todo may fold several anchors into one unit; carry the comment " +
				"documenting the shared construct, not one per instance) — but " +
				"VALIDATE it, the same not-trusted-on-faith discipline as (4): " +
				"re-authoring changes the mechanics, so a comment describing the " +
				"old cmake form (e.g. running `cmake -P`) is stale. Rewrite it to " +
				"describe the authored Bazel target accurately, or drop it if it no " +
				"longer adds value; never carry a comment that misdescribes the target.",
			"(6) Each todo carries a `disposition` — the converter's BEST-GUESS, " +
				"FALLIBLE hint, not a gate. `actionable`: the converter produced no " +
				"faithful/working result, so the build is missing behavior until you " +
				"author the Bazel form — do these. `improvement`: the converter baked " +
				"a convert-time value (the build works but is frozen/non-faithful); an " +
				"author can replace it with a dynamic Bazel idiom. `informational`: " +
				"surfaced for visibility; usually skip. CRUCIALLY, you MAY upgrade an " +
				"`improvement` or `informational` entry to action when you see a " +
				"better Bazel form — the converter can't see what you can. In " +
				"particular, a baked option derived from a check/try_compile probe " +
				"(e.g. a frozen HAVE_X) often correlates 1:1 with a " +
				"platform/sysroot/toolchain: prefer a `config_setting` + `select()` " +
				"(or an operator-overridable `bool_flag`) over the frozen value.",
		}, " "),
		Example: "28 add_test(… COMMAND cmake -P run_test.cmake <input>) share one " +
			"runner whose contract is \"compress then decompress <input> with the " +
			"built brotli CLI and assert the result equals <input>.\" Author ONE " +
			"brotli_roundtrip_test macro wrapping a diff_test (or sh_test) over " +
			"//:brotli, then instantiate it over the input list — one prompt, one " +
			"macro, 28 cheap call sites.",
	}
}

// LoadPreamble returns the operator-supplied preamble read from path,
// or the built-in DefaultPreamble when path is empty. An override file
// is read verbatim as the whole preamble block (its bytes become Text),
// so operators write prose, not JSON.
func LoadPreamble(path string) (Preamble, error) {
	if path == "" {
		return DefaultPreamble(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Preamble{}, err
	}
	return Preamble{Text: strings.TrimRight(string(b), "\n")}, nil
}
