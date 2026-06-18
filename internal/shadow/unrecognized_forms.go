package shadow

import "strings"

// UnrecognizedForm flags an in-source-tree trace event for a command the
// converter MODELS, but in a FORM no classifier accepted — i.e. a build-input-
// producing command shape that would otherwise be silently dropped (the class of
// bug where add_custom_command's TARGET-event form was filtered out for a long
// time with no signal). Surfacing it turns the next missed form loud (a coverage
// finding) instead of invisible.
type UnrecognizedForm struct {
	Cmd    string // the cmake command (e.g. "add_custom_command", "file")
	Form   string // a short form label (e.g. "add_custom_command", "file(COPY)")
	File   string // the CMakeLists.txt that issued it
	Line   int
	Detail string // a short, head-truncated arg summary for triage
}

// fileOutputSubcommands are the file() subcommands that PRODUCE a file a build
// could consume — the ones whose silent drop is a lost build input. Read /
// query / lock / directory forms (READ, STRINGS, GLOB, MD5/SHA*, LOCK,
// MAKE_DIRECTORY, CHMOD, …) don't produce a consumable artifact, so an
// unhandled one isn't a coverage gap and is left unflagged to keep the signal
// clean.
var fileOutputSubcommands = map[string]bool{
	"WRITE": true, "APPEND": true, "TOUCH": true, "TOUCH_NOCREATE": true,
	"COPY": true, "COPY_FILE": true, "GENERATE": true, "RENAME": true,
	"CONFIGURE": true,
}

// AuditUnrecognizedCommandForms scans the trace for in-source-tree events of
// build-input-producing commands the converter models, and returns the ones no
// form-classifier accepted. v1 covers add_custom_command (OUTPUT vs TARGET-event
// — the form that bit us) and the file() output-producing subcommands (e.g.
// file(COPY … PATTERN), which classifyFileWriter declines, or file(CONFIGURE),
// which has no classifier). Conservative: only flags a command we DO model in
// some form, so a hit is a real "we handle this command but not this shape" gap.
func AuditUnrecognizedCommandForms(traceRaw []byte, sourceRoot string) []UnrecognizedForm {
	var out []UnrecognizedForm
	for _, ev := range ParseTrace(traceRaw) {
		if !inSourceTree(ev.File, sourceRoot) || len(ev.Args) == 0 {
			continue
		}
		switch strings.ToLower(ev.Cmd) {
		case "add_custom_command":
			if _, ok := classifyAddCustomCommand(ev, sourceRoot, ""); ok {
				continue
			}
			if _, ok := classifyTargetEventCommand(ev); ok {
				continue
			}
			out = append(out, unrecognizedFormOf(ev, "add_custom_command"))
		case "file":
			sub := strings.ToUpper(ev.Args[0])
			if !fileOutputSubcommands[sub] || fileFormRecognized(ev, sourceRoot) {
				continue
			}
			out = append(out, unrecognizedFormOf(ev, "file("+sub+")"))
		}
	}
	return out
}

// fileFormRecognized reports whether any file()-handling classifier accepts ev.
func fileFormRecognized(ev TraceEvent, sourceRoot string) bool {
	if _, ok := classifyFileWriter(ev); ok {
		return true
	}
	if _, ok := classifyFileGenerate(ev, sourceRoot, ""); ok {
		return true
	}
	if _, ok := classifyFileRename(ev, sourceRoot, ""); ok {
		return true
	}
	return false
}

func unrecognizedFormOf(ev TraceEvent, form string) UnrecognizedForm {
	detail := strings.Join(ev.Args, " ")
	if len(detail) > 160 {
		detail = detail[:157] + "..."
	}
	return UnrecognizedForm{
		Cmd:    strings.ToLower(ev.Cmd),
		Form:   form,
		File:   ev.File,
		Line:   ev.Line,
		Detail: detail,
	}
}
