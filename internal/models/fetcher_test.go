package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agora-model/internal/config"
)

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

func TestFetchReturnsCandidateModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("拉取路径 = %s, 期望 /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-up" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-tenant"); got != "agora" {
			t.Errorf("extra_headers 未生效: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"fetched-b"},{"id":"fetched-a"}]}`))
	}))
	defer upstream.Close()

	f := NewFetcher()
	p := config.Provider{
		ID:            "p1",
		OpenAIBaseURL: upstream.URL + "/v1",
		APIKey:        "sk-up",
		ExtraHeaders:  map[string]string{"x-tenant": "agora"},
	}
	got, err := f.Fetch(context.Background(), p)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 || got[0] != "fetched-b" || got[1] != "fetched-a" {
		t.Fatalf("候选模型 = %v", got)
	}
}

func TestFetchFailureReportsUpstreamStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer upstream.Close()

	f := NewFetcher()
	_, err := f.Fetch(context.Background(), config.Provider{
		ID:            "p1",
		OpenAIBaseURL: upstream.URL + "/v1",
		APIKey:        "sk-bad",
	})
	if err == nil {
		t.Fatal("401 应被视为失败")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误信息应包含状态码，实际 %q", err.Error())
	}
}

func TestFetchDetectsUnreachable(t *testing.T) {
	f := NewFetcher()
	_, err := f.Fetch(context.Background(), config.Provider{
		ID:            "dead",
		OpenAIBaseURL: "http://127.0.0.1:1/v1",
		APIKey:        "sk",
	})
	if err == nil {
		t.Fatal("不可达应报错")
	}
}

func TestFetchRequiresBaseURL(t *testing.T) {
	f := NewFetcher()
	if _, err := f.Fetch(context.Background(), config.Provider{
		ID:                     "p1",
		OpenAIEndpointOverride: "https://api.example.com/custom/completions",
	}); err == nil {
		t.Fatal("端点覆盖模式下应拒绝拉取模型")
	}
}
