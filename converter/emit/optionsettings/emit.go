// Package optionsettings emits the //options package BUILD content
// that backs the option fold's select() arms.
//
// The option-lift ROADMAP.md item. lower's option fold
// (--lift-options) projects a lifted cmake option's attribute deltas
// onto //options select() arms — //options:<name>_{on,off} for BOOL
// option()s (lower.OptionCellLabel), //options:<name>_<value> for
// STRING cache options carrying a STRINGS allowed-value list
// (lower.OptionValueCellLabel). Those labels must resolve to real
// config_settings for the converted BUILD to load. This package
// renders a project-level //options package: one bool_flag per
// lifted BOOL option / one string_flag (with `values`) per lifted
// enum option — build_setting_default = the value the primary
// configure resolved, so an unset flag reproduces the baked
// baseline view — plus one config_setting per arm, toggled at build
// time with --//options:<name>=<value>.
//
// A dedicated flag per option rather than a shared string-valued
// dial keeps each cmake option independently toggleable — exactly
// the cache-entry semantics cmake gives them.
//
// The generated BUILD is byte-stable across runs against the same
// option set and carries a leading attribution header.
package optionsettings

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Option is one lifted cmake option: its cache-entry name, the
// value the primary configure resolved (the flag's default, so
// builds that don't set the flag get the baked view), and — for
// enum (STRING + STRINGS) options — the allowed-value list. An
// empty Values means BOOL: Default must then be "True" or "False"
// (the bool_flag literal). For enums, Default and Values are the
// cmake cache strings verbatim (the flag accepts them as typed on
// the CLI); the config_setting NAMES use the sanitized form the
// fold's arm labels use, supplied per value in ValueSuffixes.
type Option struct {
	Name    string
	Default string
	// Values, non-empty, marks an enum option: the STRINGS list
	// verbatim, in declared order.
	Values []string
	// ValueSuffixes maps each Values entry to the label-safe
	// suffix its config_setting is named with
	// (lower.SanitizeOptionValue's output). Required when Values
	// is non-empty; entries must be unique.
	ValueSuffixes map[string]string
}

// Group is one config_setting_group the 2D option×config fold's
// mixed-support facts select on: Name is the //options-package
// target name, MatchAll the conditions that must all hold (the
// //config:<cfg> setting and the option-value setting). Rendered via
// skylib's selects.config_setting_group.
type Group struct {
	Name     string
	MatchAll []string
}

// Emit renders the //options package BUILD content for the given
// lifted options plus any AND-groups the 2D fold needs. Names are
// lowercased to match lower's arm labels, de-duplicated (first
// occurrence wins), and sorted for byte stability. Returns nil when
// no options are supplied — no fold to back, no //options package
// needed.
func Emit(options []Option, groups []Group) []byte {
	seen := map[string]bool{}
	var opts []Option
	needBool, needString := false, false
	for _, o := range options {
		n := strings.ToLower(o.Name)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		o.Name = n
		opts = append(opts, o)
		if len(o.Values) > 0 {
			needString = true
		} else {
			needBool = true
		}
	}
	if len(opts) == 0 {
		return nil
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Name < opts[j].Name })

	var b bytes.Buffer
	b.WriteString(header)
	b.WriteString("\nload(\"@bazel_skylib//rules:common_settings.bzl\"")
	if needBool {
		b.WriteString(", \"bool_flag\"")
	}
	if needString {
		b.WriteString(", \"string_flag\"")
	}
	b.WriteString(")\n")
	if len(groups) > 0 {
		b.WriteString("load(\"@bazel_skylib//lib:selects.bzl\", \"selects\")\n")
	}

	for _, o := range opts {
		if len(o.Values) > 0 {
			emitEnum(&b, o)
		} else {
			emitBool(&b, o)
		}
	}
	emitGroups(&b, groups)
	return b.Bytes()
}

// emitGroups renders the 2D fold's AND-groups, deduplicated by name
// and sorted for byte stability.
func emitGroups(b *bytes.Buffer, groups []Group) {
	seen := map[string]bool{}
	var gs []Group
	for _, g := range groups {
		n := strings.ToLower(g.Name)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		g.Name = n
		gs = append(gs, g)
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].Name < gs[j].Name })
	for _, g := range gs {
		b.WriteString("\n")
		b.WriteString("selects.config_setting_group(\n")
		fmt.Fprintf(b, "    name = %q,\n", g.Name)
		b.WriteString("    match_all = [\n")
		for _, m := range g.MatchAll {
			fmt.Fprintf(b, "        %q,\n", m)
		}
		b.WriteString("    ],\n")
		b.WriteString("    visibility = [\"//visibility:public\"],\n")
		b.WriteString(")\n")
	}
}

func emitBool(b *bytes.Buffer, o Option) {
	def := "False"
	if o.Default == "True" {
		def = "True"
	}
	b.WriteString("\n")
	b.WriteString("bool_flag(\n")
	fmt.Fprintf(b, "    name = %q,\n", o.Name)
	fmt.Fprintf(b, "    build_setting_default = %s,\n", def)
	b.WriteString("    visibility = [\"//visibility:public\"],\n")
	b.WriteString(")\n")
	for _, arm := range []struct{ suffix, value string }{
		{"_on", "True"},
		{"_off", "False"},
	} {
		b.WriteString("\n")
		b.WriteString("config_setting(\n")
		fmt.Fprintf(b, "    name = %q,\n", o.Name+arm.suffix)
		fmt.Fprintf(b, "    flag_values = {\":%s\": %q},\n", o.Name, arm.value)
		b.WriteString("    visibility = [\"//visibility:public\"],\n")
		b.WriteString(")\n")
	}
}

func emitEnum(b *bytes.Buffer, o Option) {
	b.WriteString("\n")
	b.WriteString("string_flag(\n")
	fmt.Fprintf(b, "    name = %q,\n", o.Name)
	fmt.Fprintf(b, "    build_setting_default = %q,\n", o.Default)
	b.WriteString("    values = [\n")
	for _, v := range o.Values {
		fmt.Fprintf(b, "        %q,\n", v)
	}
	b.WriteString("    ],\n")
	b.WriteString("    visibility = [\"//visibility:public\"],\n")
	b.WriteString(")\n")
	for _, v := range o.Values {
		b.WriteString("\n")
		b.WriteString("config_setting(\n")
		fmt.Fprintf(b, "    name = %q,\n", o.Name+"_"+o.ValueSuffixes[v])
		fmt.Fprintf(b, "    flag_values = {\":%s\": %q},\n", o.Name, v)
		b.WriteString("    visibility = [\"//visibility:public\"],\n")
		b.WriteString(")\n")
	}
}

const header = `# Generated by convert-element-cmake. DO NOT EDIT.
#
# Flags + config_settings backing the option fold's //options select()
# arms — one bool_flag per cmake option() and one string_flag per
# enum (STRING + STRINGS) cache option lifted via --lift-options.
# Toggle an option at build time with --//options:<name>=<value>; the
# default reproduces the value the convert-time configure resolved.
`
