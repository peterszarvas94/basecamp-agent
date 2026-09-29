package main

import (
	"context"
	"os"
	"strconv"
	"testing"
)

func TestChatKeyPerAgent(t *testing.T) {
	if chatKey(123, "claude") == chatKey(123, "codex") {
		t.Fatal("bot job dedup keys collide")
	}
}

func TestCappedOutput(t *testing.T) {
	w := &cappedOutput{limit: 4}
	if n, err := w.Write([]byte("abcdef")); n != 6 || err != nil || w.String() != "abcd" || !w.truncated {
		t.Fatal("output not bounded")
	}
}

// Run with BASECAMP_CHAT_TEST_LINE=<Peter's chat line mentioning both bots> to exercise the live CLI fetch.
func TestChatJobLive(t *testing.T) {
	raw := os.Getenv("BASECAMP_CHAT_TEST_LINE")
	if raw == "" {
		t.Skip("set BASECAMP_CHAT_TEST_LINE for live Basecamp test")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{BasecampBin: "basecamp", WorkDir: "/home/tester/Projects", AllowedAccountID: 1234567,
		BotIDs: map[string]int64{"claude": 34567001, "codex": 34567002}, BotProfiles: map[string]string{"claude": "claude-bot", "codex": "codex-bot"}}}
	jobs, err := s.chatJob(context.Background(), 1, id, 34567003, 23456789)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].Agent != "claude" || jobs[1].Agent != "codex" || jobs[0].ChatRoom != 45678005 {
		t.Fatalf("unexpected chat jobs: %+v", jobs)
	}
}

func TestJoinIDsIsStableAcrossOrder(t *testing.T) {
	if got := joinIDs([]int64{49067163, 49042606, 0}); got != "49042606,49067163" {
		t.Fatalf("joinIDs = %q", got)
	}
	if joinIDs([]int64{2, 1}) != joinIDs([]int64{1, 2}) {
		t.Fatal("the same allowlist in another order must give the same feed filter")
	}
	if joinIDs(nil) != "" {
		t.Fatal("no IDs should give an empty filter")
	}
}
