package main

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed prompts/*.md
var promptFiles embed.FS

var prompts = template.Must(template.New("prompts").Funcs(template.FuncMap{
	"join": strings.Join,
}).ParseFS(promptFiles, "prompts/*.md"))

type intentPromptData struct {
	Repository  string
	Title       string
	Instruction string
}

type agentTaskPromptData struct {
	ProjectID        int64
	Requester        string
	TriggeringItem   string
	Target           string
	ShareableURL     string
	Mode             string
	Agent            string
	Instruction      string
	Restart          bool
	PreviousJobID    string
	PreviousSummary  string
	Repo             string
	Worktree         string
	LocalBranch      string
	BaseBranch       string
	Upstream         string
	Branch           string
	ExistingPR       string
	CardMove         bool
	InProgressColumn string
	PROpenColumn     string
	DoneColumn       string
	FailedColumn     string
	ChatRoom         int64
}

func renderPrompt(name string, data any) (string, error) {
	var b bytes.Buffer
	if err := prompts.ExecuteTemplate(&b, name, data); err != nil {
		return "", fmt.Errorf("render prompt %s: %w", name, err)
	}
	return strings.TrimSpace(b.String()), nil
}
