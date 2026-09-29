package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// replayTodoAssignment recovers a missed delivery. Only a real recent event
// from the authenticated Basecamp feed, matching the allowlist, is dispatched.
// It doesn't write state: the running service may be updating its cursor.
func (s *Server) replayTodoAssignment(eventID int64) error {
	if eventID <= 0 || len(s.cfg.AllowedProjectIDs) != 1 || len(s.cfg.AllowedCreatorIDs) != 1 || s.cfg.AllowedAccountID == 0 {
		return errors.New("invalid replay scope")
	}
	projectID, creatorID := s.cfg.AllowedProjectIDs[0], s.cfg.AllowedCreatorIDs[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	v, err := s.basecampJSON(ctx, "events", "poll", "--since", strconv.FormatInt(eventID-1, 10), "--buckets", strconv.FormatInt(projectID, 10), "--creators", strconv.FormatInt(creatorID, 10), "--all", "--max-pages", "2", "--json")
	if err != nil {
		return err
	}
	root, _ := v.(map[string]any)
	data, _ := root["data"].(map[string]any)
	items, _ := data["events"].([]any)
	for _, item := range items {
		e, _ := item.(map[string]any)
		if int64FromAny(e["id"]) != eventID {
			continue
		}
		if e["kind"] != "todo_assignment_changed" || int64FromAny(e["creator_id"]) != creatorID || int64FromAny(e["bucket_id"]) != projectID {
			return errors.New("event is not an allowed todo assignment")
		}
		created, err := time.Parse(time.RFC3339Nano, fmt.Sprint(e["created_at"]))
		if err != nil || time.Since(created) > 2*time.Hour || time.Until(created) > 5*time.Minute {
			return errors.New("replay event outside 2-hour window")
		}
		todoID := int64FromAny(e["recording_id"])
		if todoID <= 0 {
			return errors.New("missing todo recording id")
		}
		url := fmt.Sprintf("https://3.basecampapi.com/%d/buckets/%d/todos/%d.json", s.cfg.AllowedAccountID, projectID, todoID)
		ev := WebhookEvent{ID: eventID, Kind: "todo_assignment_changed", CreatedAt: created, Creator: Person{ID: creatorID}}
		ev.Recording.ID, ev.Recording.URL, ev.Recording.Bucket.ID = todoID, url, projectID
		job, err := s.buildJob(ctx, ev)
		if err != nil {
			return err
		}
		s.boost(ev, url, "👀", job.Profile)
		s.executeJob(job)
		return nil
	}
	return errors.New("event not found in Basecamp feed")
}
