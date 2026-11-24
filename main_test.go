package main

import (
	"net/http/httptest"
	"testing"
)

func TestBoardURL(t *testing.T) {
	for host, want := range map[string]string{
		"caffe-board.example.ankra.cc": "https://caffe-board.example.ankra.cc",
		"localhost:8080":               "http://localhost:8080",
		"192.168.1.20:8080":            "http://192.168.1.20:8080",
		"alex-mac.local:8080":          "http://alex-mac.local:8080",
	} {
		request := httptest.NewRequest("GET", "/", nil)
		request.Host = host
		if got := boardURL(request); got != want {
			t.Errorf("boardURL(%q) = %q, want %q", host, got, want)
		}
	}
}
