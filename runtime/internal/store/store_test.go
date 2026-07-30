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

func TestMemStoreLegacyArtifactLookupTreatsEmptyProjectAsIdentity(t *testing.T) {
	s := NewMemStore()
	arts := []Artifact{
		{Project: "", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1"},
		{Project: "grp/b", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1"},
	}
	if err := s.RegisterArtifacts(ctx, arts); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListArtifacts(ctx, "abcd1234", 42); err == nil || len(got) != 0 {
		t.Fatalf("ambiguous list = %+v err=%v, want fail closed", got, err)
	}
	if _, err := s.NextWorkflowAttempt(ctx, "abcd1234", 42, "v1"); err == nil {
		t.Fatal("ambiguous retry should fail closed")
	}
}
