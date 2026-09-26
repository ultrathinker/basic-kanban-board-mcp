package view

import (
	"strings"
	"testing"
	"time"
)

func TestScopeChangeMarks_NamePostingKeyNextToADifferentSignature(t *testing.T) {
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	marks := ScopeChangeMarks([]ScopeChangeNote{
		{At: at, Author: "owner", Via: "opus-x3", Body: "drop the export"},
		{At: at, Author: "opus-x3", Body: "add a test"},
	})
	if !strings.Contains(marks[0].Note, "declared here by owner · via opus-x3:") {
		t.Errorf("tooltip hides the posting key: %q", marks[0].Note)
	}
	if !strings.Contains(marks[1].Note, "declared here by opus-x3:") || strings.Contains(marks[1].Note, "via") {
		t.Errorf("self-signed tooltip: %q", marks[1].Note)
	}
}
