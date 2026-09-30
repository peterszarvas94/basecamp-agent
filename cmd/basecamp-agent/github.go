package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

const maxGitHubWebhookBytes = 1 << 20

type githubPullRequestEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		HTMLURL string `json:"html_url"`
		Merged  bool   `json:"merged"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func validGitHubSignature(secret string, body []byte, header string) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

func githubCardTransition(action string, merged bool, cards CardsConfig) (string, bool) {
	switch action {
	case "opened", "reopened":
		return cards.PROpen, true
	case "closed":
		if merged {
			return cards.Done, true
		}
	}
	return "", false
}

// jobForPR resolves a signed GitHub event back to the durable job record that
// opened the pull request. The PR URL is recorded before the dispatcher reports
// success to Basecamp, so it remains available across service restarts.
func (s *Server) jobForPR(prURL string) (Job, error) {
	for _, st := range s.readStatuses() {
		if st.PRURL != prURL {
			continue
		}
		job, err := s.readJobRecord(st.ID)
		if err != nil {
			projectID, recordingID := idsFromBasecampURL(st.Target)
			if projectID == 0 || recordingID == 0 {
				return Job{}, fmt.Errorf("job %s has no usable Basecamp target", st.ID)
			}
			job = Job{Agent: st.Agent, Profile: s.cfg.BotProfiles[st.Agent], Target: st.Target}
			job.Event.ID = st.EventID
			job.Event.Recording.ID = recordingID
			job.Event.Recording.URL = st.Target
			job.Event.Recording.Bucket.ID = projectID
		}
		if job.Profile == "" {
			job.Profile = s.cfg.BotProfiles[job.Agent]
		}
		if job.Profile == "" {
			return Job{}, fmt.Errorf("job %s has no Basecamp profile", st.ID)
		}
		return job, nil
	}
	return Job{}, errors.New("pull request is not associated with a retained job")
}

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.GitHub.WebhookSecret == "" {
		http.Error(w, "GitHub webhook is not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGitHubWebhookBytes))
	if err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if !validGitHubSignature(s.cfg.GitHub.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("X-GitHub-Event") == "ping" {
		_, _ = io.WriteString(w, "pong\n")
		return
	}
	if r.Header.Get("X-GitHub-Event") != "pull_request" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var event githubPullRequestEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid pull request payload", http.StatusBadRequest)
		return
	}
	_, shouldHandle := githubCardTransition(event.Action, event.PullRequest.Merged, s.cfg.Cards)
	if !shouldHandle {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	expectedPrefix := "https://github.com/" + event.Repository.FullName + "/pull/"
	if event.Repository.FullName == "" || !strings.HasPrefix(event.PullRequest.HTMLURL, expectedPrefix) {
		http.Error(w, "repository and pull request URL do not match", http.StatusBadRequest)
		return
	}
	job, err := s.jobForPR(event.PullRequest.HTMLURL)
	if err != nil {
		// The opened event can beat the status write by a fraction of a second;
		// executeJob performs that same PR-open move after publishing the status.
		if event.Action == "opened" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if isTodoURL(job.Target) {
		// Todos have no PR-open state. Complete them only after a merge; opened
		// and reopened events are acknowledged without changing the todo.
		if event.Action == "closed" && event.PullRequest.Merged {
			if err := s.completeTodo(job.Event, job.Target, job.Profile); err != nil {
				http.Error(w, "Basecamp todo completion failed", http.StatusBadGateway)
				return
			}
			log.Printf("github pull request merged pr=%s completed_todo=%s", event.PullRequest.HTMLURL, job.Target)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !isCardURL(job.Target) {
		http.Error(w, "associated Basecamp item is not a card or todo", http.StatusConflict)
		return
	}
	column, _ := githubCardTransition(event.Action, event.PullRequest.Merged, s.cfg.Cards)
	if err := s.moveCard(job.Event, job.Target, column, job.Profile); err != nil {
		http.Error(w, "Basecamp card move failed", http.StatusBadGateway)
		return
	}
	log.Printf("github pull request action=%s merged=%t pr=%s moved_card=%s column=%q", event.Action, event.PullRequest.Merged, event.PullRequest.HTMLURL, job.Target, column)
	w.WriteHeader(http.StatusNoContent)
}
