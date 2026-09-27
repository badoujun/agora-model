package main

import "testing"

func TestWebUIURL(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"127.0.0.1:9090", "http://127.0.0.1:9090"},
		{"localhost:9090", "http://localhost:9090"},
		// 通配监听时给出一个能直接打开的地址
		{"0.0.0.0:9090", "http://127.0.0.1:9090"},
		{"[::]:9090", "http://127.0.0.1:9090"},
		// 具体的 IPv6 保持原样，并由 JoinHostPort 补上方括号
		{"[::1]:9090", "http://[::1]:9090"},
		{"192.168.1.5:8080", "http://192.168.1.5:8080"},
		// 解析不了就原样拼上，不 panic
		{"badaddr", "http://badaddr"},
	}
	for _, tc := range cases {
		if got := webUIURL(tc.addr); got != tc.want {
			t.Errorf("webUIURL(%q) = %q, 期望 %q", tc.addr, got, tc.want)
		}
	}
}
