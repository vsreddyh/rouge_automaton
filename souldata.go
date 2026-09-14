// Package souldata embeds the SOUL.md canon so the binary never depends on
// CWD. (go:embed cannot use .., hence this root-level file.)
package souldata

import _ "embed"

// Soul is the embedded skills/rouge-automaton/SOUL.md text.
//
//go:embed skills/rouge-automaton/SOUL.md
var Soul string
