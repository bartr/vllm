package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// runLog is a per-job log file opened in the askd log directory. The
// file is created when a job starts and closed when it stops, so each
// run gets its own clean file. Suitable for "stop, reconfigure, start,
// run benchmark, stop" workflows where you want a clean log per run
// without slicing on timestamps.
type runLog struct {
	mu   sync.Mutex
	f    *os.File
	name string
	path string
}

func newRunLog(dir string) (*runLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("run-%s.log", time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	// O_EXCL guards against the (vanishingly unlikely) name collision
	// from sub-second restarts.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		// On collision, fall back to a nanosecond-suffixed name.
		name = fmt.Sprintf("run-%s.log", time.Now().UTC().Format("20060102T150405.000000000Z"))
		path = filepath.Join(dir, name)
		f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			return nil, err
		}
	}
	return &runLog{f: f, name: name, path: path}, nil
}

func (r *runLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return len(p), nil // already closed; drop silently
	}
	return r.f.Write(p)
}

func (r *runLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
