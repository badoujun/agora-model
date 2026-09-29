package presets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinLoaded(t *testing.T) {
	s := New(Options{})
	f, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Presets) == 0 {
		t.Fatal("内置预设应至少 1 条")
	}
	if s.Source() != "内置" {
		t.Errorf("Source = %q, 期望 内置", s.Source())
	}
	// 内置应包含国内四家供应商
	want := []string{"deepseek", "minimax", "scnet", "agnes"}
	got := map[string]bool{}
	for _, p := range f.Presets {
		got[p.ID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("内置预设缺少 %q", id)
		}
	}
}

func TestOverridePathWins(t *testing.T) {
	dir := t.TempDir()
	overridePath := filepath.Join(dir, "custom.json")
	raw := `{
		"version": 1,
		"presets": [
			{"id": "custom", "name": "Custom", "openai_base_url": "https://example.com/v1"}
		]
	}`
	if err := os.WriteFile(overridePath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(Options{OverridePath: overridePath})
	f, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Presets) != 1 || f.Presets[0].ID != "custom" {
		t.Fatalf("覆盖文件未生效: %+v", f.Presets)
	}
	if !strings.Contains(s.Source(), "custom.json") {
		t.Errorf("Source = %q, 期望包含自定义路径", s.Source())
	}
}

func TestOverrideMissingFallbackBuiltin(t *testing.T) {
	dir := t.TempDir()
	s := New(Options{OverridePath: filepath.Join(dir, "no-such-file.json")})
	f, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Presets) == 0 {
		t.Fatal("覆盖文件不存在时应回落到内置版本")
	}
}

func TestIgnoreInvalidEntries(t *testing.T) {
	dir := t.TempDir()
	overridePath := filepath.Join(dir, "custom.json")
	// 重复 id 与缺 id 都会被丢弃
	raw := `{
		"version": 1,
		"presets": [
			{"id": "a", "name": "A", "openai_base_url": "https://a.example.com/v1"},
			{"id": "A", "name": "A dup", "openai_base_url": "https://a2.example.com/v1"},
			{"id": "", "name": "no id", "openai_base_url": "https://x.example.com/v1"},
			{"id": "b", "name": "B", "openai_base_url": " https://b.example.com/v1/ "}
		]
	}`
	if err := os.WriteFile(overridePath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(Options{OverridePath: overridePath})
	f, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Presets) != 2 {
		t.Fatalf("presets = %d, 期望 2（重复与缺 id 丢弃）", len(f.Presets))
	}
	for _, p := range f.Presets {
		if strings.HasSuffix(p.OpenAIBaseURL, "/") {
			t.Errorf("OpenAIBaseURL 未去尾斜杠: %q", p.OpenAIBaseURL)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	dir := t.TempDir()
	overridePath := filepath.Join(dir, "custom.json")
	raw := `{"version": 1, "presets": [{"id":"x","name":"X","openai_base_url":"https://x/v1","extra":1}]}`
	if err := os.WriteFile(overridePath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Options{OverridePath: overridePath})
	if _, err := s.Load(); err == nil {
		t.Fatal("未知字段应被拒绝")
	}
}

func TestBuiltinFingerprintStable(t *testing.T) {
	if BuiltinFingerprint() == "" {
		t.Fatal("内置指纹不应为空")
	}
}
