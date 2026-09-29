package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cappedOutput discards excess CLI output instead of retaining an unbounded event feed.
type cappedOutput struct {
	bytes.Buffer
	mu        sync.Mutex
	limit     int
	truncated bool
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	remaining := w.limit - w.Len()
	if remaining <= 0 {
		w.truncated = true
		return n, nil
	}
	if n > remaining {
		w.truncated = true
		p = p[:remaining]
	}
	_, err := w.Buffer.Write(p)
	return n, err
}

func (s *Server) pollChat() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.pollChatOnce(); err != nil {
			log.Printf("chat poll: %v", err)
		}
		<-ticker.C
	}
}

func (s *Server) pollChatOnce() error {
	buckets, creators := joinIDs(s.cfg.AllowedProjectIDs), joinIDs(s.cfg.AllowedCreatorIDs)
	if buckets == "" || creators == "" {
		return errors.New("chat poll needs at least one allowlisted project and creator")
	}
	profile := defaultWebhookProfile(s.cfg)
	if profile == "" {
		return errors.New("chat poll needs a bot Basecamp profile")
	}
	// A feed position is bound to the filters it was minted for. When the
	// configured projects or requesters change, start again from now rather
	// than replaying history or failing on every poll.
	scope := buckets + "|" + creators
	s.mu.Lock()
	if s.state.ChatScope != scope {
		s.state.ChatScope, s.state.ChatPosition = scope, ""
	}
	position := s.state.ChatPosition
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := []string{"-P", profile, "events", "poll", "--buckets", buckets, "--types", "chat.line.created", "--creators", creators, "--all", "--max-pages", "2", "--json"}
	if position == "" {
		args = append(args, "--since", "now")
	} else {
		args = append(args, "--position", position)
	}
	cmd := exec.CommandContext(ctx, s.cfg.BasecampBin, args...)
	cmd.Dir = s.cfg.WorkDir
	cmd.Env = append(os.Environ(), "BASECAMP_NONINTERACTIVE=1")
	out := &cappedOutput{limit: 4 << 20}
	errOut := &cappedOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Run(); err != nil {
		// Exit 1 means the position was minted for other filters and exit 2
		// that it is no longer servable; either way, re-enter at now.
		var exit *exec.ExitError
		if position != "" && errors.As(err, &exit) && (exit.ExitCode() == 1 || exit.ExitCode() == 2) {
			s.mu.Lock()
			s.state.ChatPosition = ""
			s.saveState()
			s.mu.Unlock()
			return fmt.Errorf("events poll rejected the saved position; restarting from now: %s", strings.TrimSpace(errOut.String()))
		}
		return fmt.Errorf("events poll: %w: %s", err, errOut.String())
	}
	if out.truncated {
		return errors.New("event batch exceeds 4 MiB; cursor not advanced")
	}
	var response struct {
		Data struct {
			Position string `json:"position"`
			Events   []struct {
				ID          int64     `json:"id"`
				Kind        string    `json:"kind"`
				CreatorID   int64     `json:"creator_id"`
				BucketID    int64     `json:"bucket_id"`
				RecordingID int64     `json:"recording_id"`
				CreatedAt   time.Time `json:"created_at"`
			} `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		return fmt.Errorf("decode events: %w", err)
	}
	if response.Data.Position == "" {
		return errors.New("poll did not return durable position")
	}
	// First boot starts at the current position; never replay the account's entire history.
	if position == "" {
		s.mu.Lock()
		s.state.ChatPosition = response.Data.Position
		s.saveState()
		s.mu.Unlock()
		log.Print("chat polling started at current feed position")
		return nil
	}
	for _, item := range response.Data.Events {
		if item.Kind != "chat_lines_rich_text_created" && item.Kind != "chat_lines_text_created" {
			continue
		}
		if !containsInt64(s.cfg.AllowedProjectIDs, item.BucketID) || !containsInt64(s.cfg.AllowedCreatorIDs, item.CreatorID) ||
			item.CreatedAt.IsZero() || time.Since(item.CreatedAt) > 15*time.Minute || time.Until(item.CreatedAt) > 5*time.Minute {
			continue
		}
		jobs, err := s.chatJob(ctx, item.ID, item.RecordingID, item.CreatorID, item.BucketID)
		if err != nil {
			log.Printf("chat event=%d ignored: %v", item.ID, err)
			continue
		}
		for _, job := range jobs {
			key := chatKey(item.ID, job.Agent)
			s.mu.Lock()
			_, seen := s.state.Seen[key]
			s.mu.Unlock()
			if seen {
				continue
			}
			select {
			case s.jobs <- job:
				s.mu.Lock()
				s.state.Seen[key] = time.Now().UTC()
				s.pruneSeenLocked()
				s.saveState()
				s.mu.Unlock()
				// A boost on the line signals work without adding a chat line.
				s.boost(job.Event, job.Target, "👀", job.Profile)
			default:
				return errors.New("worker queue full; chat cursor not advanced")
			}
		}
	}
	s.mu.Lock()
	s.state.ChatPosition = response.Data.Position
	s.saveState()
	s.mu.Unlock()
	return nil
}

func chatKey(eventID int64, agent string) string {
	return "chat:" + strconv.FormatInt(eventID, 10) + ":" + agent
}

func (s *Server) chatJob(ctx context.Context, eventID, lineID, creatorID, projectID int64) ([]Job, error) {
	fetched, err := s.basecampJSON(ctx, "chat", "line", strconv.FormatInt(lineID, 10), "--project", strconv.FormatInt(projectID, 10), "--json")
	if err != nil {
		return nil, fmt.Errorf("fetch chat line: %w", err)
	}
	root, _ := fetched.(map[string]any)
	data, _ := root["data"].(map[string]any)
	if data == nil || int64FromAny(data["id"]) != lineID || data["status"] != "active" {
		return nil, errors.New("chat line missing or inactive")
	}
	creator, _ := data["creator"].(map[string]any)
	bucket, _ := data["bucket"].(map[string]any)
	parent, _ := data["parent"].(map[string]any)
	roomID := int64FromAny(parent["id"])
	url, _ := data["url"].(string)
	if int64FromAny(creator["id"]) != creatorID || int64FromAny(bucket["id"]) != projectID || roomID == 0 || parent["type"] != "Chat::Transcript" ||
		!strings.Contains(url, fmt.Sprintf("/%d/buckets/%d/chats/%d/lines/%d.json", s.cfg.AllowedAccountID, projectID, roomID, lineID)) {
		return nil, errors.New("chat line author, room or account mismatch")
	}
	var agents []string
	for agent, id := range s.cfg.BotIDs {
		if id != 0 && s.cfg.BotProfiles[agent] != "" && mentioned(data, id) {
			agents = append(agents, agent)
		}
	}
	if len(agents) == 0 {
		return nil, errors.New("no structured bot mention")
	}
	sort.Strings(agents)
	name, _ := creator["name"].(string)
	ev := WebhookEvent{ID: eventID, Kind: "chat_lines_rich_text_created", Creator: Person{ID: creatorID, Name: name}}
	ev.Recording.ID = lineID
	ev.Recording.URL = url
	ev.Recording.Bucket.ID = projectID
	jobs := make([]Job, 0, len(agents))
	for _, agent := range agents {
		jobs = append(jobs, Job{Event: ev, Agent: agent, Profile: s.cfg.BotProfiles[agent], Target: url, ChatRoom: roomID, Instruction: collectText(fetched)})
	}
	return jobs, nil
}

// replayChatEvent is a local recovery command for an already-delivered feed event.
// It does not accept arbitrary line IDs: the event must exist in Basecamp's feed.
func (s *Server) replayChatEvent(eventID int64) error {
	buckets, creators := joinIDs(s.cfg.AllowedProjectIDs), joinIDs(s.cfg.AllowedCreatorIDs)
	if eventID <= 0 || buckets == "" || creators == "" {
		return errors.New("invalid replay scope")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	v, err := s.basecampJSON(ctx, "events", "poll", "--since", strconv.FormatInt(eventID-1, 10), "--buckets", buckets, "--creators", creators, "--types", "chat.line.created", "--all", "--max-pages", "2", "--json")
	if err != nil {
		return err
	}
	root, _ := v.(map[string]any)
	data, _ := root["data"].(map[string]any)
	events, _ := data["events"].([]any)
	for _, item := range events {
		e, _ := item.(map[string]any)
		if int64FromAny(e["id"]) != eventID {
			continue
		}
		projectID, creatorID := int64FromAny(e["bucket_id"]), int64FromAny(e["creator_id"])
		if !containsInt64(s.cfg.AllowedProjectIDs, projectID) || !containsInt64(s.cfg.AllowedCreatorIDs, creatorID) || e["event_type"] != "chat.line.created" {
			return errors.New("event outside allowed scope")
		}
		created, err := time.Parse(time.RFC3339Nano, fmt.Sprint(e["created_at"]))
		if err != nil || time.Since(created) > 2*time.Hour || time.Until(created) > 5*time.Minute {
			return errors.New("replay event outside 2-hour window")
		}
		jobs, err := s.chatJob(ctx, eventID, int64FromAny(e["recording_id"]), creatorID, projectID)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			key := chatKey(eventID, job.Agent)
			s.mu.Lock()
			_, seen := s.state.Seen[key]
			s.mu.Unlock()
			if seen {
				log.Printf("event=%d agent=%s already queued; skipping", eventID, job.Agent)
				continue
			}
			s.mu.Lock()
			s.state.Seen[key] = time.Now().UTC()
			s.pruneSeenLocked()
			s.saveState()
			s.mu.Unlock()
			s.boost(job.Event, job.Target, "👀", job.Profile)
			s.executeJob(job)
		}
		return nil
	}
	return errors.New("event not found in Basecamp feed")
}

func (s *Server) chatPost(job Job, content string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := s.basecampCombined(ctx, "-P", job.Profile, "chat", "post", "-", "--room", strconv.FormatInt(job.ChatRoom, 10), "--project", strconv.FormatInt(job.Event.Recording.Bucket.ID, 10), "--json", content+"\n")
	return err
}

// joinIDs renders IDs as the sorted, comma-separated list the event feed
// filters take, so the same allowlist always yields the same filter.
func joinIDs(ids []int64) string {
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	parts := make([]string, 0, len(sorted))
	for _, id := range sorted {
		if id != 0 {
			parts = append(parts, strconv.FormatInt(id, 10))
		}
	}
	return strings.Join(parts, ",")
}
