package models

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agora-model/internal/config"
)

func TestAvailable(t *testing.T) {
	got := Available(
		[]string{"manual-a", "shared", " manual-a "},
		[]string{"auto-b", "auto-a", "shared"},
		[]string{"auto-b"},
	)
	want := []string{"manual-a", "shared", "auto-a"}
	if len(got) != len(want) {
		t.Fatalf("Available = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Available = %v, 期望 %v（手动在前、自动按字典序、排除列表生效）", got, want)
		}
	}
}

func TestAvailableEmptyExcludedAndNil(t *testing.T) {
	if got := Available(nil, nil, nil); len(got) != 0 {
		t.Fatalf("空输入应返回空列表，得到 %v", got)
	}
	got := Available(nil, []string{"m2", "m1"}, []string{"m1"})
	if len(got) != 1 || got[0] != "m2" {
		t.Fatalf("Available = %v, 期望 [m2]", got)
	}
}

func TestParseModelIDs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"OpenAI 标准形式", `{"object":"list","data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`, []string{"gpt-4o", "gpt-4o-mini"}},
		{"字符串数组", `{"data":["m1","m2"]}`, []string{"m1", "m2"}},
		{"顶层数组", `["m1","m2"]`, []string{"m1", "m2"}},
		{"重复与空白", `{"data":[{"id":" m1 "},{"id":"m1"}]}`, []string{"m1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseModelIDs(strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("parseModelIDs: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}

	for _, bad := range []string{`not json`, `{"object":"list","data":[]}`, `[]`} {
		if _, err := parseModelIDs(strings.NewReader(bad)); err == nil {
			t.Errorf("parseModelIDs(%q) 应报错", bad)
		}
	}
}

// fakeStore 实现聚合器需要的存储能力。
type fakeStore struct {
	mu          sync.Mutex
	providers   []config.Provider
	replaced    map[string][]string
	fetchErrors map[string]string
	settings    map[string]string
	masterSeen  []byte
}

func newFakeStore(providers ...config.Provider) *fakeStore {
	return &fakeStore{
		providers:   providers,
		replaced:    map[string][]string{},
		fetchErrors: map[string]string{},
		settings:    map[string]string{},
	}
}

func (f *fakeStore) LoadProviders(ctx context.Context, master []byte) ([]config.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.masterSeen = master
	return append([]config.Provider(nil), f.providers...), nil
}

func (f *fakeStore) ReplaceModelCache(ctx context.Context, providerID string, models []string, fetchedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaced[providerID] = append([]string(nil), models...)
	delete(f.fetchErrors, providerID)
	return nil
}

func (f *fakeStore) SetProviderFetchError(ctx context.Context, providerID, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchErrors[providerID] = message
	return nil
}

func (f *fakeStore) GetSetting(ctx context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.settings[key]
	return v, ok, nil
}

func (f *fakeStore) cached(providerID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaced[providerID]
}

func (f *fakeStore) fetchError(providerID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetchErrors[providerID]
}

func TestRefreshProviderStoresFetchedModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("拉取路径 = %s, 期望 /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-up" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"fetched-b"},{"id":"fetched-a"}]}`))
	}))
	defer upstream.Close()

	st := newFakeStore()
	agg := NewAggregator(st, nil, Options{Master: []byte("master"), Timeout: 2 * time.Second})

	p := config.Provider{
		ID:              "p1",
		OpenAIBaseURL:   upstream.URL + "/v1",
		APIKey:          "sk-up",
		AutoFetchModels: boolPtr(true),
		Enabled:         boolPtr(true),
	}
	if err := agg.RefreshProvider(context.Background(), p); err != nil {
		t.Fatalf("RefreshProvider: %v", err)
	}
	got := st.cached("p1")
	if len(got) != 2 || got[0] != "fetched-b" || got[1] != "fetched-a" {
		t.Fatalf("缓存 = %v", got)
	}
	if err := st.fetchError("p1"); err != "" {
		t.Fatalf("成功拉取后不应保留失败信息: %q", err)
	}
}

func TestRefreshProviderFailureKeepsCacheAndRecordsError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer upstream.Close()

	st := newFakeStore()
	agg := NewAggregator(st, nil, Options{Master: []byte("m"), Timeout: 2 * time.Second})

	p := config.Provider{ID: "p1", OpenAIBaseURL: upstream.URL + "/v1", APIKey: "sk-bad",
		AutoFetchModels: boolPtr(true), Enabled: boolPtr(true)}

	err := agg.RefreshProvider(context.Background(), p)
	if err == nil {
		t.Fatal("401 应被视为失败")
	}
	if got := st.cached("p1"); got != nil {
		t.Fatalf("失败时不应写入缓存，实际 %v", got)
	}
	if msg := st.fetchError("p1"); !strings.Contains(msg, "401") {
		t.Fatalf("应记录失败原因，实际 %q", msg)
	}
}

func TestRefreshProviderSkipsWhenAutoFetchDisabled(t *testing.T) {
	st := newFakeStore()
	agg := NewAggregator(st, nil, Options{Master: []byte("m")})
	p := config.Provider{ID: "p1", AutoFetchModels: boolPtr(false)}
	if err := agg.RefreshProvider(context.Background(), p); err != nil {
		t.Fatalf("关闭自动拉取时不应报错: %v", err)
	}
	if got := st.cached("p1"); got != nil {
		t.Fatalf("不应写入缓存: %v", got)
	}
}

func TestRefreshAllIsolatesFailuresAndCallsOnRefresh(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"ok-model"}]}`))
	}))
	defer good.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	st := newFakeStore(
		config.Provider{ID: "good", OpenAIBaseURL: good.URL + "/v1", APIKey: "sk", AutoFetchModels: boolPtr(true), Enabled: boolPtr(true)},
		config.Provider{ID: "bad", OpenAIBaseURL: deadURL + "/v1", APIKey: "sk", AutoFetchModels: boolPtr(true), Enabled: boolPtr(true)},
		config.Provider{ID: "off", OpenAIBaseURL: good.URL + "/v1", APIKey: "sk", Enabled: boolPtr(false)},
	)

	refreshed := 0
	agg := NewAggregator(st, nil, Options{
		Master:  []byte("m"),
		Timeout: 2 * time.Second,
		OnRefresh: func(context.Context) {
			refreshed++
		},
	})
	agg.RefreshAll(context.Background())

	if got := st.cached("good"); len(got) != 1 || got[0] != "ok-model" {
		t.Fatalf("可用供应商应成功写入缓存: %v", got)
	}
	if st.fetchError("bad") == "" {
		t.Fatal("不可达供应商应记录失败原因")
	}
	if st.cached("off") != nil {
		t.Fatal("停用的供应商不应被刷新")
	}
	if refreshed != 1 {
		t.Fatalf("OnRefresh 调用次数 = %d, 期望 1", refreshed)
	}
}

func TestIntervalFromSetting(t *testing.T) {
	st := newFakeStore()
	st.settings[SettingRefreshSeconds] = "120"
	agg := NewAggregator(st, nil, Options{Master: []byte("m")})
	if got := agg.interval(context.Background()); got != 120*time.Second {
		t.Fatalf("interval = %v, 期望 2m", got)
	}

	agg2 := NewAggregator(newFakeStore(), nil, Options{Master: []byte("m")})
	if got := agg2.interval(context.Background()); got != DefaultRefreshInterval {
		t.Fatalf("interval = %v, 期望默认值", got)
	}
}

func TestRefreshProviderDetectsUnreachable(t *testing.T) {
	st := newFakeStore()
	agg := NewAggregator(st, nil, Options{Master: []byte("m"), Timeout: time.Second})
	p := config.Provider{ID: "dead", OpenAIBaseURL: "http://127.0.0.1:1/v1", APIKey: "sk",
		AutoFetchModels: boolPtr(true)}
	err := agg.RefreshProvider(context.Background(), p)
	if err == nil || !errors.Is(err, err) {
		t.Fatalf("不可达应报错，得到 %v", err)
	}
}

func boolPtr(b bool) *bool { return &b }
