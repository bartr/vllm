package main

import (
	"os"
	"path/filepath"
	"testing"
)

func mkPrompts(n int) []Prompt {
	out := make([]Prompt, n)
	for i := range out {
		out[i] = Prompt{Text: "p"}
	}
	return out
}

func writePromptYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOrderedSourceForeverCycles(t *testing.T) {
	src := newOrderedSource(mkPrompts(3), true)
	for i := 0; i < 100; i++ {
		if _, ok := src.Next(); !ok {
			t.Fatalf("forever source returned !ok at i=%d", i)
		}
	}
}

func TestOrderedSourceDefaultIsOnce(t *testing.T) {
	src := newOrderedSource(mkPrompts(3), false)
	got := 0
	for {
		_, ok := src.Next()
		if !ok {
			break
		}
		got++
	}
	if got != 3 {
		t.Errorf("got %d pulls, want 3", got)
	}
}

// TestBuildPromptSourceCountCyclesForever reproduces the user-reported
// bug: --count 1000 with a 3-prompt --files used to exit after 3 pulls.
// With count set, the source must be unbounded.
func TestBuildPromptSourceCountCyclesForever(t *testing.T) {
	tmp := writePromptYAML(t, "- prompt: a\n- prompt: b\n- prompt: c\n")
	opts := options{files: []string{tmp}, count: 1000}
	src, _, err := buildPromptSource(opts, Prompt{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, ok := src.Next(); !ok {
			t.Fatalf("source exhausted at i=%d (count was set, expected unbounded)", i)
		}
	}
}

func TestBuildPromptSourceDurationCyclesForever(t *testing.T) {
	tmp := writePromptYAML(t, "- prompt: a\n- prompt: b\n")
	opts := options{files: []string{tmp}, duration: 1}
	src, _, err := buildPromptSource(opts, Prompt{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, ok := src.Next(); !ok {
			t.Fatalf("source exhausted at i=%d (duration set, expected unbounded)", i)
		}
	}
}

func TestBuildPromptSourceLoopFlagCyclesForever(t *testing.T) {
	tmp := writePromptYAML(t, "- prompt: a\n- prompt: b\n")
	opts := options{files: []string{tmp}, loop: true}
	src, _, err := buildPromptSource(opts, Prompt{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, ok := src.Next(); !ok {
			t.Fatalf("source exhausted at i=%d (loop set, expected unbounded)", i)
		}
	}
}

func TestBuildPromptSourceDefaultIsSinglePass(t *testing.T) {
	tmp := writePromptYAML(t, "- prompt: a\n- prompt: b\n")
	opts := options{files: []string{tmp}}
	src, _, err := buildPromptSource(opts, Prompt{})
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for {
		if _, ok := src.Next(); !ok {
			break
		}
		got++
	}
	if got != 2 {
		t.Errorf("default file pass: got %d pulls, want 2", got)
	}
}

func TestBuildPromptSourceConcatenatesMultipleFiles(t *testing.T) {
	a := writePromptYAML(t, "- prompt: a1\n- prompt: a2\n")
	b := writePromptYAML(t, "- prompt: b1\n")
	opts := options{files: []string{a, b}}
	src, n, err := buildPromptSource(opts, Prompt{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("prompt count: got %d want 3", n)
	}
	want := []string{"a1", "a2", "b1"}
	for i, w := range want {
		p, ok := src.Next()
		if !ok {
			t.Fatalf("exhausted at i=%d", i)
		}
		if p.Text != w {
			t.Errorf("i=%d: got %q want %q", i, p.Text, w)
		}
	}
}

func TestExtractFilesArg(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		rest []string
		want []string
	}{
		{"single", []string{"--files", "a.yaml"}, []string{}, []string{"a.yaml"}},
		{"multiple", []string{"--files", "a.yaml", "b.yaml", "c.yaml"}, []string{}, []string{"a.yaml", "b.yaml", "c.yaml"}},
		{"stops at flag", []string{"--files", "a.yaml", "--bench", "4"}, []string{"--bench", "4"}, []string{"a.yaml"}},
		{"equals form", []string{"--files=a.yaml"}, []string{}, []string{"a.yaml"}},
		{"equals plus extras", []string{"--files=a.yaml", "b.yaml"}, []string{}, []string{"a.yaml", "b.yaml"}},
		{"mixed with other flags",
			[]string{"--bench", "4", "--files", "a.yaml", "b.yaml", "--count", "10"},
			[]string{"--bench", "4", "--count", "10"},
			[]string{"a.yaml", "b.yaml"}},
		{"repeated --files",
			[]string{"--files", "a.yaml", "--files", "b.yaml", "c.yaml"},
			[]string{},
			[]string{"a.yaml", "b.yaml", "c.yaml"}},
		{"no --files", []string{"--bench", "4"}, []string{"--bench", "4"}, nil},
		{"double dash terminator",
			[]string{"--files", "a.yaml", "--", "--files", "x"},
			[]string{"--", "--files", "x"},
			[]string{"a.yaml"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRest, gotFiles := extractFilesArg(tc.in)
			if !equalStrings(gotRest, tc.rest) {
				t.Errorf("rest: got %v want %v", gotRest, tc.rest)
			}
			if !equalStrings(gotFiles, tc.want) {
				t.Errorf("files: got %v want %v", gotFiles, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
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
