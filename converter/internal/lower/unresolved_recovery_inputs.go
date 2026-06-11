package lower

import (
	"fmt"
	"sort"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
)

// Unresolved configure-time recovery inputs.
//
// The configure-time recovery family (configure_file outputs, proto import
// closures, nested-build generated headers) occasionally hits an input it
// EXPECTED to resolve but couldn't: a relative configure_file output whose
// issuing scope is outside the codemodel directory tree (so it anchors to no
// build location), or a declared source / on-disk header that fails to read
// mid-recovery. Each is an UNCERTAIN drop — a file a consumer may #include,
// lost without a trace — not a 100%-confident no-op, so per the
// no-silent-drops contract it must be surfaced rather than skipped silently.
//
// This is the cross-family sibling of cc.UnreadableConfigureOutputs (which
// already surfaces the configure_file LIVE read-failure case): one shared
// channel — an actionable conversion-todo per kind plus a stderr breadcrumb —
// for the recovery skips that were previously silent `continue`s.

const (
	// unresolvedConfigureFileUnanchored: a relative configure_file output that
	// anchored to no codemodel directory scope and whose issuing file isn't
	// under the source tree (e.g. a generate_export_header with a relative
	// output, issued from cmake's own module dir).
	unresolvedConfigureFileUnanchored = "configure-file-unanchored"
	// unresolvedProtoImportUnreadable: a declared .proto src that couldn't be
	// read while walking its transitive import closure, so its imports may be
	// absent from the genrule srcs.
	unresolvedProtoImportUnreadable = "proto-import-unreadable"
	// unresolvedNestedHeaderUnreadable: a nested-build configure-generated
	// header found on disk but unreadable, so it wasn't baked for the outer
	// consumers that prefix-match it.
	unresolvedNestedHeaderUnreadable = "nested-header-unreadable"
)

// unresolvedRecoveryInput records one recovery input the converter expected to
// resolve but couldn't. Ref is a leak-safe identifier — a source- or
// build-relative path / operand, never an absolute (per-run / machine) path —
// so the report stays byte-identical across runs.
type unresolvedRecoveryInput struct {
	Kind string
	Ref  string
}

// noteUnresolvedRecoveryInput records one unresolved recovery input. Nil-safe
// (test call sites pass a nil cc); duplicates fold in the producer.
func (cc *codegenContext) noteUnresolvedRecoveryInput(kind, ref string) {
	if cc == nil {
		return
	}
	cc.UnresolvedRecoveryInputs = append(cc.UnresolvedRecoveryInputs, unresolvedRecoveryInput{Kind: kind, Ref: ref})
}

// warnUnresolvedRecoveryInputs surfaces the unresolved recovery inputs — a
// loud stderr breadcrumb plus structured conversion-todos. No-op when empty.
func warnUnresolvedRecoveryInputs(opts Options, cc *codegenContext) {
	if len(cc.UnresolvedRecoveryInputs) == 0 {
		return
	}
	if opts.Warnings != nil {
		fmt.Fprintf(opts.Warnings,
			"lower: %d configure-time recovery input(s) couldn't be resolved (an unanchorable configure_file output or an unreadable source/header) — surfaced as conversion-todos rather than dropped silently\n",
			len(cc.UnresolvedRecoveryInputs))
	}
	emitUnresolvedRecoveryInputTodos(opts.Todos, cc.UnresolvedRecoveryInputs)
}

// emitUnresolvedRecoveryInputTodos mirrors the unresolved recovery inputs into
// structured conversion-todos, one per kind (the unit an author re-works).
// Refs dedupe and sort so the report is deterministic; the Ref is leak-safe by
// construction so no path normalization is needed.
func emitUnresolvedRecoveryInputTodos(c *todos.Collector, inputs []unresolvedRecoveryInput) {
	if c == nil || len(inputs) == 0 {
		return
	}
	byKind := map[string]map[string]bool{}
	for _, in := range inputs {
		if byKind[in.Kind] == nil {
			byKind[in.Kind] = map[string]bool{}
		}
		byKind[in.Kind][in.Ref] = true
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		refs := make([]string, 0, len(byKind[kind]))
		for r := range byKind[kind] {
			refs = append(refs, r)
		}
		sort.Strings(refs)
		anchors := make([]todos.Anchor, 0, len(refs))
		for _, r := range refs {
			anchors = append(anchors, todos.Anchor{Construct: unresolvedRecoveryConstruct(kind) + ": " + r})
		}
		c.Add(todos.Todo{
			Kind:           "unresolved-recovery-input",
			Disposition:    todos.Actionable,
			GroupKey:       kind,
			Anchors:        anchors,
			Evidence:       map[string]any{"reason": kind, "refs": refs},
			SuggestedShape: unresolvedRecoveryShape(kind),
			Prompt: fmt.Sprintf("The converter couldn't resolve %s during configure-time recovery (%s). "+
				"A consumer that depends on the resulting file(s) will fail to build. Confirm none is "+
				"needed, or author the idiomatic Bazel form.", plural(len(refs), "input"), kind),
		})
	}
}

// unresolvedRecoveryConstruct renders the per-kind anchor-construct prefix.
func unresolvedRecoveryConstruct(kind string) string {
	switch kind {
	case unresolvedConfigureFileUnanchored:
		return "configure_file output"
	case unresolvedProtoImportUnreadable:
		return "proto src"
	case unresolvedNestedHeaderUnreadable:
		return "nested header"
	default:
		return kind
	}
}

// unresolvedRecoveryShape renders the per-kind SuggestedShape hint.
func unresolvedRecoveryShape(kind string) string {
	switch kind {
	case unresolvedConfigureFileUnanchored:
		return "the configure_file's output couldn't be placed in the build tree (its issuing scope is " +
			"outside the codemodel directory tree — e.g. a generate_export_header with a relative output) — " +
			"supply the configure_file's Bazel form (a //tools cmake-configure-file genrule / a write_file) " +
			"so the consumer's #include resolves"
	case unresolvedProtoImportUnreadable:
		return "a declared .proto couldn't be read while discovering its imports — confirm the file is " +
			"present and readable, or add the missing imported protos to the proto genrule's srcs"
	case unresolvedNestedHeaderUnreadable:
		return "a nested-build configure-generated header was found on disk but couldn't be read — re-run " +
			"the convert with the nested build dir readable, or supply the header's Bazel form"
	default:
		return "confirm the input isn't needed, or author the idiomatic Bazel form"
	}
}
