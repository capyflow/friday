package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	vxagent "github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"

	"github.com/capyflow/friday/internal/config"
	"gopkg.in/yaml.v3"
)

func runtimeTestBrain(t *testing.T, handler http.Handler, store sessionstore.Store) (*config.Config, *Brain, *vxagent.Registry, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		Provider: config.ProviderConfig{Name: "openai", BaseURL: srv.URL, Model: "original-model", APIKey: "original-key", MaxTokens: 64},
		Persona:  config.PersonaConfig{Active: "original", Presets: []config.Persona{{ID: "original", Name: "Original", Prompt: "Original persona marker"}}},
	}
	pool := NewSessionPool(store)
	registry := vxagent.NewRegistry()
	ag, err := Build(cfg, registry, pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	data, err := yaml.Marshal(cfg.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, NewBrain(ag, pool, nil), registry, path
}

func runtimeWaitError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("操作未在测试期限内结束")
		return nil
	}
}

func TestRuntimeResetWaitsForAskAndClearsPersistedHistory(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	store, err := sessionstore.NewJSON(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	fake := &fakeLLM{reply: "Completed answer"}
	_, brain, _, _ := runtimeTestBrain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(entered) })
		select {
		case <-release:
			fake.handler(w, r)
		case <-r.Context().Done():
		}
	}), store)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	askDone := make(chan error, 1)
	go func() {
		_, err := brain.Ask(ctx, "History that must be cleared", nil, nil)
		askDone <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("假 LLM 未收到请求")
	}
	resetStarted, resetDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(resetStarted)
		resetDone <- brain.Reset(SessionWeb)
	}()
	<-resetStarted
	select {
	case err := <-resetDone:
		t.Fatalf("Reset 在对话尚未结束时返回: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := runtimeWaitError(t, askDone); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := runtimeWaitError(t, resetDone); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if messages := brain.Pool().History(context.Background(), SessionWeb); len(messages) != 0 {
		t.Fatalf("清除后内存历史复活: %+v", messages)
	}
	reopened, err := sessionstore.NewJSON(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if restored, err := reopened.Load(context.Background(), sessionIDPrefix+SessionWeb); err != nil || restored != nil {
		t.Fatalf("清除后磁盘仍有旧会话: session=%v err=%v", restored, err)
	}
	fresh := NewSessionPool(reopened)
	if messages := fresh.Get(context.Background(), SessionWeb).Messages(); len(messages) != 0 {
		t.Fatalf("新池恢复了被清除的历史: %+v", messages)
	}
}

type runtimeErrorStore struct {
	sessionstore.Store
	saveErr, deleteErr error
}

func (s *runtimeErrorStore) Save(ctx context.Context, sess *sessionstore.Session) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.Store.Save(ctx, sess)
}

func (s *runtimeErrorStore) Delete(ctx context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.Store.Delete(ctx, id)
}

func TestRuntimeSessionStoreErrorsReachCallerAndKeepHistory(t *testing.T) {
	backing, err := sessionstore.NewJSON(filepath.Join(t.TempDir(), "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	store := &runtimeErrorStore{Store: backing}
	fake := &fakeLLM{reply: "Answer"}
	_, brain, _, _ := runtimeTestBrain(t, http.HandlerFunc(fake.handler), store)
	ctx := context.Background()
	sess := brain.Pool().Get(ctx, SessionWeb)
	sess.Add(llm.NewTextMessage(llm.RoleUser, "Existing history"))
	if err := brain.Pool().Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	before := sess.Messages()
	deleteErr := errors.New("cannot delete session")
	store.deleteErr = deleteErr
	if err := brain.Reset(SessionWeb); !errors.Is(err, deleteErr) {
		t.Fatalf("Reset 未返回真实删除错误: %v", err)
	}
	if got := brain.Pool().History(ctx, SessionWeb); !reflect.DeepEqual(got, before) {
		t.Fatalf("删除失败清空了旧内存: %+v", got)
	}
	if got := NewSessionPool(backing).History(ctx, SessionWeb); !reflect.DeepEqual(got, before) {
		t.Fatalf("删除失败改变了持久历史: %+v", got)
	}

	saveErr := errors.New("cannot save session")
	store.saveErr = saveErr
	if err := brain.Pool().Save(ctx, sess); !errors.Is(err, saveErr) {
		t.Fatalf("Pool.Save 未返回真实错误: %v", err)
	}
	answer, err := brain.Ask(ctx, "Next question", nil, nil)
	if !errors.Is(err, saveErr) || answer != fake.reply {
		t.Fatalf("Ask 应保留回答并报告落盘失败: answer=%q err=%v", answer, err)
	}
	messages := brain.Pool().History(ctx, SessionWeb)
	if len(messages) == 0 || messageTextOf(messages[0]) != "Existing history" {
		t.Fatalf("保存失败丢失了旧内存上下文: %+v", messages)
	}
	if got := NewSessionPool(backing).History(ctx, SessionWeb); !reflect.DeepEqual(got, before) {
		t.Fatalf("失败保存覆盖了最后有效的持久历史: %+v", got)
	}
}

func TestRuntimeReloadWriteFailureKeepsActiveAgent(t *testing.T) {
	for _, kind := range []string{"persona", "provider"} {
		t.Run(kind, func(t *testing.T) {
			fake := &fakeLLM{reply: "Original endpoint answer"}
			cfg, brain, registry, path := runtimeTestBrain(t, http.HandlerFunc(fake.handler), nil)
			before := cfg.Snapshot()
			originalFile, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// 原配置仍可读取，但临时写入失败，不依赖 chmod 或用户权限。
			if err := os.Mkdir(path+".tmp", 0o700); err != nil {
				t.Fatal(err)
			}
			replacementFake := &fakeLLM{reply: "Replacement endpoint answer"}
			replacement := httptest.NewServer(http.HandlerFunc(replacementFake.handler))
			defer replacement.Close()
			if kind == "persona" {
				err = ReloadPersona(cfg, path, registry, brain, nil, "replacement", []config.Persona{{ID: "replacement", Name: "Replacement", Prompt: "Replacement persona marker"}})
			} else {
				err = ReloadProvider(cfg, path, registry, brain, nil, replacement.URL, "replacement-model", "replacement-key")
			}
			if err == nil {
				t.Fatal("配置写回失败必须向调用者报错")
			}
			after := cfg.Snapshot()
			if !reflect.DeepEqual(before.Provider, after.Provider) || !reflect.DeepEqual(before.Persona, after.Persona) {
				t.Fatalf("失败热更新仍发布了配置: provider=%+v persona=%+v", after.Provider, after.Persona)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, originalFile) {
				t.Fatalf("写回失败破坏了原配置: err=%v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			answer, err := brain.Ask(ctx, "Verify active settings", nil, nil)
			if err != nil || answer != fake.reply {
				t.Fatalf("写回失败后对话未继续使用原 provider: answer=%q err=%v", answer, err)
			}
			fake.mu.Lock()
			requestBody := append([]byte(nil), fake.requests[len(fake.requests)-1]...)
			fake.mu.Unlock()
			var request struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(requestBody, &request); err != nil {
				t.Fatal(err)
			}
			if request.Model != before.Provider.Model || len(request.Messages) == 0 || request.Messages[0].Role != "system" || !strings.Contains(request.Messages[0].Content, "Original persona marker") {
				t.Fatalf("热更新失败后实际请求没有使用旧模型与人设: %+v", request)
			}
			replacementFake.mu.Lock()
			count := len(replacementFake.requests)
			replacementFake.mu.Unlock()
			if count != 0 {
				t.Fatal("未持久化的新 provider 收到了对话请求")
			}
		})
	}
}

func TestRuntimeConcurrentPersonaReloadMatchesDisk(t *testing.T) {
	fake := &fakeLLM{reply: "Answer"}
	cfg, brain, registry, path := runtimeTestBrain(t, http.HandlerFunc(fake.handler), nil)
	start := make(chan struct{})
	errCh := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("persona-%d", i)
			preset := config.Persona{ID: id, Name: id, Prompt: "Prompt for " + id}
			errCh <- ReloadPersona(cfg, path, registry, brain, nil, id, []config.Persona{preset})
		}(i)
	}
	done := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		<-start
		for {
			select {
			case <-done:
				return
			default:
				_ = cfg.Snapshot()
				_ = cfg.ProviderAPIKey()
			}
		}
	}()
	close(start)
	wg.Wait()
	close(done)
	<-readerDone
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("并发重载人设失败: %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved config.Config
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	current := cfg.Snapshot()
	if !reflect.DeepEqual(saved.Persona, current.Persona) || !reflect.DeepEqual(saved.Provider, current.Provider) {
		t.Fatalf("磁盘与已发布配置不同: saved=%+v current=%+v", saved.Persona, current.Persona)
	}
	if cfg.ProviderAPIKey() != "original-key" {
		t.Fatal("人设更新改变了 provider 密钥")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := brain.Ask(ctx, "Verify final persona", nil, nil); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	request := append([]byte(nil), fake.requests[len(fake.requests)-1]...)
	fake.mu.Unlock()
	if !bytes.Contains(request, []byte(current.Persona.Presets[0].Prompt)) {
		t.Fatalf("最终实际请求未使用已发布人设: %s", request)
	}
}
