package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func githubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubSignature(t *testing.T) {
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	sig := githubSignature("secret", body)
	if !validGitHubSignature("secret", body, sig) {
		t.Fatal("valid signature was rejected")
	}
	if validGitHubSignature("wrong", body, sig) || validGitHubSignature("secret", []byte("changed"), sig) {
		t.Fatal("invalid signature was accepted")
	}
}

func TestGitHubCardTransition(t *testing.T) {
	cards := CardsConfig{PROpen: "PR open", Done: "Done"}
	for _, tc := range []struct {
		action string
		merged bool
		column string
		move   bool
	}{
		{"opened", false, "PR open", true},
		{"reopened", false, "PR open", true},
		{"closed", true, "Done", true},
		{"closed", false, "", false},
		{"synchronize", false, "", false},
	} {
		column, move := githubCardTransition(tc.action, tc.merged, cards)
		if column != tc.column || move != tc.move {
			t.Errorf("githubCardTransition(%q, %t) = %q, %t; want %q, %t", tc.action, tc.merged, column, move, tc.column, tc.move)
		}
	}
}

func TestGitHubWebhookRequiresSignatureAndAnswersPing(t *testing.T) {
	s := &Server{cfg: Config{GitHub: GitHubConfig{WebhookSecret: "secret"}}}
	body := []byte(`{"zen":"hello"}`)

	unsigned := httptest.NewRequest(http.MethodPost, "/github/webhook", strings.NewReader(string(body)))
	unsigned.Header.Set("X-GitHub-Event", "ping")
	unsignedResponse := httptest.NewRecorder()
	s.handleGitHubWebhook(unsignedResponse, unsigned)
	if unsignedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook status = %d", unsignedResponse.Code)
	}

	signed := httptest.NewRequest(http.MethodPost, "/github/webhook", strings.NewReader(string(body)))
	signed.Header.Set("X-GitHub-Event", "ping")
	signed.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	signedResponse := httptest.NewRecorder()
	s.handleGitHubWebhook(signedResponse, signed)
	if signedResponse.Code != http.StatusOK || signedResponse.Body.String() != "pong\n" {
		t.Fatalf("signed ping = %d %q", signedResponse.Code, signedResponse.Body.String())
	}
}

func TestRootAnswersForPublicURLChecks(t *testing.T) {
	h := (&Server{}).routes()
	for path, want := range map[string]int{"/": http.StatusOK, "/healthz": http.StatusNotFound, "/missing": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Fatalf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}
