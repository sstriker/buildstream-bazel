package shadow

import (
	"sort"

	"github.com/sstriker/buildstream-bazel/internal/cmakeparse"
)

// cmakeFileIfIndex resolves the if-block stack active at a given
// (file, line) location by parsing CMakeLists.txt source bytes
// with cmakeparse. Replaces the trace-event-driven platformIfStack
// which relied on `endif` events that cmake's
// `--trace-format=json-v1` does NOT emit (endif is a structural
// delimiter, not a command). With this index every if-block's
// scope is derived from the source bytes, so no trace-event
// observation is needed to maintain stack balance.
//
// Limitation (out of scope for this round): cross-file if-context.
// When `add_subdirectory(d)` or `include(f)` sits inside an
// `if()` block, cmake's execution model carries the parent's
// open-if context into the child file's execution. cmakeparse
// processes each file independently, so the index reports
// only the if-blocks structurally present in the EVENT'S file.
// Projects whose platform conditionals sit at add_subdirectory
// boundaries (rare in practice; cmake's idiom is to gate at the
// declaration site instead) will miss attribution. The fallback
// is the same as the pre-PR-#268 behaviour — sources land in
// flat srcs, which Bazel accepts.
type cmakeFileIfIndex struct {
	// perFile[hostFilePath] is the flattened list of if-blocks
	// in the file, sorted by StartLine. Nested blocks appear
	// after their enclosing blocks.
	perFile map[string][]flatIfBlock
	// parseAttempted tracks files we've already tried to parse
	// so a single failure doesn't trigger repeated reads.
	parseAttempted map[string]bool
}

type flatIfBlock struct {
	StartLine int
	EndLine   int
	Arms      []flatIfArm
}

type flatIfArm struct {
	StartLine int
	EndLine   int
	// PredicateArgs is the if/elseif predicate's argument vector
	// pre-evaluation. Empty for else arms (their predicate is
	// implicit). The map to a Bazel constraint label goes through
	// selectKeyFromIfArgs at lookup time so unrecognized
	// predicates produce key "" and fall through.
	PredicateArgs []string
}

// newCmakeFileIfIndex initialises an empty index. Files get
// parsed lazily by lookupOrParse the first time activeStackAt
// asks for them.
func newCmakeFileIfIndex() *cmakeFileIfIndex {
	return &cmakeFileIfIndex{
		perFile:        map[string][]flatIfBlock{},
		parseAttempted: map[string]bool{},
	}
}

// lookupOrParse returns the indexed if-blocks for hostFilePath,
// parsing the file on first request. Returns nil + caches the
// negative result on parse / read failure so the caller still
// gets a clean lookup (which simply attributes nothing).
func (idx *cmakeFileIfIndex) lookupOrParse(hostFilePath string, fs fsReader) []flatIfBlock {
	if blocks, ok := idx.perFile[hostFilePath]; ok {
		return blocks
	}
	if idx.parseAttempted[hostFilePath] {
		return nil
	}
	idx.parseAttempted[hostFilePath] = true
	raw, err := fs.ReadFile(hostFilePath)
	if err != nil {
		return nil
	}
	nodes, err := cmakeparse.Parse(string(raw))
	if err != nil {
		return nil
	}
	var blocks []flatIfBlock
	walkIfBlocks(nodes, &blocks)
	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].StartLine < blocks[j].StartLine
	})
	idx.perFile[hostFilePath] = blocks
	return blocks
}

// walkIfBlocks recursively flattens cmakeparse if-blocks from a
// Node tree into a slice in outer-then-inner declaration order.
// Nested if-blocks (inside an arm's Body) are appended to the
// same out slice so the (file, line) → stack lookup can walk
// the flat list once.
func walkIfBlocks(nodes []cmakeparse.Node, out *[]flatIfBlock) {
	for _, n := range nodes {
		if n.If == nil {
			continue
		}
		blk := flatIfBlock{
			StartLine: n.If.StartLine,
			EndLine:   n.If.EndLine,
		}
		for _, arm := range n.If.Arms {
			blk.Arms = append(blk.Arms, flatIfArm{
				StartLine:     arm.StartLine,
				EndLine:       arm.EndLine,
				PredicateArgs: arm.PredicateArgs,
			})
			walkIfBlocks(arm.Body, out)
		}
		*out = append(*out, blk)
	}
}

// activeStackAt returns the RESOLVED select key of the active arm of
// each open if-block at (hostFilePath, line), in outer-to-inner order.
// Each entry is a platform constraint label, defaultSelectKey (a
// qualifying else — see resolveArmKey), or "" (unrecognized). Mirrors
// the runtime platformIfStack: the per-block resolution (including the
// all-siblings-recognized guard for else arms) happens here, where the
// full sibling arm list is available.
func (idx *cmakeFileIfIndex) activeStackAt(hostFilePath string, line int, fs fsReader) []string {
	blocks := idx.lookupOrParse(hostFilePath, fs)
	if len(blocks) == 0 {
		return nil
	}
	var stack []string
	for _, blk := range blocks {
		if line < blk.StartLine || line > blk.EndLine {
			continue
		}
		for armIdx, arm := range blk.Arms {
			if line >= arm.StartLine && line <= arm.EndLine {
				stack = append(stack, resolveArmKey(blk.Arms, armIdx))
				break
			}
		}
	}
	return stack
}

// resolveArmKey computes the select key for arm armIdx of a block.
// A recognized predicate arm maps to its constraint label. An else arm
// (empty PredicateArgs, last in the block) maps to defaultSelectKey
// ONLY when every prior arm (if + all elseif) was a recognized
// platform constraint — then "not any of them" is exactly the select's
// default. Mixed/unrecognized siblings, or an unrecognized predicate
// arm, yield "" (flat). Mirrors platformIfStack.observe's else logic.
func resolveArmKey(arms []flatIfArm, armIdx int) string {
	args := arms[armIdx].PredicateArgs
	if len(args) > 0 {
		return selectKeyFromIfArgs(args)
	}
	// Empty PredicateArgs = an else arm. Qualify only if it's the last
	// arm and all prior arms are recognized platform constraints.
	if armIdx != len(arms)-1 {
		return ""
	}
	for i := 0; i < armIdx; i++ {
		if selectKeyFromIfArgs(arms[i].PredicateArgs) == "" {
			return ""
		}
	}
	if armIdx == 0 {
		// A block whose only arm is else() — no siblings to negate;
		// not a platform conditional.
		return ""
	}
	return defaultSelectKey
}

// currentSelectKey returns the innermost resolved select key from the
// active if-stack at (hostFilePath, line), or "" when no arm maps to
// one. The innermost recognized (or qualifying-else) key wins.
func (idx *cmakeFileIfIndex) currentSelectKey(hostFilePath string, line int, fs fsReader) string {
	stack := idx.activeStackAt(hostFilePath, line, fs)
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] != "" {
			return stack[i]
		}
	}
	return ""
}
