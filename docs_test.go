package main

import (
	"os"
	"strings"
	"testing"
)

// The Ukrainian docs are hand-maintained translations. Their structure must
// match the English source, so a section or table row added to one is not
// silently missing from the other.
func TestTranslatedDocsMatchStructure(t *testing.T) {
	for _, base := range []string{"README", "REVIEW", "docs/README", "docs/architecture", "docs/cli", "docs/guide"} {
		english, err := os.ReadFile(base + ".md")
		if err != nil {
			t.Fatal(err)
		}
		ukrainian, err := os.ReadFile(base + ".uk.md")
		if err != nil {
			t.Fatal(err)
		}
		if a, b := docShape(string(english)), docShape(string(ukrainian)); a != b {
			t.Errorf("%s.md and %s.uk.md differ in structure: %+v vs %+v", base, base, a, b)
		}
	}
}

type shape struct{ headings, tableRows, fences, listItems int }

func docShape(text string) shape {
	var s shape
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "```"):
			s.fences++
		case s.fences%2 == 1:
			// Lines inside a fenced block are code, not structure.
		case strings.HasPrefix(line, "#"):
			s.headings++
		case strings.HasPrefix(line, "|"):
			s.tableRows++
		case strings.HasPrefix(line, "- "):
			s.listItems++
		}
	}
	return s
}
