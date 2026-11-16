// Package uuidx converts between pgtype.UUID and its string form.
package uuidx

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

func String(u pgtype.UUID) string {
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func Parse(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	err := u.Scan(s)
	return u, err
}