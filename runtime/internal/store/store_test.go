package store

import "testing"

func TestMemStoreArtifactKeyIncludesProject(t *testing.T) {
	s := NewMemStore()
	arts := []Artifact{
		{Project: "grp/a", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
			BuildType: "Release", URL: "a", SHA256: "sa", Size: 1, ManifestDigest: "ma"},
		{Project: "grp/b", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
			BuildType: "Release", URL: "b", SHA256: "sb", Size: 1, ManifestDigest: "mb"},
	}
	if err := s.RegisterArtifacts(ctx, arts); err != nil {
		t.Fatal(err)
	}
	got := s.Artifacts()
	if len(got) != 2 {
		t.Fatalf("artifacts = %+v, want both projects", got)
	}
}

func TestMemStoreArtifactLookupIsProjectAware(t *testing.T) {
	s := NewMemStore()
	arts := []Artifact{
		{Project: "grp/a", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1"},
		{Project: "grp/b", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1"},
	}
	if err := s.RegisterArtifacts(ctx, arts); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListArtifacts(ctx, "grp/a", "abcd1234", 42)
	if err != nil || len(got) != 1 || got[0].Project != "grp/a" {
		t.Fatalf("project list = %+v err=%v, want grp/a only", got, err)
	}
	if n, err := s.NextWorkflowAttempt(ctx, "grp/a", "abcd1234", 42, "v1"); err != nil || n != 1 {
		t.Fatalf("project retry = %d err=%v, want 1", n, err)
	}
}
