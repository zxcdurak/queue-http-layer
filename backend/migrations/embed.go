// Package migrations встраивает SQL-миграции в бинарь.
//
// Нумерация: 001–019 у БЭК-1, 004_idempotency.sql у БЭК-2, 020+ у БЭК-3.
package migrations

import "embed"

// FS содержит все .sql-миграции проекта.
//
//go:embed *.sql
var FS embed.FS
