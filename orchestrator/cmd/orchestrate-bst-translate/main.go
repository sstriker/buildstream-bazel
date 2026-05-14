// orchestrate-bst-translate rewrites a BuildStream element tree's
// .bst files to use kind:remote-asset sources instead of the original
// kind:git / kind:tar / etc.
//
// Workflow:
//
//  1. Operator's existing pipeline runs `bst source push --remote=<cas>`
//     to populate the project's source CAS, binding asset URIs to
//     Directory digests.
//  2. orchestrate-bst-translate --in elements/ --out elements-cas/
//     produces a parallel tree where every translatable source is
//     rewritten to kind:remote-asset.
//  3. orchestrate --fdsdk-root=<wherever> --elements-dir=elements-cas
//     --source-cas=grpc://<cas>
//     converts using digest-resolved sources — no git clones, no
//     re-fetching.
//
// The translator does NOT compute BuildStream's Source.unique_key()
// itself (plugin-specific, version-fragile). The URI scheme is keyed
// off element name, with the original spec preserved as qualifiers
// so the operator's CAS-population step can match URIs to digests
// however it likes (typically: a script that runs `bst show` per
// element and binds via the Remote Asset Push API).
//
// See orchestrator/internal/bsttranslate/translate.go for the URI
// scheme + qualifier convention.
package main

import (
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/sstriker/buildstream-bazel/internal/element"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/bsttranslate"
)

func main() {
	flags := flag.NewFlagSet("orchestrate-bst-translate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	in := flags.String("in", "", "input directory containing .bst element files (recursive)")
	out := flags.String("out", "", "output directory; created if absent. .bst files are rewritten here.")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(64)
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "orchestrate-bst-translate: --in and --out are required")
		flags.Usage()
		os.Exit(64)
	}

	count, err := translateTree(*in, *out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate-bst-translate: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "translated %d element(s) from %s -> %s\n", count, *in, *out)
}

// translateTree walks every .bst file under in, parses, translates,
// and writes the result to the matching path under out. Returns the
// number of files translated.
func translateTree(in, out string) (int, error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return 0, err
	}
	count := 0
	err := filepath.WalkDir(in, func(p string, d iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(p) != ".bst" {
			return nil
		}
		rel, err := filepath.Rel(in, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(out, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		// Element name = path under in/ minus the .bst suffix,
		// forward-slashed (matches element.ReadProject's convention).
		name := filepath.ToSlash(rel[:len(rel)-len(".bst")])
		if err := translateOne(p, dst, name); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		count++
		return nil
	})
	if err != nil {
		return count, err
	}
	if count == 0 {
		return 0, errors.New("no .bst files found")
	}
	return count, nil
}

// translateOne reads a single .bst, runs it through the translator,
// and writes the rewritten YAML. Comments and key ordering in the
// original aren't preserved — yaml.v3 round-trip drops them. The
// translator's purpose is the orchestrator-consumable output, not
// human re-editing.
func translateOne(srcPath, dstPath, name string) error {
	body, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	el, err := element.ParseElement(srcPath, name)
	if err != nil {
		return err
	}
	translated, err := bsttranslate.TranslateElement(el)
	if err != nil {
		return err
	}
	rendered, err := renderElement(translated, body)
	if err != nil {
		return err
	}
	return os.WriteFile(dstPath, rendered, 0o644)
}

// renderElement emits the translated element's YAML. We re-encode
// kind/depends/sources from the translated struct; everything else
// in the original (description, variables, etc.) is preserved by
// merging into the original document.
//
// The original `body` is round-tripped through yaml.v3 Node form so
// non-translated keys keep their position and any comments at top
// level survive. Per-source comments (rare in FDSDK) do not.
func renderElement(el *element.Element, original []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(original, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("input is not a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("input is not a YAML mapping")
	}
	// Replace the `sources:` mapping with the translated one. Other
	// keys (kind, depends, description, ...) survive unchanged.
	srcNode, err := buildSourcesNode(el.Sources)
	if err != nil {
		return nil, err
	}
	if err := setMappingKey(root, "sources", srcNode); err != nil {
		return nil, err
	}
	body, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// buildSourcesNode emits a YAML SequenceNode for sources where each
// entry is a mapping of (kind, uri, qualifiers) for translated sources
// or the originals' shape (kind+url+ref+extra) for passthrough kinds.
func buildSourcesNode(sources []element.Source) (*yaml.Node, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, s := range sources {
		entry := &yaml.Node{Kind: yaml.MappingNode}
		appendScalar(entry, "kind", s.Kind)
		switch s.Kind {
		case "remote-asset":
			if uri, ok := s.Extra["uri"].(string); ok {
				appendScalar(entry, "uri", uri)
			}
			if q, ok := s.Extra["qualifiers"].(map[string]any); ok && len(q) > 0 {
				appendQualifiers(entry, q)
			}
		default:
			if s.URL != "" {
				appendScalar(entry, "url", s.URL)
			}
			if s.Ref != "" {
				appendScalar(entry, "ref", s.Ref)
			}
			for k, v := range s.Extra {
				if k == "url" || k == "ref" || k == "kind" {
					continue
				}
				appendValue(entry, k, v)
			}
		}
		seq.Content = append(seq.Content, entry)
	}
	return seq, nil
}

func appendScalar(m *yaml.Node, key, value string) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value},
	)
}

func appendValue(m *yaml.Node, key string, v any) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%v", v)},
	)
}

func appendQualifiers(m *yaml.Node, q map[string]any) {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sortStrings(keys)
	qn := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		appendValue(qn, k, q[k])
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "qualifiers"},
		qn,
	)
}

func setMappingKey(m *yaml.Node, key string, value *yaml.Node) error {
	for i := 0; i < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return nil
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		value,
	)
	return nil
}

// sortStrings is a tiny in-place sort to avoid pulling in `sort`
// when the rest of the file doesn't use it.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
