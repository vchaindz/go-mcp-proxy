package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestParseFlexible(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPos  []string
		wantFlag string
	}{
		{
			"flag-then-positional",
			[]string{"--limit", "5", "http://server"},
			[]string{"http://server"},
			"5",
		},
		{
			"positional-then-flag",
			[]string{"http://server", "--limit", "5"},
			[]string{"http://server"},
			"5",
		},
		{
			"interleaved",
			[]string{"--cursor", "abc", "http://server", "--limit", "10"},
			[]string{"http://server"},
			"10",
		},
		{
			"two-positionals",
			[]string{"http://server", "prtg_get_sensors", "--limit", "3"},
			[]string{"http://server", "prtg_get_sensors"},
			"3",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			limit := fs.String("limit", "", "")
			fs.String("cursor", "", "")
			pos, err := parseFlexible(fs, c.args)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !equal(pos, c.wantPos) {
				t.Errorf("positionals: got %v, want %v", pos, c.wantPos)
			}
			if *limit != c.wantFlag {
				t.Errorf("limit flag: got %q, want %q", *limit, c.wantFlag)
			}
		})
	}
}

func TestPrintList_Tools_Truncates(t *testing.T) {
	tools := make([]map[string]string, 0, 200)
	for i := 0; i < 200; i++ {
		tools = append(tools, map[string]string{
			"name":        fmt.Sprintf("tool_%03d", i),
			"description": fmt.Sprintf("description for tool %d", i),
		})
	}
	body, _ := json.Marshal(map[string]any{"tools": tools, "nextCursor": "next-token"})

	out := captureStdout(t, func() {
		if err := printList("tools", body, 5, false); err != nil {
			t.Fatal(err)
		}
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	// Expect: 5 item lines + 1 truncation line + 1 next-cursor line = 7.
	if len(lines) != 7 {
		t.Fatalf("expected 7 lines, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "tool_000") {
		t.Errorf("first line should mention tool_000: %q", lines[0])
	}
	if !strings.Contains(lines[5], "showing 5 of 200") {
		t.Errorf("expected truncation footer, got: %q", lines[5])
	}
	if !strings.Contains(lines[6], "next-cursor: next-token") {
		t.Errorf("expected next-cursor line, got: %q", lines[6])
	}
}

func TestPrintList_Resources_ShowsURIFirst(t *testing.T) {
	resources := []map[string]string{
		{"uri": "prtg://sensor/12345", "name": "CPU Usage", "mimeType": "application/json"},
		{"uri": "prtg://group/100", "name": "Servers"},
	}
	body, _ := json.Marshal(map[string]any{"resources": resources})

	out := captureStdout(t, func() {
		if err := printList("resources", body, 0, false); err != nil {
			t.Fatal(err)
		}
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// Each resource URI must be the first token of its line so operators can
	// copy/paste the identifier without parsing.
	if !strings.HasPrefix(lines[0], "prtg://sensor/12345") {
		t.Errorf("resource URI must lead the line: %q", lines[0])
	}
	if !strings.Contains(lines[0], "[application/json]") {
		t.Errorf("expected mimeType tag: %q", lines[0])
	}
}

func TestPrintCallResult_LargeArray_Summarises(t *testing.T) {
	// Simulate a PRTG-shaped tool result: content[0].text is a JSON array of
	// 1500 sensors, each with sensor_id and sensor_name.
	sensors := make([]map[string]any, 0, 1500)
	for i := 0; i < 1500; i++ {
		sensors = append(sensors, map[string]any{
			"sensor_id":   2000 + i,
			"sensor_name": fmt.Sprintf("CPU Load %d", i),
			"device":      "host-" + fmt.Sprintf("%03d", i%50),
		})
	}
	textBody, _ := json.Marshal(sensors)
	resultBody, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(textBody)},
		},
	})

	out := captureStdout(t, func() {
		if err := printCallResult(resultBody, 5, false); err != nil {
			t.Fatal(err)
		}
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	if len(lines) < 5 {
		t.Fatalf("expected truncated output, got %d lines:\n%s", len(lines), out)
	}
	// Each item line should expose sensor_id (the resource ID).
	if !strings.Contains(lines[0], "sensor_id=2000") {
		t.Errorf("first line should expose sensor_id: %q", lines[0])
	}
	if !strings.Contains(lines[0], "sensor_name=CPU Load 0") {
		t.Errorf("first line should expose sensor_name: %q", lines[0])
	}
	// And the footer should tell the user how to widen the view.
	footer := lines[len(lines)-1]
	if !strings.Contains(footer, "showing 5 of 1500") {
		t.Errorf("expected truncation footer, got: %q", footer)
	}
	if !strings.Contains(footer, "--full") {
		t.Errorf("footer should hint at --full: %q", footer)
	}
}

func TestPrintCallResult_TextLines_Truncates(t *testing.T) {
	// Plain text content, many lines.
	text := strings.Repeat("line\n", 500)
	resultBody, _ := json.Marshal(map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	})

	out := captureStdout(t, func() {
		if err := printCallResult(resultBody, 10, false); err != nil {
			t.Fatal(err)
		}
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 10 line slice + 1 truncation footer.
	if len(lines) != 11 {
		t.Fatalf("expected 11 lines (10 + footer), got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[10], "truncated to 10 of 500") {
		t.Errorf("expected truncation footer, got: %q", lines[10])
	}
}

func TestCompactArrayItem_PicksIDAndName(t *testing.T) {
	cases := []struct {
		input map[string]any
		want  string
	}{
		{map[string]any{"id": 42, "name": "thing"}, "id=42  name=thing"},
		{map[string]any{"objid": 7, "device_name": "router-1"}, "objid=7  device_name=router-1"},
		{map[string]any{"id": 1}, "id=1"},
		{map[string]any{"name": "alone"}, "name=alone"},
	}
	for _, c := range cases {
		got := compactArrayItem(c.input)
		if got != c.want {
			t.Errorf("compactArrayItem(%v) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestKnownVerbs(t *testing.T) {
	for _, v := range []string{"tools", "resources", "prompts", "call", "ping", "raw", "debug"} {
		if !knownVerbs[v] {
			t.Errorf("verb %q should be known", v)
		}
	}
	if knownVerbs["http://example.com"] {
		t.Error("URL must not be treated as a verb")
	}
}

// captureStdout temporarily redirects os.Stdout, runs fn, and returns whatever
// fn wrote. Any error in pipe setup fails the test.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()

	done := make(chan []byte)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.Bytes()
	}()

	fn()
	w.Close()
	return string(<-done)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
