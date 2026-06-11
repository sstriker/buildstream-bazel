package bazel

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// largeSplitPackage builds an n-package split (one cc_library per dir) with
// enough attributes that each per-package build.Format does real work — the
// shape EmitSplit's per-package render concurrency targets.
func largeSplitPackage(n int) *ir.Package {
	targets := make([]ir.Target, n)
	subs := make(map[string]string, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("lib%d", i)
		dir := fmt.Sprintf("src/mod%d", i)
		targets[i] = ir.Target{
			Name:       name,
			Kind:       ir.KindCCLibrary,
			Srcs:       []string{dir + "/a.cc", dir + "/b.cc", dir + "/c.cc"},
			Hdrs:       []string{dir + "/a.h", dir + "/b.h"},
			Deps:       []string{":dep1", ":dep2", "@ext//pkg:lib"},
			Copts:      []string{"-Wall", "-Wextra", "-O2"},
			Defines:    []string{"FOO=1", "BAR=2"},
			Visibility: []string{"//visibility:public"},
		}
		subs[name] = dir
	}
	return &ir.Package{Name: "p", Targets: targets, SubPackages: subs}
}

// BenchmarkEmitSplit measures the per-package render concurrency: workers=1 is
// the old sequential path, workers=NumCPU the parallel one. The wall-clock
// (ns/op) ratio is the speedup parallelism buys on a big multi-package convert.
func BenchmarkEmitSplit(b *testing.B) {
	pkg := largeSplitPackage(400)
	for _, w := range []int{1, runtime.NumCPU()} {
		b.Run(fmt.Sprintf("workers%d", w), func(b *testing.B) {
			old := splitEmitWorkers
			splitEmitWorkers = w
			defer func() { splitEmitWorkers = old }()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := EmitSplit(pkg, Options{BazelPackagePath: "elements/p"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
