package evidence

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// EmbeddedSchema 是 contracts/evidence.schema.json 的编译期副本;
// 与源文件的一致性由 TestEmbeddedSchemaMatchesContract 防漂移。
//
//go:embed evidence.schema.json
var EmbeddedSchema []byte

var compiledSchema = mustCompileSchema()

func mustCompileSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("evidence.schema.json", bytes.NewReader(EmbeddedSchema)); err != nil {
		panic(fmt.Sprintf("embedded evidence schema unreadable: %v", err))
	}
	return c.MustCompile("evidence.schema.json")
}
