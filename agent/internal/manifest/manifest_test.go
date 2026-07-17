package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const contractsDir = "../../../contracts"

func TestEmbeddedSchemaMatchesContract(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(contractsDir, "manifest.schema.json"))
	if err != nil {
		t.Fatalf("read contracts schema: %v", err)
	}
	if !bytes.Equal(EmbeddedSchema, want) {
		t.Fatal("embedded manifest.schema.json 与 contracts/ 不一致,请重新拷贝(防契约漂移)")
	}
}

func TestLoadValidExamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(contractsDir, "tests/examples/manifest/valid/*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no valid examples found: %v", err)
	}
	for _, f := range files {
		m, err := Load(f)
		if err != nil {
			t.Errorf("%s: expected valid, got error: %v", filepath.Base(f), err)
			continue
		}
		if m.ManifestVersion != 1 {
			t.Errorf("%s: manifest_version = %d, want 1", filepath.Base(f), m.ManifestVersion)
		}
		if m.Deploy.Workdir == "" || m.Test.Entry == "" {
			t.Errorf("%s: missing core fields after load: %+v", filepath.Base(f), m)
		}
	}
}

func TestLoadRejectsInvalidExamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(contractsDir, "tests/examples/manifest/invalid/*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no invalid examples found: %v", err)
	}
	for _, f := range files {
		if _, err := Load(f); err == nil {
			t.Errorf("%s: expected schema rejection, got nil error", filepath.Base(f))
		}
	}
}

func TestLoadFullManifestFields(t *testing.T) {
	m, err := Load(filepath.Join(contractsDir, "tests/examples/manifest/valid/snpe_android_full.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Artifact.Platform != "aarch64_Android_SNPE_2.21" {
		t.Errorf("platform = %q", m.Artifact.Platform)
	}
	if m.Requirements.MinFreeStorageMB != 512 {
		t.Errorf("min_free_storage_mb = %d", m.Requirements.MinFreeStorageMB)
	}
	if len(m.Deploy.Files) != 3 || m.Deploy.Files[0].Mode != "0755" {
		t.Errorf("deploy.files parsed wrong: %+v", m.Deploy.Files)
	}
	if m.Test.TimeoutSec != 900 || len(m.Test.FailureSignatures) != 2 {
		t.Errorf("test section parsed wrong: %+v", m.Test)
	}
	if !m.Cleanup.RemoveWorkdir || !m.Cleanup.KeepOnFailure {
		t.Errorf("cleanup parsed wrong: %+v", m.Cleanup)
	}
}

func TestResolvedEnvReplacesWorkdirPlaceholder(t *testing.T) {
	m, err := Load(filepath.Join(contractsDir, "tests/examples/manifest/valid/snpe_android_full.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env := m.ResolvedEnv()
	want := "/data/local/tmp/algo-super-sdk/lib"
	if env["LD_LIBRARY_PATH"] != want {
		t.Errorf("LD_LIBRARY_PATH = %q, want %q", env["LD_LIBRARY_PATH"], want)
	}
	// 原始 map 不得被修改
	if m.Deploy.Env["LD_LIBRARY_PATH"] != "{workdir}/lib" {
		t.Errorf("original env mutated: %q", m.Deploy.Env["LD_LIBRARY_PATH"])
	}
}
