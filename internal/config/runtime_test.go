package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRuntimeSnapshotCopiesMutableSettings(t *testing.T) {
	cfg := &Config{
		Provider: ProviderConfig{Name: "openai", Model: "old", APIKey: "original-key"},
		Persona:  PersonaConfig{Active: "original", Presets: []Persona{{ID: "original", Name: "Original", Prompt: "old prompt"}}},
		Mcp: McpConfig{Servers: []McpServerConfig{{
			Name: "example", Args: []string{"original-arg"},
			Env: map[string]string{"TOKEN": "original-token"}, Headers: map[string]string{"X-Test": "original-header"},
		}}},
	}
	before := cfg.Snapshot()
	copy := cfg.Snapshot()
	copy.Provider.APIKey = "changed-key"
	copy.Persona.Presets[0].Prompt = "changed prompt"
	copy.Mcp.Servers[0].Name = "changed-server"
	copy.Mcp.Servers[0].Args[0] = "changed-arg"
	copy.Mcp.Servers[0].Env["TOKEN"] = "changed-token"
	copy.Mcp.Servers[0].Headers["X-Test"] = "changed-header"
	if got := cfg.Snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("修改快照影响了原配置: got=%+v want=%+v", got, before)
	}

	sentinel := errors.New("configuration could not be saved")
	err := cfg.Apply(func(next *Config) error {
		next.Provider.APIKey = "failed-key"
		next.Persona.Presets[0].Prompt = "failed prompt"
		next.Mcp.Servers[0].Args[0] = "failed-arg"
		next.Mcp.Servers[0].Env["TOKEN"] = "failed-token"
		next.Mcp.Servers[0].Headers["X-Test"] = "failed-header"
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("事务错误应上抛，got %v", err)
	}
	if got := cfg.Snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("失败事务泄露了新配置: got=%+v want=%+v", got, before)
	}
}

func TestRuntimeConcurrentFileUpdatesPreserveIndependentFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "provider:\n  name: openai\n  baseURL: https://old.example/v1\n  model: old\n  apiKey: old-key\nmemory:\n  dir: keep-this-directory\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	presets := []Persona{{ID: "saved", Name: "Saved", Prompt: "Saved persona prompt"}}
	start := make(chan struct{})
	errCh := make(chan error, 32)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errCh <- UpdateProvider(path, "https://new.example/v1", "new-model", "new-key")
		}()
		go func() {
			defer wg.Done()
			<-start
			errCh <- UpdatePersona(path, "saved", presets)
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("并发更新配置失败（包括临时文件冲突）: %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Provider.BaseURL != "https://new.example/v1" || saved.Provider.Model != "new-model" || saved.Provider.APIKey != "new-key" {
		t.Fatalf("人设更新丢失了供应商字段: %+v", saved.Provider)
	}
	if saved.Persona.Active != "saved" || !reflect.DeepEqual(saved.Persona.Presets, presets) {
		t.Fatalf("供应商更新丢失了人设字段: %+v", saved.Persona)
	}
	if saved.Memory.Dir != "keep-this-directory" {
		t.Fatal("并发更新修改了无关配置字段")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("成功写回不应遗留临时文件: %v", err)
	}
}
