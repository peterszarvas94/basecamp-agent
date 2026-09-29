package main

import (
	"strings"
	"testing"
)

func TestMentionRequiresStructuredPerson(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`<p>@Claude please help</p>`, false},
		{`<p>data-avatar-for-person-id="34567001"</p>`, false},
		{`<bc-attachment content-type="application/vnd.basecamp.mention"><img data-avatar-for-person-id="34567001"></bc-attachment>`, true},
		{`<bc-attachment content-type="application/vnd.basecamp.mention"><img data-avatar-for-person-id="34567002"></bc-attachment>`, false},
	}
	for _, tc := range cases {
		if got := mentioned(map[string]any{"content": tc.body}, 34567001); got != tc.want {
			t.Errorf("mentioned(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestAssigneeRequiresMatchingPerson(t *testing.T) {
	data := map[string]any{"assignees": []any{map[string]any{"id": float64(34567002)}}}
	if !hasAssignee(data, 34567002) || hasAssignee(data, 34567001) {
		t.Fatal("incorrect assignee match")
	}
	if !hasID([]any{float64(34567002)}, 34567002) || hasID([]any{float64(34567002)}, 34567001) {
		t.Fatal("incorrect added-person match")
	}
}

func TestCommentReplyToParent(t *testing.T) {
	parent := "https://3.basecampapi.com/1234567/buckets/23456789/messages/123.json"
	v := map[string]any{"data": map[string]any{"type": "Comment", "parent": map[string]any{"url": parent}}}
	if got := replyTarget(v, "comment-url"); got != parent {
		t.Fatalf("reply target = %q", got)
	}
}

func TestCardURLDetection(t *testing.T) {
	cases := map[string]bool{
		"https://3.basecampapi.com/1234567/buckets/23456789/card_tables/cards/45678002.json": true,
		"https://app.basecamp.com/1234567/buckets/23456789/card_tables/cards/45678002":       true,
		"https://3.basecampapi.com/1234567/buckets/23456789/todos/123.json":                  false,
		"https://3.basecampapi.com/1234567/buckets/23456789/chats/456/lines/789.json":        false,
		"": false,
	}
	for target, want := range cases {
		if got := isCardURL(target); got != want {
			t.Errorf("isCardURL(%q) = %v, want %v", target, got, want)
		}
	}
}

func TestColumnTitleMatchIgnoresCaseAndPadding(t *testing.T) {
	if !sameColumn(" In Progress ", "in progress") {
		t.Error("expected padded, differently cased titles to match")
	}
	if sameColumn("In progress", "Done") {
		t.Error("expected distinct titles not to match")
	}
}

func TestNestedMapWalksOrGivesUp(t *testing.T) {
	v := map[string]any{"data": map[string]any{"parent": map[string]any{"id": float64(45678001)}}}
	if got := nestedMap(v, "data", "parent"); got == nil || got["id"] != float64(45678001) {
		t.Fatalf("nestedMap did not reach the parent: %#v", got)
	}
	if nestedMap(v, "data", "missing") != nil {
		t.Error("expected nil for a missing key")
	}
	if nestedMap("not an object", "data") != nil {
		t.Error("expected nil for a non-object")
	}
}

func TestInstructionTextIsNotRepeated(t *testing.T) {
	// A Campfire line carries the same sentence in title and in content.
	line := map[string]any{"data": map[string]any{
		"title":   "Codex what should be the next card to implement",
		"content": `<p dir="auto"><bc-attachment><figure><figcaption>Codex</figcaption></figure></bc-attachment> what should be the next card to implement</p>`,
	}}
	if got := collectText(line); got != "Codex what should be the next card to implement" {
		t.Fatalf("collectText = %q", got)
	}
	both := map[string]any{"data": map[string]any{"title": "Fix the header", "description": "<p>Make it smaller</p>"}}
	if got := collectText(both); got != "Fix the header\nMake it smaller" {
		t.Fatalf("distinct fields were not both kept: %q", got)
	}
}

func TestFailureMessageLeadsWithTheCause(t *testing.T) {
	out := strings.Join([]string{
		"Reading additional input from stdin...",
		"OpenAI Codex v0.158.0",
		"--------",
		"workdir: /home/tester/.local/state/basecamp-webhook-agent/worktrees/restaurant/codex/bc-1",
		"approval: never",
		"--------",
		"user",
		"You are running as a local worker launched by a Basecamp webhook dispatcher.",
		"warning: `--dangerously-bypass-hook-trust` is enabled.",
		"hook: SessionStart",
		"hook: SessionStart Completed",
		"ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage or try again at 11:36 AM.",
		"",
		"ERROR: exit status 1",
		"Worktree preserved for inspection: /tmp/wt",
	}, "\n")

	reason := failureReason(out)
	want := "You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage or try again at 11:36 AM."
	if reason != want {
		t.Fatalf("failureReason = %q", reason)
	}

	job := Job{Agent: "codex", JobID: "56789001-codex", Attempt: 1, Event: WebhookEvent{ID: 56789001}}
	msg := failureMessage(job, "failed", reason, out)
	if !strings.HasPrefix(msg, "**`codex` hit its usage limit**") {
		t.Errorf("headline missing, got: %s", msg)
	}
	if !strings.Contains(msg, want) {
		t.Error("the error text itself is missing")
	}
	if strings.Contains(msg, "OpenAI Codex v0.158.0") || strings.Contains(msg, "local worker") {
		t.Error("prompt echo leaked into the Basecamp message")
	}
	if strings.Contains(msg, "```") {
		t.Error("a recognised failure should not carry a log dump")
	}
}

func TestUnknownFailureKeepsTheLogTail(t *testing.T) {
	out := "step one\nstep two\nsome unexpected explosion\nERROR: exit status 1\n"
	reason := failureReason(out)
	if reason != "some unexpected explosion" {
		t.Fatalf("failureReason = %q", reason)
	}
	msg := failureMessage(Job{Agent: "claude", JobID: "j1"}, "failed", reason, out)
	if !strings.HasPrefix(msg, "**`claude` job failed**") {
		t.Errorf("headline = %s", msg)
	}
	if !strings.Contains(msg, "```text") {
		t.Error("an unrecognised failure should include the log tail")
	}
}

func TestStoppedJobSaysStopped(t *testing.T) {
	msg := failureMessage(Job{Agent: "claude", JobID: "j2"}, "stopped", "agent stopped by operator", "")
	if !strings.HasPrefix(msg, "**`claude` was stopped**") {
		t.Errorf("headline = %s", msg)
	}
}

func TestDispatcherStaysQuietWhenTheAgentAlreadyPosted(t *testing.T) {
	narrated := "Posted to the Campfire room. Summary of what I recommended:\n\n- Next: make images clickable\n\nBASECAMP_RESPONSE_POSTED"
	if !postedDirectly(narrated) {
		t.Error("a narrated run ending in the sentinel should count as posted")
	}
	if !postedDirectly("BASECAMP_RESPONSE_POSTED\n\n") {
		t.Error("a bare sentinel should count as posted")
	}
	quoted := "The rules said to answer BASECAMP_RESPONSE_POSTED but I could not.\nHere is my answer instead."
	if postedDirectly(quoted) {
		t.Error("a mid-output mention must not silence the dispatcher")
	}
	if postedDirectly("") {
		t.Error("empty output is not a direct post")
	}
	if got := stripSentinel(narrated); strings.Contains(got, "BASECAMP_RESPONSE_POSTED") {
		t.Errorf("sentinel leaked into a posted message: %q", got)
	}
}

func TestBasecampLinksPointAtTheApp(t *testing.T) {
	want := "https://app.basecamp.com/1234567/buckets/23456789/card_tables/cards/45678003"
	for _, raw := range []string{
		"https://3.basecampapi.com/1234567/buckets/23456789/card_tables/cards/45678003.json",
		"https://3.basecamp.com/1234567/buckets/23456789/card_tables/cards/45678003",
		want,
	} {
		if got := basecampAppURL(raw); got != want {
			t.Errorf("basecampAppURL(%q) = %q", raw, got)
		}
	}
	if got := basecampAppURL("https://github.com/peterszarvas94/restaurant/pull/5"); got != "https://github.com/peterszarvas94/restaurant/pull/5" {
		t.Errorf("non-Basecamp URL was rewritten: %q", got)
	}
}

func TestSupersededTargetComparesStartTimes(t *testing.T) {
	card := "https://3.basecampapi.com/1234567/buckets/23456789/card_tables/cards/45678004.json"
	older := JobStatus{ID: "a", Target: card, StartedAt: "2026-09-28T10:00:00Z"}
	newer := JobStatus{ID: "b", Target: card, StartedAt: "2026-09-28T11:00:00Z"}
	unrelated := JobStatus{ID: "c", Target: "https://3.basecampapi.com/1234567/buckets/23456789/card_tables/cards/999.json", StartedAt: "2026-09-28T12:00:00Z"}
	all := []JobStatus{older, newer, unrelated}
	if !supersededTarget(older, all) {
		t.Error("a job with a later run on the same card is superseded")
	}
	if supersededTarget(newer, all) {
		t.Error("the newest job on a card is not superseded")
	}
	if supersededTarget(JobStatus{ID: "d", Target: "", StartedAt: "2026-09-28T09:00:00Z"}, all) {
		t.Error("a job without a target cannot be superseded")
	}
}

func TestCancelledJobIsTerminal(t *testing.T) {
	if isActiveJobState("cancelled") {
		t.Error("a cancelled job is not active")
	}
	label, badge := statusPresentation("cancelled")
	if label != "cancelled" || badge != "badge-neutral" {
		t.Errorf("statusPresentation(cancelled) = %q, %q", label, badge)
	}
	if dot := statusDotClass("cancelled"); dot != "status-neutral" {
		t.Errorf("statusDotClass(cancelled) = %q", dot)
	}
	// A cancelled run must not pulse like something still working.
	if strings.Contains(statusDotClass("cancelled"), "animate-pulse") {
		t.Error("cancelled should not animate")
	}
}

func TestFailedAndStoppedJobsCanBeCancelled(t *testing.T) {
	for _, state := range []string{"failed", "stopped"} {
		if !isCancellableJobState(state) {
			t.Errorf("%s job should be cancellable", state)
		}
	}
	for _, state := range []string{"preparing", "running", "publishing", "completed", "cancelled"} {
		if isCancellableJobState(state) {
			t.Errorf("%s job should not be cancellable", state)
		}
	}
}

func TestJobStatesUseColorInsteadOfRowOpacity(t *testing.T) {
	if strings.Contains(detailCSS(), "opacity") {
		t.Error("job rows must not be dimmed for any state")
	}
	want := map[string]string{
		"completed":  "badge-success",
		"failed":     "badge-error",
		"stopped":    "badge-warning",
		"cancelled":  "badge-neutral",
		"preparing":  "badge-secondary",
		"running":    "badge-info",
		"publishing": "badge-primary",
	}
	for state, wantBadge := range want {
		_, got := statusPresentation(state)
		if got != wantBadge {
			t.Errorf("statusPresentation(%q) badge = %q, want %q", state, got, wantBadge)
		}
	}
}

func TestOutputPanelScriptSupportsFollowAndCopy(t *testing.T) {
	js := outputPanelJS()
	for _, required := range []string{"nearBottom", "following", "scrollToBottom", "navigator.clipboard.writeText", "copyJobOutput"} {
		if !strings.Contains(js, required) {
			t.Errorf("output panel script is missing %q", required)
		}
	}
}
