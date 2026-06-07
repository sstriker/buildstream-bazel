// Command intent-lens is the deterministic harness for the intent-capture
// survey lens (see ROADMAP.md). It has two subcommands; the LLM judgment in
// between is a pluggable command the operator pipes:
//
//	intent-lens prompt --converted <dir> --cmake-src <dir> \
//	    --todos <conversion-todos.json> --rejections <rejections.json> \
//	    --element <name> > prompt.txt
//	<your judge, e.g. claude -p> < prompt.txt > findings.json
//	intent-lens triage --findings findings.json \
//	    --todos <conversion-todos.json> --rejections <rejections.json> \
//	    --element <name> --out intent-capture.json
//
// Keeping the judge out of this binary means the survey/render-gate can stub it
// (no model call in CI) and operators can swap judges freely.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sstriker/buildstream-bazel/converter/internal/intentlens"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: intent-lens <prompt|triage> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "prompt":
		err = runPrompt(os.Args[2:])
	case "triage":
		err = runTriage(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q (want prompt|triage)", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "intent-lens:", err)
		os.Exit(1)
	}
}

// loadTodos reads a conversion-todos.json; an empty path yields an empty report.
func loadTodos(path string) (todos.Report, error) {
	var rep todos.Report
	if path == "" {
		return rep, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return rep, fmt.Errorf("read todos %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return rep, fmt.Errorf("parse todos %s: %w", path, err)
	}
	return rep, nil
}

// loadRejections reads a rejections.json (a top-level array); empty path → nil.
func loadRejections(path string) ([]rejection.Rejection, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rejections %s: %w", path, err)
	}
	var items []rejection.Rejection
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("parse rejections %s: %w", path, err)
	}
	return items, nil
}

func runPrompt(args []string) error {
	fs := newFlagSet("prompt")
	converted := fs.String("converted", "", "directory holding the rendered BUILD.bazel + MODULE.bazel")
	cmakeSrc := fs.String("cmake-src", "", "original cmake source tree (anchors point here)")
	todosPath := fs.String("todos", "", "path to conversion-todos.json (already-flagged set)")
	rejPath := fs.String("rejections", "", "path to rejections.json (already-refused set)")
	element := fs.String("element", "", "converted element name (prompt header)")
	contextPath := fs.String("context", "", "optional file overriding the standing context block")
	out := fs.String("out", "", "write the prompt here (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep, err := loadTodos(*todosPath)
	if err != nil {
		return err
	}
	rej, err := loadRejections(*rejPath)
	if err != nil {
		return err
	}
	ctx := ""
	if *contextPath != "" {
		b, rerr := os.ReadFile(*contextPath)
		if rerr != nil {
			return fmt.Errorf("read context %s: %w", *contextPath, rerr)
		}
		ctx = string(b)
	}
	prompt := intentlens.AssemblePrompt(intentlens.PromptInputs{
		Element:      *element,
		ConvertedDir: *converted,
		CMakeSrcDir:  *cmakeSrc,
		Todos:        rep,
		Rejections:   rej,
		Context:      ctx,
	})
	if *out == "" || *out == "-" {
		_, err = os.Stdout.WriteString(prompt)
		return err
	}
	return os.WriteFile(*out, []byte(prompt), 0o644)
}

func runTriage(args []string) error {
	fs := newFlagSet("triage")
	findingsPath := fs.String("findings", "", "judge output JSON (default: stdin)")
	todosPath := fs.String("todos", "", "path to conversion-todos.json (for dedup)")
	rejPath := fs.String("rejections", "", "path to rejections.json (for dedup)")
	element := fs.String("element", "", "converted element name")
	toolVersion := fs.String("tool-version", "", "tool version stamped into the report")
	out := fs.String("out", "", "write intent-capture.json here (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var raw []byte
	var err error
	if *findingsPath == "" || *findingsPath == "-" {
		raw, err = readAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*findingsPath)
	}
	if err != nil {
		return fmt.Errorf("read findings: %w", err)
	}
	judge, err := intentlens.ParseJudgeOutput(raw)
	if err != nil {
		return err
	}
	rep, err := loadTodos(*todosPath)
	if err != nil {
		return err
	}
	rej, err := loadRejections(*rejPath)
	if err != nil {
		return err
	}
	report := intentlens.Triage(judge, rep, rej, *element, *toolVersion)
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if *out == "" || *out == "-" {
		_, err = os.Stdout.Write(body)
		return err
	}
	return os.WriteFile(*out, body, 0o644)
}
