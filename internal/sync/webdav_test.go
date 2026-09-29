package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTargetURLBuilding(t *testing.T) {
	// 无论 URL 怎么写，文件名都固定为 RemoteFilename；URL 视为目录。
	cases := []struct {
		name string
		cfg  Config
		want string
		err  bool
	}{
		{
			name: "目录形式",
			cfg:  Config{URL: "https://dav.example.com/agora/"},
			want: "https://dav.example.com/agora/" + RemoteFilename,
		},
		{
			name: "无尾斜杠",
			cfg:  Config{URL: "https://dav.example.com/agora"},
			want: "https://dav.example.com/agora/" + RemoteFilename,
		},
		{
			name: "用户输入的恰好是完整文件 URL：视为目录 + 仍然固定追加 RemoteFilename",
			cfg:  Config{URL: "https://dav.example.com/agora/" + RemoteFilename},
			want: "https://dav.example.com/agora/" + RemoteFilename + "/" + RemoteFilename,
		},
		{name: "空地址", cfg: Config{URL: "  "}, err: true},
		{name: "非法 scheme", cfg: Config{URL: "ftp://example.com"}, err: true},
	}
	for _, tc := range cases {
		c := New(tc.cfg, time.Second)
		got, err := c.target()
		if tc.err {
			if err == nil {
				t.Errorf("%s: expected error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: target = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadWriteBasic(t *testing.T) {
	var got []byte
	var uploads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if got == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(got)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			got = body
			uploads++
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := New(Config{
		URL:      srv.URL + "/dir/",
		Username: "u", Password: "p",
	}, 2*time.Second)

	ctx := context.Background()

	body, sum, err := c.ReadRemote(ctx)
	if err != nil {
		t.Fatalf("ReadRemote: %v", err)
	}
	if body != nil {
		t.Fatalf("首次应返回 NotFound，得到 %d 字节", len(body))
	}

	payload := []byte(`{"format":"agoramodel.providers/v1","providers":[]}`)
	wantSum := HashBytes(payload)

	// 远端无文件 → 任何 expectSHA256 都应允许上传
	if err := c.WriteRemote(ctx, payload, ""); err != nil {
		t.Fatalf("WriteRemote(empty expect): %v", err)
	}
	if uploads != 1 {
		t.Errorf("uploads = %d", uploads)
	}

	body, sum, err = c.ReadRemote(ctx)
	if err != nil {
		t.Fatalf("ReadRemote 2: %v", err)
	}
	if string(body) != string(payload) || sum != wantSum {
		t.Fatalf("写入后再读，不一致: sum=%q want=%q body=%q", sum, wantSum, body)
	}
}

func TestConflictOnWriteWhenRemoteChanged(t *testing.T) {
	remote := []byte(`{"format":"v1","providers":[]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(remote)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL + "/d/"}, time.Second)
	// 期望的指纹与实际不符 → 冲突
	wrong := HashBytes([]byte("anything-else"))
	err := c.WriteRemote(context.Background(), []byte("local"), wrong)
	if err == nil {
		t.Fatal("期望 ErrConflict")
	}
	var conf *ErrConflict
	if !asConflict(err, &conf) {
		t.Fatalf("错误类型不符: %T %v", err, err)
	}
	if conf.RemoteSHA256 != HashBytes(remote) {
		t.Errorf("RemoteSHA256 = %q", conf.RemoteSHA256)
	}
}

func TestUnauthorizedNormalized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := New(Config{URL: srv.URL + "/d/", Username: "u", Password: "p"}, time.Second)
	_, _, err := c.ReadRemote(context.Background())
	if err != ErrUnauthorized {
		t.Errorf("err = %v", err)
	}
}

func TestHashBytes(t *testing.T) {
	data := []byte("hello")
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	if got := HashBytes(data); got != want {
		t.Errorf("HashBytes = %q", got)
	}
}

// asConflict 用 errors.As 取冲突错误实例。
func asConflict(err error, target **ErrConflict) bool {
	return errors.As(err, target)
}
