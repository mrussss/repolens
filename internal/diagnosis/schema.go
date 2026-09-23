package diagnosis

import (
	"bytes"
	"encoding/json"
	"fmt"

	v22contracts "repolens/contracts/v2.2"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

var diagnosisCreateSchema = func() *jsonschema.Schema {
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("diagnosis-create.schema.json", bytes.NewReader(v22contracts.DiagnosisCreateSchema)); err != nil {
		panic(fmt.Sprintf("compile diagnosis create schema resource: %v", err))
	}
	schema, err := compiler.Compile("diagnosis-create.schema.json")
	if err != nil {
		panic(fmt.Sprintf("compile diagnosis create schema: %v", err))
	}
	return schema
}()

func validateDiagnosisCreateJSON(body []byte) error {
	var instance interface{}
	if err := json.Unmarshal(body, &instance); err != nil {
		return err
	}
	return diagnosisCreateSchema.Validate(instance)
}
