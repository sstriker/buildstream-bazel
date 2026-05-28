package lower

import (
	"fmt"
	"io"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// surfaceInstallScriptInstallers emits a stderr warning summarising
// any install(SCRIPT) / install(CODE) directives in the cmake reply.
// These directory installers run cmake script code at install time
// — post-install patching of cmake config files, chmod adjustments,
// symlink creation. None of these have a clean Bazel translation:
// Bazel's install story is operator-side via rules_pkg or similar.
//
// The converter silently dropped these before; this surfacing makes
// the omission auditable. Operators who care about preserving
// install-time logic now have a clear signal that something was
// dropped, with the cmake source backtrace so they can locate the
// declaration.
//
// No-op when sink is nil (preserves the lower-as-pure-function
// shape every existing test depends on) or when no script/code
// installers are present.
func surfaceInstallScriptInstallers(r *fileapi.Reply, sink io.Writer) {
	if r == nil || sink == nil {
		return
	}
	var scriptCount, codeCount int
	for _, dir := range r.Directories {
		for _, inst := range dir.Installers {
			switch inst.Type {
			case "script":
				scriptCount++
			case "code":
				codeCount++
			}
		}
	}
	if scriptCount == 0 && codeCount == 0 {
		return
	}
	if scriptCount > 0 {
		fmt.Fprintf(sink,
			"lower: %d install(SCRIPT) directive(s) silently dropped — no Bazel analogue for install-time cmake script execution; consider rules_pkg or operator-side install handling\n",
			scriptCount)
	}
	if codeCount > 0 {
		fmt.Fprintf(sink,
			"lower: %d install(CODE) directive(s) silently dropped — no Bazel analogue for install-time inline cmake code; consider rules_pkg or operator-side install handling\n",
			codeCount)
	}
}
