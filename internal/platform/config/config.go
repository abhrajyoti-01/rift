// Package config is RIFT's configuration engine: YAML load, strict
// unknown-field rejection, programmatic validation with field-path errors,
// redaction, diffing, and file watching (ARCHITECTURE §10). rift config
// validate runs the identical path as service startup, so CI checks the
// file an operator would write.
//
// Phase 1 (ROADMAP.md). This file pins the public contract.
package config

// Schema is the validated root configuration document: one file per
// deployment with service sections inside it (ARCHITECTURE §10).
type Schema struct {
	// Phase 1: lb, dns, tls, media, shared sections with exact YAML tags.
}

// Source names where a Schema came from — file path plus applied env
// overrides — so error paths and diffs can address it.
type Source struct {
	Path    string
	EnvApplied []string
}

// Diff is a stable, field-path-addressed difference between two Schemas.
// It is logged on reload and printed by `rift config diff`.
type Diff struct {
	// Phase 1: changed []FieldChange with path, from, to (values redacted).
}

// Load reads, parses, and validates the YAML at path, applying documented
// scalar env overrides. Validation is pure and never touches the network.
func Load(path string) (*Schema, *Source, error) {
	// Phase 1 (ROADMAP.md).
	_ = path
	return nil, nil, nil
}

// Validate checks a Schema, returning every violation with a field path —
// never just the first.
func Validate(s *Schema) error {
	// Phase 1 (ROADMAP.md).
	_ = s
	return nil
}

// Redacted renders a Schema with all secret material replaced — the only
// rendering any echo path (diff, snapshot API, error messages) may use.
func Redacted(s *Schema) *Schema {
	// Phase 1 (ROADMAP.md).
	_ = s
	return s
}

// DiffSchemas computes the field-path diff between two schemas.
func DiffSchemas(old, new *Schema) *Diff {
	// Phase 1 (ROADMAP.md).
	_, _ = old, new
	return &Diff{}
}
