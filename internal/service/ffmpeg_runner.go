package service

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"codeberg.org/gruf/go-ffmpreg/ffmpreg"
	"codeberg.org/gruf/go-ffmpreg/wasm"
	"github.com/tetratelabs/wazero"
)

// rootFSConfig mounts the host root filesystem so ffmpeg can read/write arbitrary paths.
func rootFSConfig(cfg wazero.ModuleConfig) wazero.ModuleConfig {
	fscfg := wazero.NewFSConfig()
	fscfg = fscfg.WithDirMount("/", "/")
	return cfg.WithFSConfig(fscfg)
}

// runFFmpegCtx runs the embedded FFmpeg WASM binary with the given args.
// It returns combined stdout+stderr output. A non-zero exit code is returned as an error.
// The output bytes are always returned even when err != nil (useful for error message inspection).
func runFFmpegCtx(ctx context.Context, args ...string) ([]byte, error) {
	var out bytes.Buffer
	rc, err := ffmpreg.Ffmpeg(ctx, wasm.Args{
		Args:   args,
		Stdout: &out,
		Stderr: &out,
		Config: rootFSConfig,
	})
	if err != nil {
		return out.Bytes(), err
	}
	if rc != 0 {
		return out.Bytes(), fmt.Errorf("ffmpeg exited with code %d\noutput: %s", rc, out.String())
	}
	return out.Bytes(), nil
}

// ffmpegResult holds the return values of a runFFmpegCtx call.
type ffmpegResult struct {
	out []byte
	err error
}

// runFFmpegWithGoroutineTimeout runs FFmpeg in a separate goroutine and returns
// after at most `timeout`, regardless of whether wazero/WASM honours ctx cancellation.
//
// Wazero cannot interrupt a WASM module that is in a tight CPU loop (e.g. x264
// encoding) via Go context alone. Wrapping in a goroutine + channel select gives
// a hard wall-clock timeout that actually works.
//
// NOTE: if the timeout fires, the underlying goroutine is left running in the
// background. It will complete eventually (WASM finishes or wazero exits). Callers
// should treat the timed-out output file as absent / incomplete.
func runFFmpegWithGoroutineTimeout(timeout time.Duration, args ...string) ([]byte, error) {
	ch := make(chan ffmpegResult, 1)
	go func() {
		out, err := runFFmpegCtx(context.Background(), args...)
		ch <- ffmpegResult{out, err}
	}()
	select {
	case res := <-ch:
		return res.out, res.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("ffmpeg goroutine timeout after %v (wasm still running in background)", timeout)
	}
}
