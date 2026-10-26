package database

import _ "embed"

// Schema is provisioned by deployment, never by service startup.
//
//go:embed schema.sql
var Schema string
