package streamfmt

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"unicode"
)

var toolAliases = map[string]string{
	"read":               "Read",
	"readfile":           "Read",
	"edit":               "Edit",
	"multiedit":          "Edit",
	"replace":            "Edit",
	"searchreplace":      "Edit",
	"write":              "Write",
	"writefile":          "Write",
	"bash":               "Bash",
	"runshellcommand":    "Bash",
	"runterminalcmd":     "Bash",
	"runterminalcommand": "Bash",
	"shell":              "Bash",
	"grep":               "Grep",
	"search":             "Grep",
	"glob":               "Glob",
	"list":               "List",
	"listdir":            "List",
	"ls":                 "List",
	"webfetch":           "WebFetch",
	"websearch":          "WebSearch",
	"fetch":              "WebFetch",
	"searchtool":         "SearchTool",
	"usetool":            "UseTool",
	"task":               "Task",
}

func toolEvent(name string, input jsontext.Value) Event {
	name = sanitizeControl(name)
	display := canonicalToolName(name)
	if display == "" {
		display = name
	}
	event := Event{Kind: EventTool, Name: display}

	var raw map[string]jsontext.Value
	if err := json.Unmarshal(input, &raw); err != nil || len(raw) == 0 {
		return event
	}
	fields := normalizeFields(raw)

	switch display {
	case "Read", "Edit", "Write":
		event.Arg = firstJSONField(
			fields, "filepath", "path", "targetfile", "file",
		)
	case "Bash":
		event.Arg = jsonString(fields["command"])
		if len(event.Arg) > 80 {
			event.Arg = event.Arg[:77] + "..."
		}
	case "Grep":
		pattern := jsonString(fields["pattern"])
		path := jsonString(fields["path"])
		if path != "" {
			event.Arg = pattern + "  " + path
		} else {
			event.Arg = pattern
		}
	case "Glob":
		event.Arg = jsonString(fields["pattern"])
	case "List":
		event.Arg = firstJSONField(fields, "path", "targetdirectory")
	case "WebFetch":
		event.Arg = jsonString(fields["url"])
	}
	return event
}

func firstJSONField(
	fields map[string]jsontext.Value, keys ...string,
) string {
	for _, key := range keys {
		if value := jsonString(fields[key]); value != "" {
			return value
		}
	}
	return ""
}

func normalizeToolKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '_' || r == '-' {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func canonicalToolName(name string) string {
	return toolAliases[normalizeToolKey(name)]
}

func normalizeFields(
	fields map[string]jsontext.Value,
) map[string]jsontext.Value {
	out := make(map[string]jsontext.Value, len(fields))
	for key, value := range fields {
		out[normalizeToolKey(key)] = value
	}
	return out
}

func jsonString(raw jsontext.Value) string {
	if raw == nil {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return strings.Trim(string(raw), `"`)
	}
	return value
}

func jsonStringField(raw jsontext.Value) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
