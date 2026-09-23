package v22contracts

import _ "embed"

// DiagnosisCreateSchema is the versioned request contract used by the API
// handler and its tests. Keeping the schema beside the public contract avoids
// a second, silently divergent validation definition.
//
//go:embed diagnosis-create.schema.json
var DiagnosisCreateSchema []byte
