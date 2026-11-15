package migration

import "gorm.io/gorm"

// Keep legacy ranking state until a separately qualified read projection exists.
func removeRankingMigration(tx *gorm.DB) error {
	return nil
}
