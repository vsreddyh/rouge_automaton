// Package souldata embeds the SOUL.md canon so the binary never depends on
// CWD. (go:embed cannot use .., hence this root-level file.)
package souldata

import _ "embed"

// Soul is the full text of skills/rouge-automaton/SOUL.md, baked in at
// compile time. The directive below is the only copy the binary needs;
// SKILLS_DIR at runtime can still override it (see internal/persona).
//
//go:embed skills/rouge-automaton/SOUL.md
var Soul string
