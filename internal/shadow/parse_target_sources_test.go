package shadow

import (
	"reflect"
	"testing"
)

// TestParseTargetSourcesArgs covers the flat-list and FILE_SET (cmake 3.23+)
// grammars, including the mixed shape that previously dropped EVERYTHING (the
// bail-on-FILE_SET behavior lost even the flat sources in the same call).
func TestParseTargetSourcesArgs(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		target string
		srcs   []string
		ok     bool
	}{
		{
			name:   "flat list",
			args:   []string{"app", "PRIVATE", "a.cpp", "b.cpp"},
			target: "app", srcs: []string{"a.cpp", "b.cpp"}, ok: true,
		},
		{
			name:   "multiple visibility groups",
			args:   []string{"app", "PRIVATE", "a.cpp", "PUBLIC", "b.cpp"},
			target: "app", srcs: []string{"a.cpp", "b.cpp"}, ok: true,
		},
		{
			name:   "semicolon list split",
			args:   []string{"app", "PRIVATE", "a.cpp;b.cpp"},
			target: "app", srcs: []string{"a.cpp", "b.cpp"}, ok: true,
		},
		{
			// FILE_SET HEADERS: the set name (HEADERS) and BASE_DIRS dir are
			// skipped; only the FILES entries are collected.
			name:   "FILE_SET HEADERS with BASE_DIRS",
			args:   []string{"app", "PUBLIC", "FILE_SET", "HEADERS", "BASE_DIRS", "include", "FILES", "include/foo.h"},
			target: "app", srcs: []string{"include/foo.h"}, ok: true,
		},
		{
			// Explicit set name + TYPE: both name (myset) and type value
			// (HEADERS) are skipped, BASE_DIRS dir skipped, FILES collected.
			name:   "FILE_SET named set with TYPE",
			args:   []string{"app", "PUBLIC", "FILE_SET", "myset", "TYPE", "HEADERS", "BASE_DIRS", "inc", "FILES", "inc/a.h", "inc/b.h"},
			target: "app", srcs: []string{"inc/a.h", "inc/b.h"}, ok: true,
		},
		{
			// C++ modules FILE_SET — these FILES are compiled module units.
			name:   "FILE_SET CXX_MODULES",
			args:   []string{"app", "PUBLIC", "FILE_SET", "CXX_MODULES", "FILES", "mod.cppm"},
			target: "app", srcs: []string{"mod.cppm"}, ok: true,
		},
		{
			// The regression the bail caused: a flat source AND a FILE_SET in one
			// call. Previously the whole call returned false (a.cpp lost too); now
			// both the flat source and the FILE_SET's FILES are collected.
			name:   "mixed flat + FILE_SET",
			args:   []string{"app", "PRIVATE", "a.cpp", "PUBLIC", "FILE_SET", "HEADERS", "BASE_DIRS", "include", "FILES", "include/b.h"},
			target: "app", srcs: []string{"a.cpp", "include/b.h"}, ok: true,
		},
		{
			// `;`-delimited FILES group splits like a flat list.
			name:   "FILE_SET FILES semicolon list",
			args:   []string{"app", "PUBLIC", "FILE_SET", "HEADERS", "FILES", "a.h;b.h"},
			target: "app", srcs: []string{"a.h", "b.h"}, ok: true,
		},
		{
			// A FILE_SET that declares only BASE_DIRS (no FILES) contributes no
			// sources — the dirs are not files.
			name: "FILE_SET BASE_DIRS only, no files",
			args: []string{"app", "PUBLIC", "FILE_SET", "HEADERS", "BASE_DIRS", "include"},
			ok:   false,
		},
		{
			name: "too few args",
			args: []string{"app"},
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, srcs, ok := parseTargetSourcesArgs(tc.args)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (target=%q srcs=%v)", ok, tc.ok, target, srcs)
			}
			if !ok {
				return
			}
			if target != tc.target || !reflect.DeepEqual(srcs, tc.srcs) {
				t.Errorf("got (%q, %v), want (%q, %v)", target, srcs, tc.target, tc.srcs)
			}
		})
	}
}
