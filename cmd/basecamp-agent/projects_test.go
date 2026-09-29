package main

import "testing"

func TestDoneColumnIndexPrefersDoneType(t *testing.T) {
	columns := []basecampColumn{{Title: "Triage", Type: "Kanban::Triage"}, {Title: "In progress", Type: "Kanban::Column"}, {Title: "Shipped", Type: "Kanban::DoneColumn"}}
	if got := doneColumnIndex(columns, "Done"); got != 2 {
		t.Fatalf("doneColumnIndex = %d, want 2", got)
	}
	if got := columnIndex(columns, " in PROGRESS "); got != 1 {
		t.Fatalf("columnIndex = %d, want 1", got)
	}
	if got := columnIndex(columns, "PR open"); got != -1 {
		t.Fatalf("columnIndex = %d, want -1", got)
	}
}
