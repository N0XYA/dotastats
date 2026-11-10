package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// SettingInt returns the integer stored under key, or def when unset.
func SettingInt(ctx context.Context, db *sql.DB, key string, def int) (int, error) {
	var v string
	err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("setting %s: %q is not a number", key, v)
	}
	return n, nil
}
