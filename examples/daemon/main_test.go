package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccessControl(t *testing.T) {
	d := &daemon{sessions: map[string]*hosted{}, token: "secret", hosts: allowedHosts("127.0.0.1", "8080")}
	handler := d.routes()
	cases := []struct {
		name   string
		method string
		path   string
		header map[string]string
		body   string
		want   int
	}{
		{name: "authorized", method: "GET", path: "/sessions", want: http.StatusOK},
		{name: "localhost", method: "GET", path: "/sessions", header: map[string]string{"Host": "localhost:8080"}, want: http.StatusOK},
		{name: "ipv6 loopback", method: "GET", path: "/sessions", header: map[string]string{"Host": "[::1]:8080"}, want: http.StatusOK},
		{name: "missing token", method: "GET", path: "/sessions", header: map[string]string{"Authorization": ""}, want: http.StatusUnauthorized},
		{name: "wrong token", method: "GET", path: "/sessions", header: map[string]string{"Authorization": "Bearer guess"}, want: http.StatusUnauthorized},
		{name: "rebound host", method: "GET", path: "/sessions", header: map[string]string{"Host": "evil.example:8080"}, want: http.StatusForbidden},
		{name: "other port", method: "GET", path: "/sessions", header: map[string]string{"Host": "127.0.0.1:9090"}, want: http.StatusForbidden},
		{name: "origin", method: "GET", path: "/sessions", header: map[string]string{"Origin": "http://127.0.0.1:8080"}, want: http.StatusForbidden},
		{name: "text body", method: "POST", path: "/sessions", header: map[string]string{"Content-Type": "text/plain"}, body: `{"provider":"claude"}`, want: http.StatusUnsupportedMediaType},
		{name: "no content type", method: "POST", path: "/sessions", header: map[string]string{"Content-Type": ""}, body: `{"provider":"claude"}`, want: http.StatusUnsupportedMediaType},
		{name: "json body", method: "POST", path: "/sessions/none/messages", body: `{"text":"hi"}`, want: http.StatusNotFound},
		{name: "json with charset", method: "POST", path: "/sessions/none/messages", header: map[string]string{"Content-Type": "application/json; charset=utf-8"}, body: `{}`, want: http.StatusNotFound},
		{name: "empty post", method: "POST", path: "/sessions/none/interrupt", header: map[string]string{"Content-Type": ""}, want: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Host = "127.0.0.1:8080"
			r.Header.Set("Authorization", "Bearer secret")
			r.Header.Set("Content-Type", "application/json")
			for k, v := range tc.header {
				if k == "Host" {
					r.Host = v
				} else if v == "" {
					r.Header.Del(k)
				} else {
					r.Header.Set(k, v)
				}
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}
