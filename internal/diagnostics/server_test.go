package diagnostics

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEnvironmentGate(t *testing.T) {
	for _, env := range []string{"", "dev", "prod", "staging"} {
		t.Setenv("SHORTCUT_ENVIRONMENT", env)
		for _, flag := range []string{"", "0", "1"} {
			t.Setenv("IRON_PROXY_DIAGNOSTICS", flag)
			if Enabled() != (env == "staging" && flag == "1") {
				t.Fatalf("gate env=%q flag=%q", env, flag)
			}
		}
	}
	t.Setenv("SHORTCUT_ENVIRONMENT", "prod")
	s, err := Start(context.Background(), slog.Default())
	if err != nil || s != nil {
		t.Fatal("production must not start diagnostics")
	}
}

func TestPrivateDiagnosticsContract(t *testing.T) {
	s := newServer()
	if s.http.Addr != "127.0.0.1:6060" {
		t.Fatal("must bind loopback")
	}
	for _, route := range []string{"/runtime", "/debug/pprof/heap", "/debug/pprof/goroutine"} {
		r := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, route, nil))
		if r.Code != 200 {
			t.Fatalf("%s: %d", route, r.Code)
		}
		if route == "/runtime" {
			var v map[string]any
			if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"heap_live_bytes", "heap_alloc_bytes", "heap_objects", "total_alloc_bytes", "gc_cycles", "goroutines", "process_started_at", "revision", "mem_profile_rate_bytes"} {
				if _, ok := v[field]; !ok {
					t.Fatalf("missing %s", field)
				}
			}
		} else {
			reader, err := gzip.NewReader(bytes.NewReader(r.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || len(data) == 0 {
				t.Fatal("invalid binary profile", err)
			}
		}
	}
	for _, route := range []string{"/debug/pprof/heap?gc=1", "/debug/pprof/goroutine?debug=2", "/debug/pprof/profile", "/debug/pprof/trace", "/debug/pprof/cmdline"} {
		r := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, route, nil))
		if r.Code == 200 {
			t.Fatalf("unexpected exposed route %s", route)
		}
	}
	s.profileMu.Lock()
	r := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil))
	s.profileMu.Unlock()
	if r.Code != 429 {
		t.Fatal("concurrent profiles should be rejected")
	}
}

func TestLiveLoopbackServer(t *testing.T) {
	t.Setenv("SHORTCUT_ENVIRONMENT", "staging")
	t.Setenv("IRON_PROXY_DIAGNOSTICS", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Start(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		if err := s.Shutdown(stop); err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + Address + "/runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
}
