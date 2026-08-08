package manifest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// KnownFields 第二轮反序列化必须拒绝 Schema 未声明的字段,防止 Manifest
// struct 扩展时意外接受未声明数据(审查 #2 回归测试)。
func TestKnownFieldsRejectsUndeclaredFields(t *testing.T) {
	// 构造一个 Schema 合法但含未声明字段的 manifest
	yaml := `manifest_version: 1
artifact:
  project: test
  commit: abc1234
  pipeline_id: 1
  platform: aarch64_Android_SNPE_2.21
  build_type: Release
requirements:
  os: android
  abi: arm64-v8a
deploy:
  workdir: /data/local/tmp/test
  files:
    - src: run.sh
      dst: run.sh
      mode: "0755"
      sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
test:
  entry: ./run.sh
  timeout_sec: 300
  success:
    exit_code: 0
collect:
  - results/*.json
cleanup:
  remove_workdir: true
undeclared_field: should_be_rejected
`
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write test manifest: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected KnownFields rejection for undeclared_field, got nil")
	}
	// 错误信息应包含 "unknown field" 或 "undeclared" 关键词
	errMsg := err.Error()
	if !strings.Contains(errMsg, "unknown field") && !strings.Contains(errMsg, "undeclared") {
		t.Errorf("err = %v, want mention of unknown/undeclared field", err)
	}
}

// env 键会被 adb.ShellRunEntry 裸拼进设备 shell 命令串(值加引号、键不加),
// 所以非法键必须在 Load 阶段就被拒(红线 §14:Client 不提供任意 shell)。
// Schema 的 propertyNames 与 Load 里的 envKeyPattern 是两道独立防线,
// 本用例覆盖两者中先触发的那道。
func TestLoadRejectsShellInjectionInEnvKey(t *testing.T) {
	manifestTmpl := `manifest_version: 1
artifact:
  project: test
  commit: abc1234
  pipeline_id: 1
  platform: aarch64_Android_SNPE_2.21
  build_type: Release
requirements:
  os: android
  abi: arm64-v8a
deploy:
  workdir: /data/local/tmp/test
  files:
    - src: run.sh
      dst: run.sh
      mode: "0755"
      sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
  env:
    %q: "x"
test:
  entry: ./run.sh
  timeout_sec: 300
  success:
    exit_code: 0
collect:
  - results/*.json
`
	badKeys := []string{
		"LD_LIBRARY_PATH=x; rm -rf /data/local/tmp; X", // 命令分隔符注入
		"A B",              // 空格拆出新 argv
		"A$(id)",           // 命令替换
		"A`id`",            // 反引号替换
		"A&&id",            // 逻辑连接符
		"0STARTS_WITH_NUM", // 非法首字符(POSIX 环境变量名不得以数字开头)
	}
	for _, k := range badKeys {
		t.Run(k, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf(manifestTmpl, k)), 0644); err != nil {
				t.Fatalf("write test manifest: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("env key %q was accepted; it can inject arbitrary device commands", k)
			}
		})
	}
}

// 合法的 POSIX 环境变量名不能被上面的约束误伤(防止修红线时把正常变体打挂)。
func TestLoadAcceptsPosixEnvKeys(t *testing.T) {
	for _, k := range []string{"LD_LIBRARY_PATH", "ADSP_LIBRARY_PATH", "_UNDERSCORE_LEAD", "A1"} {
		if !envKeyPattern.MatchString(k) {
			t.Errorf("legit env key %q rejected by envKeyPattern", k)
		}
	}
}
