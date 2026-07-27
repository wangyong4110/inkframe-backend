package ai

import (
	"testing"
)

// TestLipSyncProvider_InterfaceSatisfiedByAllImplementations pins the
// LipSyncProvider interface's implementors at compile time.
var _ LipSyncProvider = (*KlingLipSyncProvider)(nil)

func TestLipSyncRequest_Fields(t *testing.T) {
	req := &LipSyncRequest{
		ImageURL: "https://example.com/face.png",
		AudioURL: "https://example.com/audio.mp3",
		Model:    "kling-v1-6",
		Mode:     "pro",
	}
	if req.ImageURL == "" || req.AudioURL == "" {
		t.Error("expected ImageURL and AudioURL to be set")
	}
	if req.Model != "kling-v1-6" || req.Mode != "pro" {
		t.Errorf("unexpected request: %+v", req)
	}
}

func TestLipSyncTask_Fields(t *testing.T) {
	task := &LipSyncTask{TaskID: "id1", Status: "pending", Provider: "kling-lipsync"}
	if task.TaskID != "id1" || task.Status != "pending" || task.Provider != "kling-lipsync" {
		t.Errorf("unexpected LipSyncTask: %+v", task)
	}
}

func TestLipSyncTaskStatus_Fields(t *testing.T) {
	ts := &LipSyncTaskStatus{TaskID: "id1", Status: "processing", Progress: 42, Error: ""}
	if ts.Progress != 42 {
		t.Errorf("Progress = %v, want 42", ts.Progress)
	}
}
