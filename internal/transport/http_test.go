package transport

import (
	"io"
	"log/slog"
	"reflect"
	"testing"
)

func TestParsePattern(t *testing.T) {
	cases := []struct {
		pattern string
		want    []pathPart
	}{
		{"/echo", []pathPart{{literal: "echo"}}},
		{"/echo/:msg", []pathPart{{literal: "echo"}, {param: "msg"}}},
		{"/a/:b/c/:d", []pathPart{{literal: "a"}, {param: "b"}, {literal: "c"}, {param: "d"}}},
	}
	for _, tc := range cases {
		got := parsePattern(tc.pattern)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parsePattern(%q) = %+v, want %+v", tc.pattern, got, tc.want)
		}
	}
}

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern    string
		path       string
		wantOK     bool
		wantParams map[string]string
	}{
		{"/echo", "/echo", true, map[string]string{}},
		{"/echo", "/echo/extra", false, nil},
		{"/echo/:msg", "/echo/hello", true, map[string]string{"msg": "hello"}},
		{"/echo/:msg", "/echo", false, nil},
		{"/a/:b/c", "/a/x/c", true, map[string]string{"b": "x"}},
		{"/a/:b/c", "/a/x/d", false, nil},
	}
	for _, tc := range cases {
		parts := parsePattern(tc.pattern)
		params, ok := matchPattern(parts, tc.path)
		if ok != tc.wantOK {
			t.Errorf("matchPattern(%q, %q) ok = %v, want %v", tc.pattern, tc.path, ok, tc.wantOK)
			continue
		}
		if ok && !reflect.DeepEqual(params, tc.wantParams) {
			t.Errorf("matchPattern(%q, %q) params = %v, want %v", tc.pattern, tc.path, params, tc.wantParams)
		}
	}
}

func TestRegisterRouteRejectsBadInput(t *testing.T) {
	s := NewHTTPServer(":0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.RegisterRoute("", "/ok"); err == nil {
		t.Error("expected error for empty method")
	}
	if err := s.RegisterRoute("GET", "no-slash"); err == nil {
		t.Error("expected error for pattern missing leading /")
	}
}

func TestPopRequestEmptyQueue(t *testing.T) {
	s := NewHTTPServer(":0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, ok := s.PopRequest(); ok {
		t.Error("expected no request on empty queue")
	}
}
