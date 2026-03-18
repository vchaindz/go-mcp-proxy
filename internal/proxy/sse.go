package proxy

import (
	"bufio"
	"io"
	"net/url"
	"strings"
)

// parseSSE implements the text/event-stream parser per the WHATWG spec.
func parseSSE(reader io.Reader, handler func(event, data string)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024) // up to 1MB lines

	var eventType string
	var dataBuf strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if dataBuf.Len() > 0 {
				data := dataBuf.String()
				if strings.HasSuffix(data, "\n") {
					data = data[:len(data)-1]
				}
				ev := eventType
				if ev == "" {
					ev = "message"
				}
				handler(ev, data)
			}
			eventType = ""
			dataBuf.Reset()
			continue
		}

		if line[0] == ':' {
			continue
		}

		field, value, hasSep := strings.Cut(line, ":")
		if hasSep && len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}

		switch field {
		case "event":
			eventType = value
		case "data":
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(value)
		}
	}
	return scanner.Err()
}

// resolveSessionURL resolves a potentially relative endpoint URL against the SSE base.
func resolveSessionURL(baseSSE, endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	base, err := url.Parse(baseSSE)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}
