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

func TestLoadJSONArgs_Inline(t *testing.T) {
	got, err := loadJSONArgs(`{"k":1}`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != `{"k":1}` {
		t.Errorf("inline arg should pass through unchanged, got %q", got)
	}
}

func TestLoadJSONArgs_File(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/args.json"
	want := `{"sensor_id":30235}`
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadJSONArgs("@" + path)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLoadJSONArgs_FileMissing(t *testing.T) {
	_, err := loadJSONArgs("@/nonexistent/path/that/should/not/exist.json")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	// The error must mention the path so the user can see what they typed.
	if !strings.Contains(err.Error(), "/nonexistent/path/that/should/not/exist.json") {
		t.Errorf("error should include the path, got: %v", err)
	}
}

func TestLoadJSONArgs_EmptyAt(t *testing.T) {
	_, err := loadJSONArgs("@")
	if err == nil {
		t.Fatal("expected error for bare '@', got nil")
	}
}

// TestDiagnoseJSONArgs_PowerShellQuoteStripping covers the screenshot scenario:
// PowerShell stripped the double quotes from {"sensor_id":65635}, so the program
// saw {sensor_id:65635}. The diagnostic must (a) echo the bytes received,
// (b) call out PowerShell/cmd as the likely culprit, and (c) suggest the
// recovered form so the user can copy-paste a fix.
func TestDiagnoseJSONArgs_PowerShellQuoteStripping(t *testing.T) {
	out := diagnoseJSONArgs(`{sensor_id:65635}`)
	for _, want := range []string{
		"received 17 bytes",
		"{sensor_id:65635}",
		"PowerShell",
		`did you mean: '{"sensor_id":65635}'`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestDiagnoseJSONArgs_NoFalseRecovery makes sure we don't suggest a fix when
// the bareword-quoting heuristic produces something that still doesn't parse.
// Random garbage should fall through to the generic hint without a "did you mean".
func TestDiagnoseJSONArgs_NoFalseRecovery(t *testing.T) {
	out := diagnoseJSONArgs(`not json at all`)
	if strings.Contains(out, "did you mean") {
		t.Errorf("should not suggest recovery for unrecoverable input\n%s", out)
	}
	if !strings.Contains(out, "Pass JSON args") {
		t.Errorf("missing generic hint\n%s", out)
	}
}

// TestTryRecoverJSON_NestedObject confirms the bareword-key auto-quoter
// handles nested objects, since PowerShell strips quotes everywhere.
func TestTryRecoverJSON_NestedObject(t *testing.T) {
	fixed, ok := tryRecoverJSON(`{a:1,b:{c:"keep me"}}`)
	if !ok {
		t.Fatalf("expected recovery, got none")
	}
	want := `{"a":1,"b":{"c":"keep me"}}`
	if fixed != want {
		t.Errorf("got %q, want %q", fixed, want)
	}
}

// TestTryRecoverJSON_AlreadyValid: when the input is already valid JSON we
// should NOT claim to have rewritten anything, even though our regex might
// match parts of it (e.g. unquoted keys never appear in valid JSON, so this
// is mostly belt-and-suspenders).
func TestTryRecoverJSON_AlreadyValid(t *testing.T) {
	if _, ok := tryRecoverJSON(`{"a":1}`); ok {
		t.Error("should not 'recover' input that was already valid JSON")
	}
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
