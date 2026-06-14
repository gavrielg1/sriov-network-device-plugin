// Copyright 2024 Intel Corp. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	defaultLogDir    = "/var/log/sriovdp"
	defaultMaxSizeMB = 100
	defaultMaxFiles  = 5
	defaultMaxAge    = 30
	logFileName      = "sriovdp.log"

	maxAllowedSizeMB  = 1024 // 1 GB per file
	maxAllowedFiles   = 100
	maxAllowedAgeDays = 365
)

type Config struct {
	LogDir     string
	MaxSizeMB  int
	MaxFiles   int
	MaxAgeDays int
	Compress   bool
}

func ResolveConfig(cfg Config) (Config, []string, error) {
	def := DefaultConfig()
	normalized := cfg
	warnings := []string{}

	if normalized.LogDir == "" {
		normalized.LogDir = def.LogDir
	}
	if normalized.MaxSizeMB <= 0 || normalized.MaxSizeMB > maxAllowedSizeMB {
		warnings = append(warnings,
			fmt.Sprintf("invalid --log-max-size=%d; using default %d", normalized.MaxSizeMB, def.MaxSizeMB))
		normalized.MaxSizeMB = def.MaxSizeMB
	}
	if normalized.MaxFiles < 0 || normalized.MaxFiles > maxAllowedFiles {
		warnings = append(warnings,
			fmt.Sprintf("invalid --log-max-files=%d; using default %d", normalized.MaxFiles, def.MaxFiles))
		normalized.MaxFiles = def.MaxFiles
	}
	if normalized.MaxAgeDays < 0 || normalized.MaxAgeDays > maxAllowedAgeDays {
		warnings = append(warnings,
			fmt.Sprintf("invalid --log-max-age=%d; using default %d", normalized.MaxAgeDays, def.MaxAgeDays))
		normalized.MaxAgeDays = def.MaxAgeDays
	}

	normalized.Compress = true

	return normalized, warnings, nil
}

// DefaultConfig returns a Config populated with production defaults.
func DefaultConfig() Config {
	return Config{
		LogDir:     defaultLogDir,
		MaxSizeMB:  defaultMaxSizeMB,
		MaxFiles:   defaultMaxFiles,
		MaxAgeDays: defaultMaxAge,
		Compress:   true,
	}
}

// NewRotatingWriter creates a lumberjack-backed io.WriteCloser that
// automatically rotates the log file when it reaches the configured size.
// It also returns any config-normalization warnings generated during resolve.
// The caller must call Close on the returned writer when done.
func NewRotatingWriter(cfg Config) (io.WriteCloser, []string, error) {
	resolvedCfg, warnings, err := ResolveConfig(cfg)
	if err != nil {
		return nil, warnings, fmt.Errorf("invalid logging config: %w", err)
	}

	if err := os.MkdirAll(resolvedCfg.LogDir, 0755); err != nil {
		return nil, warnings, fmt.Errorf("cannot create log directory %q: %w", resolvedCfg.LogDir, err)
	}

	probe := filepath.Join(resolvedCfg.LogDir, ".probe")
	if err := os.WriteFile(probe, []byte{}, 0644); err != nil {
		return nil, warnings, fmt.Errorf("log directory %q is not writable: %w", resolvedCfg.LogDir, err)
	}
	os.Remove(probe) //nolint:errcheck

	return &lumberjack.Logger{
		Filename:   filepath.Join(resolvedCfg.LogDir, logFileName),
		MaxSize:    resolvedCfg.MaxSizeMB,
		MaxBackups: resolvedCfg.MaxFiles,
		MaxAge:     resolvedCfg.MaxAgeDays,
		Compress:   resolvedCfg.Compress,
	}, warnings, nil
}

// resilientTeeWriter writes to primary unconditionally and to secondary on a
// best-effort basis. Errors from secondary are silently discarded so that a
// failing log file never blocks or crashes the process by stalling the pipe
// drain goroutine.
type resilientTeeWriter struct {
	primary   io.Writer // original stderr
	secondary io.Writer // best-effort - rotating log file
}

func (t *resilientTeeWriter) Write(p []byte) (int, error) {
	n, err := t.primary.Write(p)
	if err != nil {
		return n, err
	}
	t.secondary.Write(p) //nolint:errcheck
	return n, nil
}

// CaptureStderr redirects the process's stderr file descriptor so that all
// data written to it (by glog or any other code) is tee'd to both the
// original stderr and the supplied io.Writer (typically a rotating file).
//
// It returns a cleanup function that MUST be called before process exit to
// restore the original stderr, close the pipe, and wait for the copy
// goroutine to drain remaining data.
func CaptureStderr(w io.Writer) (cleanup func(), err error) {
	if w == nil {
		return nil, fmt.Errorf("writer must not be nil")
	}

	origFd, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		return nil, fmt.Errorf("dup stderr: %w", err)
	}

	r, pw, err := os.Pipe()
	if err != nil {
		syscall.Close(origFd) //nolint:errcheck
		return nil, fmt.Errorf("create pipe: %w", err)
	}

	if err := syscall.Dup2(int(pw.Fd()), int(os.Stderr.Fd())); err != nil {
		r.Close()   //nolint:errcheck
		pw.Close()  //nolint:errcheck
		syscall.Close(origFd) //nolint:errcheck
		return nil, fmt.Errorf("dup2 pipe to stderr: %w", err)
	}

	pw.Close() //nolint:errcheck

	origFile := os.NewFile(uintptr(origFd), "original-stderr")
	tee := &resilientTeeWriter{primary: origFile, secondary: w}

	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(tee, r) //nolint:errcheck
	}()

	return func() {
		// Restore original stderr — this closes the pipe's write end
		// (fd 2), causing the copy goroutine to see EOF and drain.
		syscall.Dup2(origFd, int(os.Stderr.Fd())) //nolint:errcheck
		// Wait for the goroutine to finish draining the pipe before
		// closing the read end — otherwise buffered data is lost.
		<-done
		r.Close()        //nolint:errcheck
		origFile.Close() //nolint:errcheck
	}, nil
}
