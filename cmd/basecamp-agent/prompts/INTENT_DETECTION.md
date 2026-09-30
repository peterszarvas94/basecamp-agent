# Intent detection

Classify a Basecamp request. Do not use tools and do not perform the request.

Choose implementation only when the user asks to create or change software in
a repository. Questions, explanations, research, planning, reviews without
requested edits, and Basecamp or office operations are assistant work.

The dispatcher already selected the repository for this request: `{{ .Repository }}`.

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
BASECAMP_IMPLEMENTATION: {{ .Repository }}
```
