package trigger

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v5"

	wf "hermes-devops/runtime/internal/workflow"
)

// embeddedBundleSchema 是 contracts/bundle.schema.json 的编译期副本;
// 一致性由 TestEmbeddedBundleSchemaMatchesContract 防漂移。
//
//go:embed bundle.schema.json
var embeddedBundleSchema []byte

var bundleSchema = mustCompileBundleSchema()

func mustCompileBundleSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("bundle.schema.json", bytes.NewReader(embeddedBundleSchema)); err != nil {
		panic(fmt.Sprintf("embedded bundle schema unreadable: %v", err))
	}
	return c.MustCompile("bundle.schema.json")
}

// Bundle 是 bundle-g{sha}.json 的解析结果(contracts/bundle.schema.json)。
type Bundle struct {
	BundleVersion    int             `json:"bundle_version"`
	Project          string          `json:"project"`
	Commit           string          `json:"commit"`
	PipelineID       int             `json:"pipeline_id"`
	PipelineGlobalID int             `json:"pipeline_global_id"`
	Version          string          `json:"version"`
	CreatedAt        string          `json:"created_at"`
	Packages         []wf.PackageRef `json:"packages"`
}

// ParseBundle 先过 Schema 再反序列化(红线:未经校验不消费)。
func ParseBundle(raw []byte) (*Bundle, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse bundle json: %w", err)
	}
	if err := bundleSchema.Validate(doc); err != nil {
		return nil, fmt.Errorf("bundle schema validation: %w", err)
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("decode bundle: %w", err)
	}
	return &b, nil
}
