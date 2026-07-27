package ai

import (
	"testing"
)

// TestVideoProvider_InterfaceSatisfiedByAllImplementations pins the
// VideoProvider interface's implementors at compile time. If any of these
// providers drift from the interface shape, this file fails to compile.
var (
	_ VideoProvider = (*KlingProvider)(nil)
	_ VideoProvider = (*DoubaoVideoProvider)(nil)
	_ VideoProvider = (*JimengVideoProvider)(nil)
	_ VideoProvider = (*HappyHorseProvider)(nil)
)

// TestVideoGenerateRequest_ZeroValue documents the zero-value shape of
// VideoGenerateRequest used across provider tests (no special behavior,
// simply guards against accidental field removal/renaming going unnoticed).
func TestVideoGenerateRequest_ZeroValue(t *testing.T) {
	var req VideoGenerateRequest
	if req.ImageURL != "" || len(req.ImageURLs) != 0 {
		t.Error("zero-value VideoGenerateRequest should have no images")
	}
	if req.Duration != 0 {
		t.Error("zero-value VideoGenerateRequest should have zero duration")
	}
	if req.GenerateAudio != nil {
		t.Error("zero-value VideoGenerateRequest.GenerateAudio should be nil")
	}
}

func TestVideoTask_Fields(t *testing.T) {
	task := &VideoTask{TaskID: "t1", Status: "pending", Provider: "kling"}
	if task.TaskID != "t1" || task.Status != "pending" || task.Provider != "kling" {
		t.Errorf("unexpected VideoTask: %+v", task)
	}
}

func TestVideoTaskStatus_Fields(t *testing.T) {
	ts := &VideoTaskStatus{TaskID: "t1", Status: "completed", Progress: 100, Error: "", LastFrameURL: "https://x/y.png"}
	if ts.Progress != 100 {
		t.Errorf("Progress = %v, want 100", ts.Progress)
	}
	if ts.LastFrameURL != "https://x/y.png" {
		t.Errorf("LastFrameURL = %v", ts.LastFrameURL)
	}
}
