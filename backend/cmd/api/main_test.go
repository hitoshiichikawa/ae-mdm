package main

import "testing"

// TestHealthcheckURL_RespectsHTTPListenAddr は HTTP_LISTEN_ADDR env からの port 抽出と
// loopback URL 構築の挙動を確認する（PR #31 round-1 / round-2 / round-3 review 由来）。
//
// listen addr が 0.0.0.0 / :port / 完全な host:port のいずれでも、healthcheck は
// 127.0.0.1:<port>/healthz を叩く（コンテナ内 healthcheck の前提）。
func TestHealthcheckURL_RespectsHTTPListenAddr(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "default (空文字)", input: "", want: "http://127.0.0.1:8080/healthz"},
		{name: ":9090 → port 上書き", input: ":9090", want: "http://127.0.0.1:9090/healthz"},
		{name: "0.0.0.0:8080", input: "0.0.0.0:8080", want: "http://127.0.0.1:8080/healthz"},
		{name: "127.0.0.1:9000", input: "127.0.0.1:9000", want: "http://127.0.0.1:9000/healthz"},
		{name: "解析不能 → fallback", input: "garbage", want: "http://127.0.0.1:8080/healthz"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := healthcheckURL(c.input)
			if got != c.want {
				t.Errorf("healthcheckURL(%q) = %q; want %q", c.input, got, c.want)
			}
		})
	}
}
