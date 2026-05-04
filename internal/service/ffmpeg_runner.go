package service

import (
	"bytes"
	"context"
	"fmt"

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
