package plugin

import (
	"bytes"
	"encoding/json"
)

// streamTerminalDetector recognizes upstream completion before EOF while
// retaining only one bounded SSE field. CLIProxyAPI can concatenate stripped
// SSE fields, so it follows the same tolerant event:/data: boundary policy as
// usage parsing.
type streamTerminalDetector struct {
	line            []byte
	skipLine        bool
	finished        bool
	expectedChoices int
	finishedChoices map[int]bool
	jsonDepth       int
	jsonInString    bool
	jsonEscape      bool
}

func newStreamTerminalDetector(request []byte) streamTerminalDetector {
	var opts struct {
		N int `json:"n"`
	}
	_ = json.Unmarshal(request, &opts)
	if opts.N < 1 {
		opts.N = 1
	}
	return streamTerminalDetector{expectedChoices: opts.N}
}

func (d *streamTerminalDetector) Feed(payload []byte) bool {
	if d.finished {
		return false
	}
	// Some host chunks concatenate complete SSE fields after stripping the blank
	// delimiter. Recover only markers outside JSON so model content containing
	// literal "event:" or "data:" does not corrupt a terminal frame.
	payload = d.normalizeSSEBoundaries(payload)
	for len(payload) > 0 {
		i := bytes.IndexByte(payload, '\n')
		part := payload
		if i >= 0 {
			part = payload[:i]
		}
		if !d.skipLine {
			if len(d.line)+len(part) > 64*1024 {
				d.line = nil
				d.skipLine = true
			} else {
				d.line = append(d.line, part...)
			}
		}
		// An event header is only authoritative after its line delimiter. Without
		// it, `event: error` may be the prefix of a longer non-terminal name.
		completeField := i >= 0
		line := bytes.TrimSpace(d.line)
		// JSON data can be split at any byte boundary. Only a complete field may
		// update choice state; [DONE] is the lone safe no-delimiter shortcut.
		exactDone := bytes.Equal(line, []byte("data: [DONE]")) || bytes.Equal(line, []byte("[DONE]"))
		completeRawJSON := !bytes.HasPrefix(line, []byte("data:")) && json.Valid(line)
		if !d.skipLine && (completeField || exactDone || completeRawJSON) && d.terminalSSELine(line) {
			d.finished = true
			d.line = nil
			return true
		}
		if i < 0 {
			break
		}
		d.line = d.line[:0]
		d.skipLine = false
		payload = payload[i+1:]
	}
	return false
}

func (d *streamTerminalDetector) normalizeSSEBoundaries(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	out := make([]byte, 0, len(payload)+16)
	for i := 0; i < len(payload); {
		if !d.jsonInString && d.jsonDepth == 0 && (bytes.HasPrefix(payload[i:], []byte("event:")) || bytes.HasPrefix(payload[i:], []byte("data:"))) {
			if (i > 0 && payload[i-1] != '\n') || (i == 0 && len(d.line) > 0) {
				out = append(out, '\n')
				d.resetJSONState()
			}
			markerLen := len("event:")
			if bytes.HasPrefix(payload[i:], []byte("data:")) {
				markerLen = len("data:")
			}
			out = append(out, payload[i:i+markerLen]...)
			i += markerLen
			continue
		}
		b := payload[i]
		out = append(out, b)
		d.consumeJSONByte(b)
		i++
	}
	return out
}

func (d *streamTerminalDetector) consumeJSONByte(b byte) {
	if b == '\n' || b == '\r' {
		d.resetJSONState()
		return
	}
	if d.jsonInString {
		if d.jsonEscape {
			d.jsonEscape = false
			return
		}
		if b == '\\' {
			d.jsonEscape = true
			return
		}
		if b == '"' {
			d.jsonInString = false
		}
		return
	}
	switch b {
	case '"':
		d.jsonInString = true
	case '{', '[':
		d.jsonDepth++
	case '}', ']':
		if d.jsonDepth > 0 {
			d.jsonDepth--
		}
	}
}

func (d *streamTerminalDetector) resetJSONState() {
	d.jsonDepth = 0
	d.jsonInString = false
	d.jsonEscape = false
}

func (d *streamTerminalDetector) terminalSSELine(line []byte) bool {
	if bytes.HasPrefix(line, []byte("event:")) {
		return terminalEvent(string(bytes.TrimSpace(line[6:])))
	}
	data := line
	if bytes.HasPrefix(line, []byte("data:")) {
		data = bytes.TrimSpace(line[5:])
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return true
	}
	var event struct {
		Type    string `json:"type"`
		Choices []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	if terminalEvent(event.Type) {
		return true
	}
	for _, choice := range event.Choices {
		if choice.FinishReason == nil || *choice.FinishReason == "" {
			continue
		}
		if d.finishedChoices == nil {
			d.finishedChoices = make(map[int]bool)
		}
		d.finishedChoices[choice.Index] = true
	}
	want := d.expectedChoices
	if want < 1 {
		want = 1
	}
	return len(d.finishedChoices) >= want
}

func terminalEvent(name string) bool {
	switch name {
	case "response.completed", "response.failed", "response.incomplete", "message_stop", "error":
		return true
	default:
		return false
	}
}
