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

	"github.com/stretchr/testify/require"
)

func TestEnvironmentGate(t *testing.T) {
	cases := []struct {
		name, env, flag string
		enabled         bool
	}{
		{"absent", "", "", false}, {"disabled", "staging", "0", false},
		{"missing opt in", "staging", "", false}, {"dev", "dev", "1", false},
		{"production", "prod", "1", false}, {"staging", "staging", "1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHORTCUT_ENVIRONMENT", tc.env)
			t.Setenv("IRON_PROXY_DIAGNOSTICS", tc.flag)
			require.Equal(t, tc.enabled, Enabled())
		})
	}
	t.Setenv("SHORTCUT_ENVIRONMENT", "prod")
	t.Setenv("IRON_PROXY_DIAGNOSTICS", "1")
	s, err := Start(context.Background(), slog.Default())
	require.NoError(t, err)
	require.Nil(t, s)
}

func TestPrivateDiagnosticsContract(t *testing.T) {
	s := newServer(slog.Default())
	require.Equal(t, "127.0.0.1:6060", s.http.Addr)
	cases := []struct {
		name, route string
		code        int
		binary      bool
	}{
		{"runtime", "/runtime", 200, false}, {"heap", "/debug/pprof/heap", 200, true},
		{"goroutine", "/debug/pprof/goroutine", 200, true},
		{"forced GC denied", "/debug/pprof/heap?gc=1", 400, false},
		{"text stacks denied", "/debug/pprof/goroutine?debug=2", 400, false},
		{"CPU disabled", "/debug/pprof/profile", 404, false},
		{"trace disabled", "/debug/pprof/trace", 404, false},
		{"cmdline disabled", "/debug/pprof/cmdline", 404, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, tc.route, nil))
			require.Equal(t, tc.code, r.Code)
			if tc.route == "/runtime" {
				var v map[string]any
				require.NoError(t, json.Unmarshal(r.Body.Bytes(), &v))
				for _, field := range []string{"heap_live_bytes", "heap_alloc_bytes", "heap_objects", "total_alloc_bytes", "gc_cycles", "goroutines", "process_started_at", "revision", "mem_profile_rate_bytes"} {
					require.Contains(t, v, field)
				}
			}
			if tc.binary {
				reader, err := gzip.NewReader(bytes.NewReader(r.Body.Bytes()))
				require.NoError(t, err)
				data, err := io.ReadAll(reader)
				require.NoError(t, err)
				require.NoError(t, reader.Close())
				require.NotEmpty(t, data)
			}
		})
	}
	s.profileMu.Lock()
	r := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil))
	s.profileMu.Unlock()
	require.Equal(t, http.StatusTooManyRequests, r.Code)
}

func TestLiveLoopbackServer(t *testing.T) {
	t.Setenv("SHORTCUT_ENVIRONMENT", "staging")
	t.Setenv("IRON_PROXY_DIAGNOSTICS", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Start(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, err)
	defer func() {
		stop, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		require.NoError(t, s.Shutdown(stop))
	}()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + Address + "/runtime")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
}
