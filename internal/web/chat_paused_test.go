package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/agent"
)

type noopProvider struct{}

func (noopProvider) Name() string       { return "noop" }
func (noopProvider) ContextWindow() int { return 128000 }
func (noopProvider) Chat(context.Context, *llm.ChatRequest, func(llm.Delta) error) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Message: llm.NewTextMessage(llm.RoleAssistant, "回答")}, nil
}

func newChat(t *testing.T, file string) *Chat {
	t.Helper()
	pool := agent.NewSessionPool(nil)
	ag := vxagent.New(vxagent.Options{Provider: noopProvider{}, Model: "m"})
	return New(agent.NewBrain(ag, pool, nil), t.TempDir(), file)
}

func readTurns(t *testing.T, file string) map[string]*PausedTurn {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var turns map[string]*PausedTurn
	if err := json.Unmarshal(data, &turns); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v (%s)", err, data)
	}
	return turns
}

// TestPausedTurnPersistsWithSkill 暂停现场落盘，重启后仍能继续并复用当时的技能。
func TestPausedTurnPersistsWithSkill(t *testing.T) {
	file := filepath.Join(t.TempDir(), "paused-turns.json")
	c := newChat(t, file)

	c.mu.Lock()
	c.paused["web"] = &PausedTurn{Question: "晨会 说一下今天", Partial: "今天是", Skill: "晨会准备"}
	c.savePausedLocked()
	c.mu.Unlock()

	turns := readTurns(t, file)
	if turns["web"] == nil || turns["web"].Skill != "晨会准备" {
		t.Fatalf("落盘内容不符: %+v", turns)
	}

	reopened := newChat(t, file)
	got := reopened.PausedTurn("web")
	if got == nil || got.Question != "晨会 说一下今天" || got.Partial != "今天是" || got.Skill != "晨会准备" {
		t.Fatalf("重启恢复不符: %+v", got)
	}
}

// TestTakePausedClearsDisk 取出（继续）后磁盘上不应再留着，避免重复继续。
func TestTakePausedClearsDisk(t *testing.T) {
	file := filepath.Join(t.TempDir(), "paused-turns.json")
	c := newChat(t, file)
	c.mu.Lock()
	c.paused["web"] = &PausedTurn{Question: "问题", Partial: "一半"}
	c.savePausedLocked()
	c.mu.Unlock()

	turn := c.takePaused("web")
	if turn == nil || turn.Question != "问题" {
		t.Fatalf("takePaused 应返回现场: %+v", turn)
	}
	if turns := readTurns(t, file); len(turns) != 0 {
		t.Errorf("取出后应清空落盘现场: %+v", turns)
	}
	if again := c.takePaused("web"); again != nil {
		t.Error("同一现场不应被继续两次")
	}
}

// TestClearPausedOnNewQuestion 暂停期间发新问题会丢弃旧现场（含磁盘）。
func TestClearPausedOnNewQuestion(t *testing.T) {
	file := filepath.Join(t.TempDir(), "paused-turns.json")
	c := newChat(t, file)
	c.mu.Lock()
	c.paused["web"] = &PausedTurn{Question: "旧问题", Partial: "旧的一半"}
	c.savePausedLocked()
	c.mu.Unlock()

	c.clearPaused("web")
	if turns := readTurns(t, file); len(turns) != 0 {
		t.Errorf("放弃暂停现场后磁盘应清空: %+v", turns)
	}
	// 没有现场时 clear 不该造出文件
	c.clearPaused("web")
	if _, err := os.Stat(file); err == nil {
		if turns := readTurns(t, file); len(turns) != 0 {
			t.Errorf("重复 clear 不应留下内容: %+v", turns)
		}
	}
}

// TestCorruptPausedFilePreserved 损坏文件保留原样并放弃加载（不用空状态覆盖）。
func TestCorruptPausedFilePreserved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "paused-turns.json")
	original := "{ this is broken"
	if err := os.WriteFile(file, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newChat(t, file)
	if got := c.PausedTurn("web"); got != nil {
		t.Errorf("损坏文件应放弃加载: %+v", got)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Errorf("原文件应被保留，实际 %q", data)
	}
}

// TestMemoryOnlyMode 未配置落盘路径时仍可正常存取（测试/嵌入式场景）。
func TestMemoryOnlyMode(t *testing.T) {
	c := newChat(t, "")
	c.mu.Lock()
	c.paused["web"] = &PausedTurn{Question: "问题"}
	c.savePausedLocked()
	c.mu.Unlock()
	if c.takePaused("web") == nil {
		t.Error("纯内存模式应照常工作")
	}
}
