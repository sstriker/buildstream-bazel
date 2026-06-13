package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func TestEmitDownloadLiftTodos(t *testing.T) {
	c := todos.New()
	emitDownloadLiftTodos(c, "", []downloadLiftRecord{
		{Rel: "dl_config.h", URL: "https://example.com/config.h", Hash: "SHA256=abc123"},
		{Rel: "vendor/lib.tar", URL: "https://example.com/lib.tar", Hash: ""},
	})
	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 2 {
		t.Fatalf("expected 2 download todos, got %d", len(rep.Todos))
	}
	var withHash todos.Todo
	for _, td := range rep.Todos {
		if td.Kind != "download" || td.Disposition != todos.Improvement {
			t.Errorf("kind/disp = %q/%q; want download/improvement", td.Kind, td.Disposition)
		}
		if td.GroupKey == "dl_config.h" {
			withHash = td
		}
	}
	st := withHash.SuggestedShape
	// SHA256=abc123 → SRI integrity (sha256-<base64(0xab 0xc1 0x23)>), not the
	// raw hex sha256 attr.
	for _, want := range []string{`http_file(`, `name = "dl_dl_config_h"`, `urls = ["https://example.com/config.h"]`, `integrity = "sha256-q8Ej"`, `@dl_dl_config_h//file`} {
		if !strings.Contains(st, want) {
			t.Errorf("stanza missing %q:\n%s", want, st)
		}
	}
}

func TestDownloadRepoName(t *testing.T) {
	// Standalone (root/empty package path) keeps the bare dl_<rel> form
	// (gate-stable). A non-root package path namespaces the name so a
	// multi-element envelope where two elements both write config.h doesn't
	// collide in project B's project-wide repo namespace.
	cases := []struct{ pkg, rel, want string }{
		{"", "config.h", "dl_config_h"},
		{".", "config.h", "dl_config_h"},
		{"elements/foo", "config.h", "dl_elements_foo_config_h"},
		{"elements/bar", "config.h", "dl_elements_bar_config_h"},
		{"elements/foo", "sub/dir/gen.h", "dl_elements_foo_sub_dir_gen_h"},
	}
	for _, c := range cases {
		if got := downloadRepoName(c.pkg, c.rel); got != c.want {
			t.Errorf("downloadRepoName(%q, %q) = %q; want %q", c.pkg, c.rel, got, c.want)
		}
	}
	// Same rel, distinct elements → distinct names (the collision the
	// namespacing exists to prevent).
	if downloadRepoName("elements/foo", "config.h") == downloadRepoName("elements/bar", "config.h") {
		t.Error("distinct elements with the same output rel must get distinct repo names")
	}
}

func TestDownloadIntegritySRI(t *testing.T) {
	// hex "abc123" = bytes 0xab 0xc1 0x23 → base64 "q8Ej". The transform is
	// mechanical, so SHA384/SHA512 resolve to a real integrity too (not just
	// SHA256) — closing the earlier SHA256-only gap.
	cases := []struct {
		hash   string
		want   string
		wantOK bool
	}{
		{"SHA256=abc123", "sha256-q8Ej", true},
		{"SHA384=abc123", "sha384-q8Ej", true},
		{"SHA512=abc123", "sha512-q8Ej", true},
		{"sha256=abc123", "sha256-q8Ej", true}, // case-insensitive algo
		{"MD5=abc123", "", false},              // no SRI form
		{"SHA1=abc123", "", false},             // no SRI form
		{"SHA256=nothex", "", false},           // malformed hex declines
		{"nohash", "", false},                  // no "=" declines
	}
	for _, c := range cases {
		got, ok := downloadIntegritySRI(c.hash)
		if ok != c.wantOK || got != c.want {
			t.Errorf("downloadIntegritySRI(%q) = %q/%v; want %q/%v", c.hash, got, ok, c.want, c.wantOK)
		}
	}
}

func TestDownloadRepoSpecs(t *testing.T) {
	specs := downloadRepoSpecs("", []downloadLiftRecord{
		{Rel: "vendor/lib.tar", URL: "https://example.com/lib.tar", Hash: ""},
		{Rel: "config.h", URL: "https://example.com/config.h", Hash: "SHA256=abc123"},
	})
	if len(specs) != 2 {
		t.Fatalf("want 2 specs, got %d: %+v", len(specs), specs)
	}
	// Sorted by rel: "config.h" before "vendor/lib.tar".
	if specs[0].Rel != "config.h" || specs[1].Rel != "vendor/lib.tar" {
		t.Errorf("specs not sorted by rel: %+v", specs)
	}
	if specs[0].Repo != "dl_config_h" || specs[0].Integrity != "sha256-q8Ej" || specs[0].DownloadedFilePath != "config.h" {
		t.Errorf("config.h spec = %+v", specs[0])
	}
	// No EXPECTED_HASH → integrity omitted (empty).
	if specs[1].Integrity != "" || specs[1].DownloadedFilePath != "lib.tar" {
		t.Errorf("lib.tar spec = %+v; want empty integrity + downloaded_file_path lib.tar", specs[1])
	}
}

// TestBakeBuildDirFile_LiftDownload pins the producer rewiring: with
// --lift-download a recovered file(DOWNLOAD) becomes a fetch genrule
// (@<repo>//file → <rel>) registered in OutToGenrule like any producer,
// works OFFLINE (no live build dir / no on-disk bytes — the fetch is
// trace-derived), and records the lockfile spec. Without the flag and with
// no bytes on disk, the same call declines — proving the flag is what
// enables the bytes-free path.
func TestBakeBuildDirFile_LiftDownload(t *testing.T) {
	mkLC := func(lift bool) targetLowerCtx {
		cc := newCodegenContext()
		cc.LiftDownload = lift
		cc.FileWriterIndex = buildFileWriterIndex([]shadow.FileWriterCall{
			{Op: "download", URL: "https://example.com/dl.h", Outputs: []string{"/b/dl.h"}},
		}, "/b")
		// hostBuild "" → offline: no on-disk bytes available.
		return targetLowerCtx{cc: cc, cmakeSrc: "/s", cmakeBuild: "/b"}
	}

	t.Run("lifts-offline-to-fetch-genrule", func(t *testing.T) {
		lc := mkLC(true)
		name, ok := bakeBuildDirFile("dl.h", lc)
		if !ok {
			t.Fatal("--lift-download must produce a fetch genrule with no live build dir")
		}
		if lc.cc.OutToGenrule["dl.h"] != name {
			t.Errorf("producer registration: %+v", lc.cc.OutToGenrule)
		}
		g := lc.cc.Genrules[0]
		if len(g.Srcs) != 1 || g.Srcs[0] != "@dl_dl_h//file" {
			t.Errorf("fetch srcs = %v; want [@dl_dl_h//file]", g.Srcs)
		}
		if g.GenruleOuts[0] != "dl.h" {
			t.Errorf("outs = %v; want [dl.h]", g.GenruleOuts)
		}
		if !stringSliceContains(g.Tags, "cmake-codegen-download-fetch") {
			t.Errorf("tags = %v; want the download-fetch facet", g.Tags)
		}
		specs := downloadRepoSpecs("", lc.cc.DownloadLifts)
		if len(specs) != 1 || specs[0].Repo != "dl_dl_h" || specs[0].URL != "https://example.com/dl.h" {
			t.Errorf("lockfile record = %+v", specs)
		}
	})

	t.Run("without-flag-offline-declines", func(t *testing.T) {
		lc := mkLC(false)
		if _, ok := bakeBuildDirFile("dl.h", lc); ok {
			t.Fatal("without --lift-download and no on-disk bytes, the download must decline")
		}
	})
}

func TestDownloadFetchTarget(t *testing.T) {
	tgt := downloadFetchTarget("baked_config_h", "config.h", "dl_config_h")
	if tgt.Kind != ir.KindGenrule {
		t.Fatalf("kind = %v; want genrule", tgt.Kind)
	}
	if len(tgt.Srcs) != 1 || tgt.Srcs[0] != "@dl_config_h//file" {
		t.Errorf("srcs = %v; want [@dl_config_h//file]", tgt.Srcs)
	}
	if len(tgt.GenruleOuts) != 1 || tgt.GenruleOuts[0] != "config.h" {
		t.Errorf("outs = %v; want [config.h]", tgt.GenruleOuts)
	}
	if tgt.GenruleCmd != "cp $(SRCS) $@" {
		t.Errorf("cmd = %q; want cp $(SRCS) $@", tgt.GenruleCmd)
	}
}
