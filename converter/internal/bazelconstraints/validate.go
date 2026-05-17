// Package bazelconstraints validates the emit-ready IR against
// the small set of Bazel-side semantic constraints we've been
// bitten by — the bucket of "syntactically valid Bazel that
// `bazel build` rejects at analysis or action time."
//
// Scope is deliberately narrow. Each constraint is here because
// a real operator-reported bug landed in this family:
//
//   - issue #193: a CUSTOM_COMMAND with a no-op shell command
//     expanded to `cmd = ""` in the emitted genrule; Bazel
//     rejects with "declared output was not created by genrule".
//     Caught by ValidateGenruleCmd.
//   - issue #194: a transitive-dep-loop produced duplicate
//     entries in deps / implementation_deps; Bazel rejects with
//     "duplicate value in deps". Caught by validateNoDuplicates.
//   - general: a malformed rule name (whitespace, control chars,
//     empty) makes the BUILD.bazel un-loadable. Caught by
//     validateName.
//
// What this package deliberately doesn't do:
//
//   - Catch issue #192 (build-dir leakage into rule names). The
//     leaked-name shape is syntactically valid Bazel and only
//     non-deterministic across runs; it's not constrainable
//     statically. The fix in the name-derivation helper
//     (genruleNameFor) is the right place; we test it directly
//     in lower/genrule_internal_test.go.
//   - Become an "is this Bazel idiomatic?" inspector. New
//     constraints land here only when a real bug behind them
//     justifies the test-suite cost.
//   - Mirror Bazel's full label / target-name grammar. The
//     validateName regex is intentionally a conservative subset
//     covering the cases the lowering layer actually emits.
//
// All checks operate purely on the in-memory ir.Package — no
// filesystem, no Bazel invocation. ValidatePackage is the
// emitter's entry point; it walks every target and aggregates
// per-target errors via errors.Join so one bad target doesn't
// hide others in the same package.
package bazelconstraints

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// validNameRe accepts the conservative subset of Bazel target
// names the converter actually emits: ASCII letters, digits,
// underscore, dot, hyphen, plus. Bazel's full grammar allows
// more (slash, equals, comma, etc.), but the lowering layer
// never produces those — tightening here surfaces lowerer bugs
// that would otherwise ship as cryptic Bazel parse errors.
//
// Must start with a letter, digit, or underscore (Bazel forbids
// leading slash; we tighten further to also reject leading
// hyphen/dot to keep names unambiguous-looking).
var validNameRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.+\-]*$`)

// ValidatePackage runs every constraint check on every target in
// pkg, plus the package-level unique-name check. Returns nil if
// every check passes. On failure, returns a joined error
// carrying one entry per violation so callers (and tests) can
// see the full picture instead of fighting their way through
// fix-one-at-a-time.
func ValidatePackage(pkg *ir.Package) error {
	if pkg == nil {
		return nil
	}
	var errs []error
	seen := map[string]int{}
	for i, t := range pkg.Targets {
		if err := validateTarget(t); err != nil {
			errs = append(errs, fmt.Errorf("target %q (index %d): %w", t.Name, i, err))
		}
		if prev, ok := seen[t.Name]; ok {
			errs = append(errs, fmt.Errorf("duplicate target name %q at indexes %d and %d", t.Name, prev, i))
		} else {
			seen[t.Name] = i
		}
	}
	return errors.Join(errs...)
}

// validateTarget runs all per-target checks. Each check
// returns its own typed error; we join so a target with both
// a bad name AND empty cmd surfaces both in one validation pass.
func validateTarget(t ir.Target) error {
	var errs []error
	if err := validateName(t.Name); err != nil {
		errs = append(errs, err)
	}
	if t.Kind == ir.KindGenrule {
		if err := ValidateGenruleCmd(t.GenruleCmd); err != nil {
			errs = append(errs, err)
		}
	}
	if err := validateNoDuplicates(t.Deps, "deps"); err != nil {
		errs = append(errs, err)
	}
	if err := validateNoDuplicates(t.ImplementationDeps, "implementation_deps"); err != nil {
		errs = append(errs, err)
	}
	if err := validateNoDuplicates(t.Srcs, "srcs"); err != nil {
		errs = append(errs, err)
	}
	if err := validateNoDuplicates(t.Hdrs, "hdrs"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// validateName enforces validNameRe and rejects the empty
// string. Bazel itself rejects names with whitespace, control
// characters, and many other shapes; the regex is the
// converter's narrower contract — see the regex's comment.
func validateName(name string) error {
	if name == "" {
		return errors.New("target name is empty")
	}
	if !validNameRe.MatchString(name) {
		return fmt.Errorf("target name %q is not a valid Bazel identifier (allowed: [a-zA-Z0-9_][a-zA-Z0-9_.+-]*)", name)
	}
	return nil
}

// ValidateGenruleCmd catches the issue #193 shape: a genrule
// whose cmd is empty or whitespace-only. Bazel accepts the
// load-time syntax but rejects at action time with "declared
// output was not created by genrule" — the operator-facing
// failure mode the typed-error refusal in recoverGenrule was
// added to prevent. This validator is the second line of
// defense for any future code path that synthesizes a genrule
// without going through recoverGenrule's empty-cmd guard.
//
// Exported because lower/ may want to call it directly during
// synthesis instead of waiting for the package-level emit-time
// pass.
func ValidateGenruleCmd(cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return errors.New("genrule cmd is empty or whitespace-only (Bazel would reject `cmd = \"\"` at action time)")
	}
	return nil
}

// validateNoDuplicates catches the issue #194 shape: a deps or
// implementation_deps list with the same label appearing twice.
// Bazel rejects with "duplicate value <label> in attribute
// 'deps'" at load time. attr is the Bazel attribute name used
// in the error message so the operator sees which list to
// inspect (`deps` vs `implementation_deps` matter to the fix).
func validateNoDuplicates(slice []string, attr string) error {
	if len(slice) < 2 {
		return nil
	}
	seen := make(map[string]struct{}, len(slice))
	var dups []string
	for _, v := range slice {
		if _, ok := seen[v]; ok {
			dups = append(dups, v)
			continue
		}
		seen[v] = struct{}{}
	}
	if len(dups) == 0 {
		return nil
	}
	return fmt.Errorf("attribute %q has duplicate entries: %v", attr, dups)
}
