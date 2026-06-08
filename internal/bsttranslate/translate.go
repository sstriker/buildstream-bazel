// Package bsttranslate rewrites BuildStream element YAMLs to use
// kind:remote-asset sources, replacing kind:git / kind:tar / etc.
// with a URI+qualifier pair the orchestrator's M3d source-CAS
// resolver consumes.
//
// What we do NOT do: compute BuildStream's Source.unique_key()
// ourselves. That hash is plugin-specific, encodes BuildStream
// internals, and drifts across BuildStream versions. We instead
// pick a stable URI keyed off the element's path identity and
// preserve the original source spec as qualifiers, leaving the
// CAS-population step (`bst source push` or equivalent) responsible
// for binding the URI to the actual Directory digest.
//
// URI scheme:
//
//	bst:source:<element-name>[:<source-index>]
//
// `<element-name>` is the project-relative path of the .bst file
// minus the suffix (forward-slashed). `<source-index>` is appended
// only when an element has multiple sources; single-source elements
// (the FDSDK norm) get the unsuffixed form.
//
// Qualifiers preserve the original spec so an operator's CAS-push
// pipeline can match on them:
//
//	bst-source-kind:    <git/tar/...>
//	bst-source-url:     <original url>
//	bst-source-ref:     <original ref>
//	bst-source-<key>:   <stringified Extra value>, for each preserved
//	                    BuildStream Source field
package bsttranslate

import (
	"fmt"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/element"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// TranslateElement returns a copy of el with each translatable source
// rewritten to kind:remote-asset. kind:local and kind:remote-asset
// sources are passed through unchanged. Returns an error if a source
// is missing required identity fields (url for git/tar/...).
//
// The returned Element shares no mutable state with the input — safe
// to marshal and write to disk independently.
func TranslateElement(el *element.Element) (*element.Element, error) {
	out := &element.Element{
		Name:       el.Name,
		SourcePath: el.SourcePath,
		Kind:       el.Kind,
		Depends:    append([]element.Dep(nil), el.Depends...),
	}
	multiSource := len(el.Sources) > 1
	for i, src := range el.Sources {
		switch src.Kind {
		case "", "local", "remote-asset":
			out.Sources = append(out.Sources, copySource(src))
		default:
			translated, err := translateSource(el.Name, i, multiSource, src)
			if err != nil {
				return nil, fmt.Errorf("element %s source[%d]: %w", el.Name, i, err)
			}
			out.Sources = append(out.Sources, translated)
		}
	}
	return out, nil
}

// translateSource maps one git / tar / ... source to kind:remote-asset.
// URL must be present; Ref is optional but recommended (operators
// without a ref-bound URI can still bind by url-only).
func translateSource(elementName string, index int, multiSource bool, src element.Source) (element.Source, error) {
	if src.URL == "" {
		return element.Source{}, fmt.Errorf("kind:%s missing url", src.Kind)
	}
	uri := URIFor(elementName, index, multiSource)

	qualifiers := map[string]any{}
	qualifiers["bst-source-kind"] = src.Kind
	qualifiers["bst-source-url"] = src.URL
	if src.Ref != "" {
		qualifiers["bst-source-ref"] = src.Ref
	}
	for k, v := range src.Extra {
		// url and ref already promoted into top-level fields.
		if k == "url" || k == "ref" || k == "kind" {
			continue
		}
		qualifiers["bst-source-"+k] = stringify(v)
	}

	return element.Source{
		Kind: "remote-asset",
		Extra: map[string]any{
			"uri":        uri,
			"qualifiers": qualifiers,
		},
	}, nil
}

// URIFor returns the canonical bst:source:... URI for one source
// position in an element. Single-source elements (the common case)
// get the unsuffixed form; multi-source elements append :<index>.
func URIFor(elementName string, index int, multiSource bool) string {
	base := "bst:source:" + elementName
	if multiSource {
		return fmt.Sprintf("%s:%d", base, index)
	}
	return base
}

// QualifierKeys returns the qualifier names this translator emits, in
// canonical sorted order. Useful for tooling that wants to know what
// to expect in the asset server.
func QualifierKeys(src element.Source) []string {
	if src.Kind != "remote-asset" {
		return nil
	}
	q, _ := src.Extra["qualifiers"].(map[string]any)
	keys := sliceutil.SortedKeys(q)
	return keys
}

func copySource(src element.Source) element.Source {
	out := element.Source{
		Kind: src.Kind,
		URL:  src.URL,
		Ref:  src.Ref,
	}
	if len(src.Extra) > 0 {
		out.Extra = make(map[string]any, len(src.Extra))
		for k, v := range src.Extra {
			out.Extra[k] = v
		}
	}
	return out
}

// stringify renders a YAML scalar back to its canonical string form.
// Qualifier values are strings on the wire (REAPI proto), but
// BuildStream YAML may give us int / bool / float for some plugin
// fields. Anything else is best-effort `%v`.
func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}
