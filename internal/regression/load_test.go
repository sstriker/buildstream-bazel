package regression_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/regression"
)

// stageRun writes a minimal <root>/manifest/{converted,failures,determinism}.json
// triple shaped like what the orchestrator produces. Returns the run root.
func stageRun(t *testing.T, conv []string, fails []failEntry, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	mDir := filepath.Join(root, "manifest")
	if err := os.MkdirAll(mDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mustWrite(t, filepath.Join(mDir, "converted.json"), convJSON(conv))
	mustWrite(t, filepath.Join(mDir, "failures.json"), failJSON(fails))
	if files != nil {
		mustWrite(t, filepath.Join(mDir, "determinism.json"), detJSON(files))
	}
	return root
}

type failEntry struct {
	Element string
	Tier    int
	Code    string
	Message string
}

func TestLoadRun_BasicShape(t *testing.T) {
	root := stageRun(t,
		[]string{"components/hello", "components/uses-hello"},
		nil,
		map[string]string{
			"components/hello/BUILD.bazel":          "aaa",
			"components/hello/cmake-config/x.cmake": "bbb",
			"components/uses-hello/BUILD.bazel":     "ccc",
		},
	)
	r, err := regression.LoadRun(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"components/hello", "components/uses-hello"}
	if got := r.Names(); !sliceEq(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
	for _, n := range want {
		oc := r.Outcomes[n]
		if oc.Failure != nil {
			t.Errorf("%s should not have Failure", n)
		}
		if oc.Converted == nil {
			t.Fatalf("%s missing Converted", n)
		}
		if oc.Converted.Fingerprint == "" {
			t.Errorf("%s fingerprint empty", n)
		}
	}
}

func TestLoadRun_FailureOutcomeOverridesConverted(t *testing.T) {
	root := stageRun(t,
		[]string{"components/hello"},
		[]failEntry{{Element: "components/hello", Tier: 1, Code: "configure-failed", Message: "boom"}},
		nil,
	)
	r, err := regression.LoadRun(root)
	if err != nil {
		t.Fatal(err)
	}
	oc := r.Outcomes["components/hello"]
	if oc.Failure == nil || oc.Converted != nil {
		t.Fatalf("expected Failure-only outcome, got %+v", oc)
	}
	if oc.Failure.Code != "configure-failed" || oc.Failure.Tier != 1 {
		t.Errorf("Failure = %+v", oc.Failure)
	}
}

func TestLoadRun_FingerprintStableUnderEntryReorder(t *testing.T) {
	// Same logical files, two different orderings in the determinism.json
	// produce the same per-element fingerprint because LoadRun sorts the
	// keys before hashing. (orchestrator's writer already sorts; this
	// test belts-and-suspenders the loader.)
	files1 := map[string]string{
		"components/hello/a": "1",
		"components/hello/b": "2",
	}
	files2 := map[string]string{
		"components/hello/b": "2",
		"components/hello/a": "1",
	}
	r1, err := regression.LoadRun(stageRun(t, []string{"components/hello"}, nil, files1))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := regression.LoadRun(stageRun(t, []string{"components/hello"}, nil, files2))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Outcomes["components/hello"].Converted.Fingerprint !=
		r2.Outcomes["components/hello"].Converted.Fingerprint {
		t.Errorf("fingerprint diverged across reorder")
	}
}

func TestLoadRun_FingerprintShiftsOnContentChange(t *testing.T) {
	r1, err := regression.LoadRun(stageRun(t,
		[]string{"components/hello"}, nil,
		map[string]string{"components/hello/a": "1"},
	))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := regression.LoadRun(stageRun(t,
		[]string{"components/hello"}, nil,
		map[string]string{"components/hello/a": "2"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Outcomes["components/hello"].Converted.Fingerprint ==
		r2.Outcomes["components/hello"].Converted.Fingerprint {
		t.Errorf("identical fingerprint despite content change")
	}
}

func TestLoadRun_MissingDeterminismIsTolerated(t *testing.T) {
	root := stageRun(t, []string{"x"}, nil, nil)
	r, err := regression.LoadRun(root)
	if err != nil {
		t.Fatal(err)
	}
	oc := r.Outcomes["x"]
	if oc.Converted == nil {
		t.Fatal("x missing Converted")
	}
	if oc.Converted.Fingerprint != "" {
		t.Errorf("fingerprint = %q, want empty when determinism.json absent",
			oc.Converted.Fingerprint)
	}
}

func TestLoadRun_RejectsMissingConvertedJSON(t *testing.T) {
	_, err := regression.LoadRun(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "converted.json") {
		t.Errorf("err = %v, want missing-converted.json error", err)
	}
}

func TestLoadRun_RejectsUnknownConvertedVersion(t *testing.T) {
	root := t.TempDir()
	mDir := filepath.Join(root, "manifest")
	_ = os.MkdirAll(mDir, 0o755)
	mustWrite(t, filepath.Join(mDir, "converted.json"), `{"version":99,"elements":[]}`)
	_, err := regression.LoadRun(root)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("err = %v, want version error", err)
	}
}

// ----- helpers -----------------------------------------------------------

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func convJSON(names []string) string {
	var b strings.Builder
	b.WriteString(`{"version":1,"elements":[`)
	for i, n := range names {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"`)
		b.WriteString(n)
		b.WriteString(`"}`)
	}
	b.WriteString("]}")
	return b.String()
}

func failJSON(fs []failEntry) string {
	var b strings.Builder
	b.WriteString(`{"version":1,"elements":[`)
	for i, f := range fs {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"element":"`)
		b.WriteString(f.Element)
		b.WriteString(`","tier":`)
		b.WriteString(itoa(f.Tier))
		b.WriteString(`,"code":"`)
		b.WriteString(f.Code)
		b.WriteString(`","message":"`)
		b.WriteString(f.Message)
		b.WriteString(`"}`)
	}
	b.WriteString("]}")
	return b.String()
}

func detJSON(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	// Sort for stable test fixtures.
	sortStrings(keys)
	var b strings.Builder
	b.WriteString(`{"version":1,"files":[`)
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"path":"`)
		b.WriteString(k)
		b.WriteString(`","sha256":"`)
		b.WriteString(files[k])
		b.WriteString(`"}`)
	}
	b.WriteString("]}")
	return b.String()
}

// tiny self-contained helpers so this file doesn't pull more imports.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
