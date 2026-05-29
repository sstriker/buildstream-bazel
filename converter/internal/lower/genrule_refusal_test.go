package lower

import (
	"errors"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestRecoverGenrule_RefusalBranches covers the typed-error
// refusal contract of (*codegenContext).recoverGenrule (Batch
// B of the test-coverage plan in PR #199). The plan's stated
// goal: 100% coverage of the function's refusal paths, since
// the refusal taxonomy is operator-facing (drives the audit-
// tag classification the orchestrator dedupes failure logs
// on — see docs/failure-schema.md).
//
// Each row asserts:
//   - the typed *failure.Error fires with the expected Code,
//   - a per-branch message-substring sanity-check (so we know
//     we hit the intended branch, not just a same-code one),
//   - no genrule was synthesized on cc.Genrules,
//   - no entry was added to cc.OutToGenrule or cc.SeenBuilds.
func TestRecoverGenrule_RefusalBranches(t *testing.T) {
	const (
		buildDir = "/tmp/build"
		cmakeSrc = "/src/project"
	)

	// graphWithCustom is the "happy" graph the bare-refusal
	// branches don't need but the cmd-resolve-fail / script-
	// mode rows do.
	withRule := func(ruleName string, b *ninja.Build) *ninja.Graph {
		g := &ninja.Graph{
			Vars:  map[string]string{},
			Rules: map[string]*ninja.Rule{},
			Pools: map[string]*ninja.Pool{},
		}
		g.Rules[ruleName] = &ninja.Rule{
			Name: ruleName,
			Bindings: map[string]string{
				"command": "$COMMAND",
			},
			BindingOrder: []string{"command"},
		}
		g.Builds = []*ninja.Build{b}
		return g
	}

	cases := []struct {
		name       string
		srcPath    string
		graph      *ninja.Graph
		wantCode   failure.Code
		wantSubstr string // substring of the typed error's Message
	}{
		{
			name:       "srcPath outside buildDir",
			srcPath:    "/some/other/dir/foo.h",
			graph:      nil, // unused — guarded before the graph check
			wantCode:   failure.UnsupportedCustomCommand,
			wantSubstr: "outside the build dir",
		},
		{
			name:       "no ninja graph available",
			srcPath:    buildDir + "/gen/foo.h",
			graph:      nil,
			wantCode:   failure.UnsupportedCustomCommand,
			wantSubstr: "no cmake build graph (build.ninja) was available",
		},
		{
			name:    "no build statement produces the output",
			srcPath: buildDir + "/gen/missing.h",
			graph: withRule("CUSTOM_COMMAND", &ninja.Build{
				// Produces something DIFFERENT from what we ask
				// for, so BuildFor(relOut) and BuildFor(srcPath)
				// both miss.
				Outputs: []string{"gen/other.h"},
				Rule:    "CUSTOM_COMMAND",
			}),
			wantCode:   failure.UnsupportedCustomCommand,
			wantSubstr: "no ninja build statement produces",
		},
		{
			name:    "producing rule is not CUSTOM_COMMAND",
			srcPath: buildDir + "/gen/foo.o",
			graph: withRule("CXX_COMPILER__foo", &ninja.Build{
				Outputs: []string{"gen/foo.o"},
				Rule:    "CXX_COMPILER__foo",
			}),
			wantCode:   failure.UnsupportedCustomCommand,
			wantSubstr: `produced by rule "CXX_COMPILER__foo"`,
		},
		{
			name:    "CommandFor cannot resolve the command",
			srcPath: buildDir + "/gen/foo.h",
			// CommandFor returns !ok when the referenced rule
			// has no `command` binding. Construct a graph where
			// the rule exists by name but the binding is missing.
			graph: func() *ninja.Graph {
				g := &ninja.Graph{
					Vars:  map[string]string{},
					Rules: map[string]*ninja.Rule{},
					Pools: map[string]*ninja.Pool{},
				}
				g.Rules["CUSTOM_COMMAND"] = &ninja.Rule{
					Name:     "CUSTOM_COMMAND",
					Bindings: map[string]string{}, // no `command`
				}
				g.Builds = []*ninja.Build{{
					Outputs: []string{"gen/foo.h"},
					Rule:    "CUSTOM_COMMAND",
				}}
				return g
			}(),
			wantCode:   failure.UnsupportedCustomCommand,
			wantSubstr: "could not resolve command",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc := newCodegenContext()
			relOut, name, err := cc.recoverGenrule(tc.srcPath, cmakeSrc, buildDir, tc.graph)
			if err == nil {
				t.Fatalf("recoverGenrule succeeded (relOut=%q, name=%q); want refusal", relOut, name)
			}
			if relOut != "" || name != "" {
				t.Errorf("refusal returned non-empty relOut/name: %q / %q", relOut, name)
			}
			var fe *failure.Error
			if !errors.As(err, &fe) {
				t.Fatalf("err is not *failure.Error: %T (%v)", err, err)
			}
			if fe.Code != tc.wantCode {
				t.Errorf("err.Code = %q, want %q", fe.Code, tc.wantCode)
			}
			if !strings.Contains(fe.Message, tc.wantSubstr) {
				t.Errorf("err.Message %q does not contain %q", fe.Message, tc.wantSubstr)
			}
			// Refusal contract: no IR synthesis on any failure.
			if len(cc.Genrules) != 0 {
				t.Errorf("refusal synthesized %d Genrules; want 0", len(cc.Genrules))
			}
			if len(cc.OutToGenrule) != 0 {
				t.Errorf("refusal populated OutToGenrule: %v; want empty", cc.OutToGenrule)
			}
			if len(cc.SeenBuilds) != 0 {
				t.Errorf("refusal populated SeenBuilds: %d entries; want 0", len(cc.SeenBuilds))
			}
		})
	}
}

// TestRecoverGenrule_NoGraph_ActionableHint locks in the #215
// diagnostic: when a target references a generated source but the
// converter has no build graph (it ran without --source-root /
// --cmake-build-dir, the common configure_file() case), the refusal
// must name the fix rather than emit an opaque "no build.ninja"
// message. Asserts both the configure_file framing and the concrete
// flags, so a future message edit that drops the actionable hint
// fails loudly.
func TestRecoverGenrule_NoGraph_ActionableHint(t *testing.T) {
	cc := newCodegenContext()
	_, _, err := cc.recoverGenrule("/tmp/build/gen/config.h", "/src/project", "/tmp/build", nil)
	if err == nil {
		t.Fatal("recoverGenrule with nil graph succeeded; want refusal")
	}
	for _, want := range []string{"configure_file()", "--source-root", "--cmake-build-dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q does not mention %q (the #215 actionable hint)", err.Error(), want)
		}
	}
}

// TestRecoverGenrule_CmakeScriptModeRefusal is split out because
// it returns a DIFFERENT typed Code (UnsupportedCustomCommandScript)
// than the rest of the refusal branches and carries its own
// rewrite-in-real-language guidance — operator-facing in a way
// the generic UnsupportedCustomCommand isn't. Worth its own row
// so a future refactor that folds it into the generic code would
// fail loudly.
func TestRecoverGenrule_CmakeScriptModeRefusal(t *testing.T) {
	const (
		buildDir = "/tmp/build"
		cmakeSrc = "/src/project"
	)

	g := &ninja.Graph{
		Vars:  map[string]string{},
		Rules: map[string]*ninja.Rule{},
		Pools: map[string]*ninja.Pool{},
	}
	g.Rules["CUSTOM_COMMAND"] = &ninja.Rule{
		Name: "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"command": "$COMMAND",
		},
		BindingOrder: []string{"command"},
	}
	g.Builds = []*ninja.Build{{
		Outputs: []string{"gen/script_out.h"},
		Rule:    "CUSTOM_COMMAND",
		// CommandFor expands $COMMAND from the build statement's
		// own bindings — same path cmake-emitted custom commands
		// use.
		Bindings: map[string]string{
			"COMMAND": "/usr/bin/cmake -DFOO=bar -P /src/scripts/gen.cmake",
		},
		BindingOrder: []string{"COMMAND"},
	}}

	cc := newCodegenContext()
	relOut, name, err := cc.recoverGenrule(buildDir+"/gen/script_out.h", cmakeSrc, buildDir, g)
	if err == nil {
		t.Fatalf("recoverGenrule succeeded (relOut=%q, name=%q); want script-mode refusal", relOut, name)
	}
	var fe *failure.Error
	if !errors.As(err, &fe) {
		t.Fatalf("err is not *failure.Error: %T (%v)", err, err)
	}
	if fe.Code != failure.UnsupportedCustomCommandScript {
		t.Errorf("err.Code = %q, want %q", fe.Code, failure.UnsupportedCustomCommandScript)
	}
	if !strings.Contains(fe.Message, "rewrite the script in a real language") {
		t.Errorf("err.Message %q is missing the rewrite-guidance substring", fe.Message)
	}
	// Improved message (#207) names the actual -P script so
	// operators see which file to rewrite — not just the
	// consuming target's output.
	if !strings.Contains(fe.Message, "/src/scripts/gen.cmake") {
		t.Errorf("err.Message %q is missing the cmake -P script path", fe.Message)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("script-mode refusal synthesized %d Genrules; want 0", len(cc.Genrules))
	}
}

// TestRecoverGenrule_DedupesSeenBuilds locks the "already
// recovered? Reuse." path between the rule-check and the
// CommandFor call. Two recoverGenrule calls for the SAME ninja
// build (different output paths from the same statement, or
// the same path requested twice) must yield ONE genrule with
// the same name. Without dedup, codegen-tag emission would
// double-count and BUILD.bazel would carry two identical
// genrules.
func TestRecoverGenrule_DedupesSeenBuilds(t *testing.T) {
	const (
		buildDir = "/tmp/build"
		cmakeSrc = "/src/project"
	)

	g := &ninja.Graph{
		Vars:  map[string]string{},
		Rules: map[string]*ninja.Rule{},
		Pools: map[string]*ninja.Pool{},
	}
	g.Rules["CUSTOM_COMMAND"] = &ninja.Rule{
		Name: "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"command": "$COMMAND",
		},
		BindingOrder: []string{"command"},
	}
	g.Builds = []*ninja.Build{{
		Outputs: []string{"gen/foo.h", "gen/foo.cc"},
		Rule:    "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"COMMAND": "/usr/bin/python3 /src/scripts/gen.py",
		},
		BindingOrder: []string{"COMMAND"},
	}}

	cc := newCodegenContext()
	_, name1, err1 := cc.recoverGenrule(buildDir+"/gen/foo.h", cmakeSrc, buildDir, g)
	if err1 != nil {
		t.Fatalf("first recoverGenrule failed: %v", err1)
	}
	_, name2, err2 := cc.recoverGenrule(buildDir+"/gen/foo.cc", cmakeSrc, buildDir, g)
	if err2 != nil {
		t.Fatalf("second recoverGenrule failed: %v", err2)
	}
	if name1 != name2 {
		t.Errorf("rule names diverge across calls for same build: %q vs %q", name1, name2)
	}
	if got := len(cc.Genrules); got != 1 {
		t.Errorf("dedup failed: %d Genrules synthesized for one build; want 1", got)
	}
}
