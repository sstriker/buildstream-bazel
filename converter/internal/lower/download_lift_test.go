package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
)

func TestEmitDownloadLiftTodos(t *testing.T) {
	c := todos.New()
	emitDownloadLiftTodos(c, []downloadLiftRecord{
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
	for _, want := range []string{`http_file(`, `name = "dl_dl_config_h"`, `urls = ["https://example.com/config.h"]`, `sha256 = "abc123"`, `@dl_dl_config_h//file`} {
		if !strings.Contains(st, want) {
			t.Errorf("stanza missing %q:\n%s", want, st)
		}
	}
}

func TestHttpFileIntegrity(t *testing.T) {
	if a, v, ok := httpFileIntegrity("SHA256=deadbeef"); !ok || a != "sha256" || v != "deadbeef" {
		t.Errorf("SHA256 → %q/%q/%v; want sha256/deadbeef/true", a, v, ok)
	}
	if _, _, ok := httpFileIntegrity("MD5=x"); ok {
		t.Error("MD5 has no http_file attr; want ok=false")
	}
	if _, _, ok := httpFileIntegrity("nohash"); ok {
		t.Error("malformed hash; want ok=false")
	}
}
