package view_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// ---------------------------------------------------------------------------
// KANB-40, review debt item 3: every other fixture handed NewAssessedProgress
// a nil forecast, so the badge's template branch and its overdue path had
// never executed under test. These two walks render the badge for real —
// once with a standing promise, once with an expired one.
// ---------------------------------------------------------------------------

func TestProgressBar_ForecastBadgeRendersThroughTheTemplate(t *testing.T) {
	eta := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Minute)
	html := renderProgress(t, "progress-bar", map[string]any{
		"Progress": view.NewAssessedProgress(percentPtr(60), 2, &eta, "lead"),
	})
	for _, want := range []string{
		`class="forecast"`,
		`title="Forecast by lead"`,
		`eta `,
		`eta 2026-`, // the eta text begins with the ISO date
		`· lead`,    // the by-line after the middle dot
	} {
		if !strings.Contains(html, want) {
			t.Errorf("badge markup is missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "overdue") {
		t.Errorf("a future forecast was rendered as overdue:\n%s", html)
	}
	if strings.Contains(html, "lead") == false {
		t.Error("the badge never names who made the promise")
	}
}

func TestProgressBar_ExpiredForecastCarriesTheOverdueMark(t *testing.T) {
	eta := time.Now().UTC().Add(-24 * time.Hour)
	html := renderProgress(t, "progress-bar", map[string]any{
		"Progress": view.NewAssessedProgress(percentPtr(60), 2, &eta, "lead"),
	})
	if !strings.Contains(html, `>overdue</span>`) {
		t.Errorf("an expired forecast carries no overdue mark:\n%s", html)
	}
	if !strings.Contains(html, `class="forecast"`) {
		t.Error("the expired forecast lost the badge itself")
	}

	// And the nil path still renders nothing: no forecast, no empty slot.
	bare := renderProgress(t, "progress-bar", map[string]any{
		"Progress": view.NewAssessedProgress(percentPtr(60), 2, nil, ""),
	})
	if strings.Contains(bare, `class="forecast"`) {
		t.Error("a nil forecast rendered a badge")
	}
}
