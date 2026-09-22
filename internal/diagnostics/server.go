// Package diagnostics provides opt-in, loopback-only staging memory diagnostics.
package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Address = "127.0.0.1:6060"

type Server struct {
	http      *http.Server
	started   time.Time
	revision  string
	profileMu sync.Mutex
	logger    *slog.Logger
}

func Enabled() bool {
	return os.Getenv("IRON_PROXY_DIAGNOSTICS") == "1" && os.Getenv("SHORTCUT_ENVIRONMENT") == "staging"
}

// Start binds synchronously so an enabled but unavailable collector cannot look
// healthy. No debug handlers are registered on the public health server.
func Start(ctx context.Context, logger *slog.Logger) (*Server, error) {
	if !Enabled() {
		return nil, nil
	}
	s := newServer(logger)
	listener, err := net.Listen("tcp", Address)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := s.http.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("go_diagnostics_server_failed", "error", err.Error())
		}
	}()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			logger.Info("go_runtime", "runtime", s.Snapshot())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return s, nil
}

func newServer(logger *slog.Logger) *Server {
	s := &Server{started: time.Now().UTC(), revision: "unknown", logger: logger}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				s.revision = setting.Value
			}
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /runtime", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s.Snapshot()); err != nil {
			s.logger.Error("go_diagnostics_response_failed", "error", err.Error())
		}
	})
	for _, name := range []string{"heap", "goroutine"} {
		mux.HandleFunc("GET /debug/pprof/"+name, func(w http.ResponseWriter, r *http.Request) {
			// Binary profiles only: no argument-bearing debug dumps, forced GC, CPU
			// profiling, or caller-controlled sampling/duration endpoints.
			if r.URL.RawQuery != "" {
				http.Error(w, "query parameters disabled", http.StatusBadRequest)
				return
			}
			if !s.profileMu.TryLock() {
				http.Error(w, "profile in progress", http.StatusTooManyRequests)
				return
			}
			defer s.profileMu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			if err := pprof.Lookup(name).WriteTo(w, 0); err != nil {
				s.logger.Error("go_profile_response_failed", "profile", name, "error", err.Error())
			}
		})
	}
	s.http = &http.Server{Addr: Address, Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	return s
}

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) Snapshot() map[string]any {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	samples := []metrics.Sample{{Name: "/gc/heap/live:bytes"}, {Name: "/gc/heap/goal:bytes"}}
	metrics.Read(samples)
	v := map[string]any{
		"schema_version": 1, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"process_started_at": s.started.Format(time.RFC3339Nano), "pid": os.Getpid(),
		"revision": s.revision, "go_version": runtime.Version(), "uptime_seconds": time.Since(s.started).Seconds(),
		"heap_alloc_bytes": m.HeapAlloc, "heap_live_bytes": samples[0].Value.Uint64(),
		"heap_goal_bytes": samples[1].Value.Uint64(), "heap_inuse_bytes": m.HeapInuse,
		"heap_idle_bytes": m.HeapIdle, "heap_released_bytes": m.HeapReleased,
		"heap_objects": m.HeapObjects, "total_alloc_bytes": m.TotalAlloc,
		"mallocs": m.Mallocs, "frees": m.Frees, "stack_inuse_bytes": m.StackInuse,
		"runtime_sys_bytes": m.Sys, "gc_cycles": m.NumGC, "gc_pause_total_ns": m.PauseTotalNs,
		"last_gc_unix_ns": m.LastGC, "gc_cpu_fraction": m.GCCPUFraction,
		"goroutines": runtime.NumGoroutine(), "mem_profile_rate_bytes": runtime.MemProfileRate,
	}
	// OS resident memory is distinct from live heap and runtime-reserved pages.
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			names := map[string]string{"VmRSS:": "rss_bytes", "RssAnon:": "rss_anon_bytes", "RssFile:": "rss_file_bytes", "VmSwap:": "swap_bytes"}
			if name, ok := names[fields[0]]; ok {
				if n, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					v[name] = n * 1024
				}
			}
		}
	} else {
		v["process_memory_error"] = fmt.Sprintf("%T", err)
	}
	if files, err := os.ReadDir("/proc/self/fd"); err == nil {
		v["fd_count"] = len(files)
	} else {
		v["fd_count_error"] = fmt.Sprintf("%T", err)
	}
	return v
}
