package domain

import "errors"

// ErrPartialBatch reports that a source batch was applied but some entries
// were rejected as unrepresentable. Every upstream list contains such
// entries, so this is a routine outcome rather than a source failure; callers
// keep the batch and must not degrade the service or log it as an error.
var ErrPartialBatch = errors.New("source batch partially accepted")

// ErrNoAdmissibleNodes reports that a source contained no representable nodes.
// A source that keeps publishing only unusable entries is worth reporting.
var ErrNoAdmissibleNodes = errors.New("source contains no representable nodes")
