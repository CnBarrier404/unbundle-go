package session

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"regexp"
	"strings"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

var environmentHeader = regexp.MustCompile(`^\s*(?:<total_tokens>[0-9]+ tokens left</total_tokens>\s*)*# Environment(?: update)?\r?\n`)

// The official client removes a redundant "cd <cwd> && " from Bash inputs.
// Match that receipt only against the caller directory known BEFORE the call.
// This never changes the native call, submits history or executes a command.
func assistantReceiptEqual(expected, actual messages.Message, preceding []messages.Message) bool {
	if jsonutil.Equal(expected, actual) {
		return true
	}
	if expected.Role != "assistant" || actual.Role != "assistant" || expected.Content.Kind() != '[' || actual.Content.Kind() != '[' {
		return false
	}
	prefixes := bashDirectoryPrefixes(callerDirectory(preceding))
	if len(prefixes) == 0 {
		return false
	}
	a, errA := messages.Blocks(expected.Content)
	b, errB := messages.Blocks(actual.Content)
	if errA != nil || errB != nil || len(a) != len(b) {
		return false
	}
	for i := range a {
		if jsonutil.Equal(a[i], b[i]) {
			continue
		}
		var original, receipt messages.ToolUseBlock
		if json.Unmarshal(a[i], &original) != nil || json.Unmarshal(b[i], &receipt) != nil ||
			original.Type != "tool_use" || receipt.Type != "tool_use" || original.Name != "Bash" || receipt.Name != "Bash" || original.ID != receipt.ID {
			return false
		}
		var input, returned map[string]jsontext.Value
		if json.Unmarshal(original.Input, &input) != nil || json.Unmarshal(receipt.Input, &returned) != nil {
			return false
		}
		var command, shortened string
		if json.Unmarshal(input["command"], &command) != nil || json.Unmarshal(returned["command"], &shortened) != nil || shortened == "" {
			return false
		}
		matched := false
		for _, prefix := range prefixes {
			if command == prefix+shortened {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
		returned["command"] = input["command"]
		receipt.Input, _ = json.Marshal(returned)
		// Preserve every other block field, including any caller metadata.
		var fields map[string]jsontext.Value
		if json.Unmarshal(b[i], &fields) != nil {
			return false
		}
		fields["input"] = receipt.Input
		b[i], _ = json.Marshal(fields)
	}
	actual.Content, _ = json.Marshal(b)
	return jsonutil.Equal(expected, actual)
}

func callerDirectory(history []messages.Message) string {
	var cwd string
	for _, message := range history {
		if message.Role != "system" {
			continue
		}
		blocks, err := messages.Blocks(message.Content)
		if err != nil || len(blocks) != 1 {
			continue
		}
		var block messages.TextBlock
		if json.Unmarshal(blocks[0], &block) != nil || block.Type != "text" || block.Text == nil {
			continue
		}
		text := *block.Text
		header := environmentHeader.FindString(text)
		if header == "" {
			continue
		}
		// A malformed or ambiguous update invalidates the old directory too.
		cwd = ""
		const field = " - Primary working directory: "
		body := text[len(header):]
		const preamble = "You have been invoked in the following environment:"
		if !strings.Contains(header, "# Environment update") {
			line, rest, ok := strings.Cut(body, "\n")
			if ok && strings.TrimRight(line, " \t\r") == preamble {
				body = rest
			}
		}
		if !strings.HasPrefix(body, field) || strings.Count(text, field) != 1 {
			continue
		}
		line, _, _ := strings.Cut(body[len(field):], "\n")
		line = strings.TrimSuffix(line, "\r")
		if strings.Contains(header, "# Environment update") {
			current, previous, ok := strings.Cut(line, " (was ")
			if !ok || !strings.HasSuffix(previous, ")") || len(bashDirectoryPrefixes(strings.TrimSuffix(previous, ")"))) == 0 {
				continue
			}
			line = current
		}
		if len(bashDirectoryPrefixes(line)) > 0 {
			cwd = line
		}
	}
	return cwd
}

// Only literal, unquoted absolute paths with no shell expansion or dot segments
// are supported. Compare paths as given; normalize drive case only.
func bashDirectoryPrefixes(cwd string) []string {
	windows := len(cwd) >= 3 && (cwd[0] >= 'A' && cwd[0] <= 'Z' || cwd[0] >= 'a' && cwd[0] <= 'z') && cwd[1] == ':' && (cwd[2] == '/' || cwd[2] == '\\')
	path := cwd
	if windows {
		path = "/" + strings.ReplaceAll(cwd[3:], `\`, "/")
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil
	}
	for _, c := range path {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/_-.~", c)) {
			return nil
		}
	}
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part == "." || part == ".." || part == "" && path != "/" {
			return nil
		}
	}
	if !windows {
		return []string{"cd " + cwd + " && "}
	}
	drive := strings.ToLower(cwd[:1])
	return []string{"cd /" + drive + path + " && ", "cd " + drive + ":" + path + " && ", "cd " + strings.ToUpper(drive) + ":" + path + " && "}
}
