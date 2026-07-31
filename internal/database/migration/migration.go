package migration

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
)

//go:embed *.up.sql
var files embed.FS

func Apply(ctx context.Context, db *gorm.DB) error {
	if err := db.WithContext(ctx).Exec("CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)").Error; err != nil {
		return err
	}
	entries, err := files.ReadDir(".")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		version := strings.TrimSuffix(e.Name(), ".up.sql")
		var n int64
		if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version).Scan(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		sql, _ := files.ReadFile(e.Name())
		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(string(sql)).Error; err != nil {
				return fmt.Errorf("migration %s: %w", version, err)
			}
			return tx.Exec("INSERT INTO schema_migrations(version) VALUES (?)", version).Error
		}); err != nil {
			return err
		}
	}
	return nil
}
