// Package claimy provides the repository's authoritative code generation entry point.
package claimy

//go:generate go run github.com/justtrackio/gotempl template api/openapi.yaml.gotempl api/openapi.yaml
//go:generate go tool oapi-codegen --config api/oapi-codegen.yaml api/openapi.yaml
//go:generate go tool mockery --config .mockery.yml
