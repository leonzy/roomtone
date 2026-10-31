package models

import "strings"

// LyricSearchError preserves provider diagnostics instead of disguising outages as missing lyrics.
type LyricSearchError struct {
	Status  int
	Reasons []string
}

func (e *LyricSearchError) Error() string { return strings.Join(e.Reasons, "；") }
