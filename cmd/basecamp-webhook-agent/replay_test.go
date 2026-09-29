package main

import (
	"context"
	"os"
	"strconv"
	"testing"
)

func TestAssignmentActorMayDifferFromTodoCreator(t *testing.T) {
	if os.Getenv("BASECAMP_ASSIGNMENT_TEST") != "1" {
		t.Skip("set BASECAMP_ASSIGNMENT_TEST=1 for live Basecamp assignment fetch")
	}
	s := &Server{cfg: Config{BasecampBin: "basecamp", WorkDir: "/home/tester/Projects", BotIDs: map[string]int64{"codex": 34567002, "claude": 34567001}, BotProfiles: map[string]string{"codex": "codex-bot", "claude": "claude-bot"}}}
	for _, tc := range []struct {
		eventID, todoID int64
		agent           string
	}{{56789002, 45678006, "codex"}, {56789003, 45678007, "claude"}} {
		ev := WebhookEvent{ID: tc.eventID, Kind: "todo_assignment_changed", Creator: Person{ID: 34567003}}
		ev.Recording.ID = tc.todoID
		ev.Recording.Bucket.ID = 23456789
		ev.Recording.URL = "https://3.basecampapi.com/1234567/buckets/23456789/todos/" + strconv.FormatInt(tc.todoID, 10) + ".json"
		job, err := s.buildJob(context.Background(), ev)
		if err != nil || job.Agent != tc.agent || job.Title == "" {
			t.Fatalf("buildJob(%d)=%+v, %v", tc.eventID, job, err)
		}
	}
}
