package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseCell_ThreeFieldForm: the historical
// <name>|<constraints>|<path> shape parses cleanly with no
// SelectLabel.
func TestParseCell_ThreeFieldForm(t *testing.T) {
	got, err := parseCell("linux|@platforms//os:linux,@platforms//cpu:x86_64|/tmp/ir.json")
	if err != nil {
		t.Fatalf("parseCell: %v", err)
	}
	want := parsedCell{
		name:        "linux",
		constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"},
		irJSONPath:  "/tmp/ir.json",
		selectLabel: "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v; want %+v", got, want)
	}
}

// TestParseCell_FourFieldForm: the optional 4th field
// carries the operator-declared SelectLabel.
func TestParseCell_FourFieldForm(t *testing.T) {
	got, err := parseCell("linux_aarch64|@platforms//os:linux,@platforms//cpu:arm64|/tmp/ir.json|//platforms:linux_aarch64")
	if err != nil {
		t.Fatalf("parseCell: %v", err)
	}
	if got.selectLabel != "//platforms:linux_aarch64" {
		t.Errorf("selectLabel = %q; want //platforms:linux_aarch64", got.selectLabel)
	}
}

// TestParseCell_FourFieldEmptyLabel: an empty 4th field is
// treated as "no override" — operators emitting a uniform
// 4-pipe form across cells can leave the field blank for
// platforms that don't need an explicit label.
func TestParseCell_FourFieldEmptyLabel(t *testing.T) {
	got, err := parseCell("linux|@platforms//os:linux|/tmp/ir.json|")
	if err != nil {
		t.Fatalf("parseCell: %v", err)
	}
	if got.selectLabel != "" {
		t.Errorf("empty 4th field should produce empty selectLabel; got %q", got.selectLabel)
	}
}

// TestParseCell_TooFewFields: a 2-pipe input is rejected.
func TestParseCell_TooFewFields(t *testing.T) {
	_, err := parseCell("linux|@platforms//os:linux")
	if err == nil || !strings.Contains(err.Error(), "3 or 4") {
		t.Errorf("expected 'expected 3 or 4 ...' error; got %v", err)
	}
}

// TestParseCell_TooManyFields: a 4-pipe input (5 fields) is
// rejected so accidental pipes in path/label don't quietly
// get split off into a phantom 5th field.
func TestParseCell_TooManyFields(t *testing.T) {
	_, err := parseCell("linux|@platforms//os:linux|/tmp/ir.json|//platforms:linux|extra")
	if err == nil || !strings.Contains(err.Error(), "3 or 4") {
		t.Errorf("expected 'expected 3 or 4 ...' error; got %v", err)
	}
}
