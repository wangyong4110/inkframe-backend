package ai

import "testing"

func TestVideoEngineTraits_RegisterAndLookupRoundTrip(t *testing.T) {
	name := "test-video-engine-roundtrip"
	traits := VideoEngineTraits{
		SnapsFixedDuration:          true,
		ResolvesModelFromDB:         true,
		SupportsMultiImageReference: true,
		NeedsPerImageAnnotation:     true,
		SupportsTemporalLinking:     true,
		SupportsExtendedVideoParams: true,
		DefaultResolution: func(hdEnabled bool, configuredResolution string) string {
			if hdEnabled {
				return "1080p"
			}
			return configuredResolution
		},
	}
	RegisterVideoEngineTraits(name, traits)

	got := VideoEngineTraitsFor(name)
	if !got.SnapsFixedDuration || !got.ResolvesModelFromDB || !got.SupportsMultiImageReference ||
		!got.NeedsPerImageAnnotation || !got.SupportsTemporalLinking || !got.SupportsExtendedVideoParams {
		t.Fatalf("expected all registered bool traits to round-trip true, got %+v", got)
	}
	if got.DefaultResolution == nil {
		t.Fatal("expected DefaultResolution func to round-trip non-nil")
	}
	if r := got.DefaultResolution(true, "720p"); r != "1080p" {
		t.Fatalf("DefaultResolution(true, ...) = %q, want 1080p", r)
	}
	if r := got.DefaultResolution(false, "720p"); r != "720p" {
		t.Fatalf("DefaultResolution(false, ...) = %q, want 720p", r)
	}
}

func TestVideoEngineTraitsFor_UnregisteredReturnsZeroValue(t *testing.T) {
	got := VideoEngineTraitsFor("nonexistent-video-engine-xyz")
	want := VideoEngineTraits{}
	if got.SnapsFixedDuration != want.SnapsFixedDuration ||
		got.ResolvesModelFromDB != want.ResolvesModelFromDB ||
		got.SupportsMultiImageReference != want.SupportsMultiImageReference ||
		got.NeedsPerImageAnnotation != want.NeedsPerImageAnnotation ||
		got.SupportsTemporalLinking != want.SupportsTemporalLinking ||
		got.SupportsExtendedVideoParams != want.SupportsExtendedVideoParams {
		t.Fatalf("expected zero-value VideoEngineTraits for unregistered provider, got %+v", got)
	}
	if got.DefaultResolution != nil {
		t.Fatal("expected nil DefaultResolution for unregistered provider")
	}
}

func TestImageEngineTraits_RegisterAndLookupRoundTrip(t *testing.T) {
	name := "test-image-engine-roundtrip"
	traits := ImageEngineTraits{
		Supports2KResolution:  true,
		SupportsReferenceImage: true,
		SelectModel: func(entry ImageProviderEntry, referenceImageCount int, style string, consistencyWeight float64) string {
			if referenceImageCount > 1 {
				return "multi-ref-model"
			}
			return entry.Model
		},
	}
	RegisterImageEngineTraits(name, traits)

	got := ImageEngineTraitsFor(name)
	if !got.Supports2KResolution || !got.SupportsReferenceImage {
		t.Fatalf("expected registered bool traits to round-trip true, got %+v", got)
	}
	if got.SelectModel == nil {
		t.Fatal("expected SelectModel func to round-trip non-nil")
	}
	entry := ImageProviderEntry{ProviderName: "p", Model: "base-model"}
	if m := got.SelectModel(entry, 2, "anime", 0.5); m != "multi-ref-model" {
		t.Fatalf("SelectModel with 2 refs = %q, want multi-ref-model", m)
	}
	if m := got.SelectModel(entry, 1, "anime", 0.5); m != "base-model" {
		t.Fatalf("SelectModel with 1 ref = %q, want base-model", m)
	}
}

func TestImageEngineTraitsFor_UnregisteredReturnsZeroValue(t *testing.T) {
	got := ImageEngineTraitsFor("nonexistent-image-engine-xyz")
	if got.Supports2KResolution || got.SupportsReferenceImage {
		t.Fatalf("expected zero-value ImageEngineTraits for unregistered provider, got %+v", got)
	}
	if got.SelectModel != nil {
		t.Fatal("expected nil SelectModel for unregistered provider")
	}
}

func TestVideoEngineTraits_RegisteringDifferentNamesDoesNotCollide(t *testing.T) {
	RegisterVideoEngineTraits("engine-a-collision-test", VideoEngineTraits{SnapsFixedDuration: true})
	RegisterVideoEngineTraits("engine-b-collision-test", VideoEngineTraits{ResolvesModelFromDB: true})

	a := VideoEngineTraitsFor("engine-a-collision-test")
	b := VideoEngineTraitsFor("engine-b-collision-test")
	if !a.SnapsFixedDuration || a.ResolvesModelFromDB {
		t.Fatalf("engine-a traits polluted: %+v", a)
	}
	if !b.ResolvesModelFromDB || b.SnapsFixedDuration {
		t.Fatalf("engine-b traits polluted: %+v", b)
	}
}

func TestRegisterVideoEngineTraits_OverwritesPreviousRegistration(t *testing.T) {
	name := "test-video-engine-overwrite"
	RegisterVideoEngineTraits(name, VideoEngineTraits{SnapsFixedDuration: true})
	RegisterVideoEngineTraits(name, VideoEngineTraits{SupportsTemporalLinking: true})

	got := VideoEngineTraitsFor(name)
	if got.SnapsFixedDuration {
		t.Fatal("expected second registration to overwrite the first")
	}
	if !got.SupportsTemporalLinking {
		t.Fatal("expected second registration's trait to be present")
	}
}
