// Package manifest 解析并校验包内 manifest.yaml(契约 contracts/manifest.schema.json)。
// 红线:未经 Schema 校验不得消费 Manifest(CLAUDE.md §14)。
package manifest

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"
)

// EmbeddedSchema 是 contracts/manifest.schema.json 的编译期副本;
// 与源文件的一致性由 TestEmbeddedSchemaMatchesContract 防漂移。
//
//go:embed manifest.schema.json
var EmbeddedSchema []byte

var compiledSchema = mustCompileSchema()

func mustCompileSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("manifest.schema.json", bytes.NewReader(EmbeddedSchema)); err != nil {
		panic(fmt.Sprintf("embedded manifest schema unreadable: %v", err))
	}
	return c.MustCompile("manifest.schema.json")
}

type File struct {
	Src    string `yaml:"src"`
	Dst    string `yaml:"dst"`
	Mode   string `yaml:"mode"`
	SHA256 string `yaml:"sha256"`
}

type Signature struct {
	ID       string `yaml:"id"`
	Where    string `yaml:"where"`
	Pattern  string `yaml:"pattern"`
	Classify string `yaml:"classify"`
}

type Manifest struct {
	ManifestVersion int `yaml:"manifest_version"`
	Artifact        struct {
		Project    string `yaml:"project"`
		Commit     string `yaml:"commit"`
		PipelineID int    `yaml:"pipeline_id"`
		Platform   string `yaml:"platform"`
		BuildType  string `yaml:"build_type"`
	} `yaml:"artifact"`
	Requirements struct {
		OS               string   `yaml:"os"`
		ABI              string   `yaml:"abi"`
		SOC              []string `yaml:"soc"`
		Capabilities     []string `yaml:"capabilities"`
		MinFreeStorageMB int      `yaml:"min_free_storage_mb"`
	} `yaml:"requirements"`
	Deploy struct {
		Workdir string            `yaml:"workdir"`
		Files   []File            `yaml:"files"`
		Env     map[string]string `yaml:"env"`
	} `yaml:"deploy"`
	Test struct {
		Entry      string   `yaml:"entry"`
		Args       []string `yaml:"args"`
		TimeoutSec int      `yaml:"timeout_sec"`
		Success    struct {
			ExitCode     int      `yaml:"exit_code"`
			RequireFiles []string `yaml:"require_files"`
		} `yaml:"success"`
		FailureSignatures []Signature `yaml:"failure_signatures"`
	} `yaml:"test"`
	Collect []string `yaml:"collect"`
	Cleanup struct {
		RemoveWorkdir bool `yaml:"remove_workdir"`
		KeepOnFailure bool `yaml:"keep_on_failure"`
	} `yaml:"cleanup"`
}

// Load 读取 manifest.yaml,先过 Schema 再反序列化。
func Load(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	// Schema 校验走通用类型(yaml.v3 的 map 键为 string,与 JSON 兼容)
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse manifest yaml: %w", err)
	}
	if err := compiledSchema.Validate(doc); err != nil {
		return nil, fmt.Errorf("manifest schema validation: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// ResolvedEnv 返回 env 副本,值中的 {workdir} 占位符替换为实际 workdir。
func (m *Manifest) ResolvedEnv() map[string]string {
	out := make(map[string]string, len(m.Deploy.Env))
	for k, v := range m.Deploy.Env {
		out[k] = strings.ReplaceAll(v, "{workdir}", m.Deploy.Workdir)
	}
	return out
}
