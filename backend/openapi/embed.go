// Package openapi API sözleşmesini binary içine gömer (GET /openapi.yaml).
package openapi

import _ "embed"

//go:embed openapi.yaml
var Spec []byte
