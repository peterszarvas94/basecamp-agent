# Intent detection

Classify a Basecamp request. Do not use tools and do not perform the request.

Choose implementation only when the user asks to create or change software in
a repository. Questions, explanations, research, planning, reviews without
requested edits, and Basecamp or office operations are assistant work. If
implementation is requested but no repository can be identified, choose
assistant so the full agent can ask a clarifying question.

## Allowed repositories

{{- range .Repositories }}
- `{{ .Name }}`{{ if .Aliases }} (aliases: {{ join .Aliases ", " }}){{ end }}
{{- end }}

Project default repository: {{ if .DefaultRepository }}`{{ .DefaultRepository }}`{{ else }}none{{ end }}

## Request

Title: {{ .Title }}

{{ .Instruction }}

## Output

Return exactly one line:

```text
BASECAMP_ASSISTANT
```

or:

```text
BASECAMP_IMPLEMENTATION: exact-repository-name
```
