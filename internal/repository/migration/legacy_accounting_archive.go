package migration

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"math"
	"strings"

	"gorm.io/gorm"
)

const legacyAccountingArchiveVersion = 1
const legacyAccountingArchiveManifest = "keeper_legacy_accounting_archives"
const legacyAccountingRowID = "keeper_source_rowid"

var legacyOverviewTables = []string{"usage_overview_hourly_stats", "usage_overview_daily_stats"}

type legacyAccountingArchive struct {
	SourceTable   string `gorm:"primaryKey"`
	ArchiveTable  string
	SchemaVersion int
	SchemaJSON    string
	ColumnsJSON   string
	RowCount      int64
	RowsSHA256    string
}

type legacyAccountingSchemaEntry struct {
	Type string
	Name string
	SQL  *string
}

// PreserveLegacyOverviewArchives must share the transaction that first clears
// the old buckets. No-affinity columns retain SQLite storage classes; the M1
// SQLite backup remains the authority for the original database bytes.
func PreserveLegacyOverviewArchives(tx *gorm.DB) error {
	if err := tx.Exec(`CREATE TABLE IF NOT EXISTS keeper_legacy_accounting_archives (
		source_table TEXT PRIMARY KEY, archive_table TEXT NOT NULL, schema_version INTEGER NOT NULL,
		schema_json TEXT NOT NULL, columns_json TEXT NOT NULL, row_count INTEGER NOT NULL, rows_sha256 TEXT NOT NULL
	)`).Error; err != nil {
		return fmt.Errorf("create legacy accounting archive manifest: %w", err)
	}
	for _, source := range legacyOverviewTables {
		if err := preserveLegacyAccountingTable(tx, source); err != nil {
			return err
		}
	}
	return nil
}

// VerifyLegacyOverviewArchives checks retained data on restart without comparing
// it to the intentionally rebuilt live buckets or silently recreating it.
func VerifyLegacyOverviewArchives(tx *gorm.DB) error {
	for _, source := range legacyOverviewTables {
		var record legacyAccountingArchive
		if err := tx.Table(legacyAccountingArchiveManifest).Where("source_table = ?", source).Take(&record).Error; err != nil {
			return fmt.Errorf("load legacy accounting archive %s: %w", source, err)
		}
		if err := verifyLegacyAccountingArchive(tx, record); err != nil {
			return err
		}
	}
	return nil
}

func preserveLegacyAccountingTable(tx *gorm.DB, source string) error {
	archive := "keeper_legacy_" + source
	var schema []legacyAccountingSchemaEntry
	if err := tx.Raw("SELECT type, name, sql FROM sqlite_master WHERE tbl_name = ? ORDER BY type, name", source).Scan(&schema).Error; err != nil {
		return fmt.Errorf("read legacy accounting schema %s: %w", source, err)
	}
	if len(schema) == 0 {
		return fmt.Errorf("legacy accounting source %s is missing", source)
	}
	var info []struct {
		Name   string
		Hidden int
	}
	if err := tx.Raw("PRAGMA table_xinfo(" + legacyAccountingIdentifier(source) + ")").Scan(&info).Error; err != nil {
		return fmt.Errorf("read legacy accounting columns %s: %w", source, err)
	}
	columns := make([]string, 0, len(info))
	for _, column := range info {
		if column.Hidden != 0 || column.Name == legacyAccountingRowID {
			return fmt.Errorf("legacy accounting source %s has unsupported columns", source)
		}
		columns = append(columns, column.Name)
	}
	if len(columns) == 0 {
		return fmt.Errorf("legacy accounting source %s has no columns", source)
	}
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	columnsJSON, err := json.Marshal(columns)
	if err != nil {
		return err
	}
	var records []legacyAccountingArchive
	if err := tx.Table(legacyAccountingArchiveManifest).Where("source_table = ?", source).Find(&records).Error; err != nil {
		return fmt.Errorf("read legacy accounting archive manifest: %w", err)
	}
	if len(records) > 0 {
		record := records[0]
		if len(records) != 1 || record.SchemaJSON != string(schemaJSON) || record.ColumnsJSON != string(columnsJSON) {
			return fmt.Errorf("legacy accounting source %s schema differs from retained archive", source)
		}
		if err := verifyLegacyAccountingArchive(tx, record); err != nil {
			return err
		}
		return compareLegacyAccountingSource(tx, source, archive, columns)
	}
	if tx.Migrator().HasTable(archive) {
		return fmt.Errorf("legacy accounting archive %s exists without a manifest", archive)
	}
	quoted := make([]string, 0, len(columns)+1)
	quoted = append(quoted, legacyAccountingIdentifier(legacyAccountingRowID))
	for _, column := range columns {
		quoted = append(quoted, legacyAccountingIdentifier(column))
	}
	if err := tx.Exec("CREATE TABLE " + legacyAccountingIdentifier(archive) + " (" + strings.Join(quoted, ", ") + ")").Error; err != nil {
		return fmt.Errorf("create legacy accounting archive %s: %w", source, err)
	}
	if err := tx.Exec("INSERT INTO " + legacyAccountingIdentifier(archive) + " SELECT rowid, " + strings.Join(quoted[1:], ", ") + " FROM " + legacyAccountingIdentifier(source) + " ORDER BY rowid").Error; err != nil {
		return fmt.Errorf("copy legacy accounting archive %s: %w", source, err)
	}
	if err := compareLegacyAccountingSource(tx, source, archive, columns); err != nil {
		return err
	}
	count, digest, err := digestLegacyAccountingRows(tx, archive, columns)
	if err != nil {
		return err
	}
	record := legacyAccountingArchive{SourceTable: source, ArchiveTable: archive, SchemaVersion: legacyAccountingArchiveVersion,
		SchemaJSON: string(schemaJSON), ColumnsJSON: string(columnsJSON), RowCount: count, RowsSHA256: digest}
	if err := tx.Table(legacyAccountingArchiveManifest).Create(&record).Error; err != nil {
		return fmt.Errorf("save legacy accounting archive manifest: %w", err)
	}
	return nil
}

func verifyLegacyAccountingArchive(tx *gorm.DB, record legacyAccountingArchive) error {
	if record.SchemaVersion != legacyAccountingArchiveVersion || record.ArchiveTable != "keeper_legacy_"+record.SourceTable || record.RowCount < 0 {
		return fmt.Errorf("invalid legacy accounting archive identity")
	}
	var columns []string
	var schema []legacyAccountingSchemaEntry
	if json.Unmarshal([]byte(record.ColumnsJSON), &columns) != nil || len(columns) == 0 ||
		json.Unmarshal([]byte(record.SchemaJSON), &schema) != nil || len(schema) == 0 {
		return fmt.Errorf("invalid legacy accounting archive schema")
	}
	var info []struct {
		Name string
		Type string
	}
	if err := tx.Raw("PRAGMA table_info(" + legacyAccountingIdentifier(record.ArchiveTable) + ")").Scan(&info).Error; err != nil {
		return fmt.Errorf("inspect legacy accounting archive: %w", err)
	}
	want := append([]string{legacyAccountingRowID}, columns...)
	if len(info) != len(want) {
		return fmt.Errorf("legacy accounting archive %s columns differ", record.SourceTable)
	}
	for index, column := range info {
		if column.Name != want[index] || column.Type != "" {
			return fmt.Errorf("legacy accounting archive %s column identity or affinity differs", record.SourceTable)
		}
	}
	count, digest, err := digestLegacyAccountingRows(tx, record.ArchiveTable, columns)
	if err != nil {
		return err
	}
	if count != record.RowCount || digest != record.RowsSHA256 {
		return fmt.Errorf("legacy accounting archive %s rows differ from retained manifest", record.SourceTable)
	}
	return nil
}

func compareLegacyAccountingSource(tx *gorm.DB, source, archive string, columns []string) error {
	left, right := []string{"rowid"}, []string{legacyAccountingIdentifier(legacyAccountingRowID)}
	for _, column := range columns {
		name := legacyAccountingIdentifier(column)
		left = append(left, "typeof("+name+")", name+" COLLATE BINARY")
		right = append(right, "typeof("+name+")", name+" COLLATE BINARY")
	}
	a := "SELECT " + strings.Join(left, ", ") + " FROM " + legacyAccountingIdentifier(source)
	b := "SELECT " + strings.Join(right, ", ") + " FROM " + legacyAccountingIdentifier(archive)
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		var mismatch bool
		if err := tx.Raw("SELECT EXISTS(SELECT * FROM (" + pair[0] + " EXCEPT " + pair[1] + ") LIMIT 1)").Scan(&mismatch).Error; err != nil {
			return fmt.Errorf("compare legacy accounting archive %s: %w", source, err)
		}
		if mismatch {
			return fmt.Errorf("legacy accounting archive %s differs from source", source)
		}
	}
	return nil
}

func digestLegacyAccountingRows(tx *gorm.DB, table string, columns []string) (int64, string, error) {
	selects := []string{legacyAccountingIdentifier(legacyAccountingRowID)}
	for _, column := range columns {
		selects = append(selects, legacyAccountingIdentifier(column))
	}
	rows, err := tx.Raw("SELECT " + strings.Join(selects, ", ") + " FROM " + legacyAccountingIdentifier(table) + " ORDER BY " + legacyAccountingIdentifier(legacyAccountingRowID)).Rows()
	if err != nil {
		return 0, "", fmt.Errorf("read legacy accounting archive %s: %w", table, err)
	}
	defer rows.Close()
	digest := sha256.New()
	var count int64
	for rows.Next() {
		values, dest := make([]any, len(selects)), make([]any, len(selects))
		for index := range values {
			dest[index] = &values[index]
		}
		if err := rows.Scan(dest...); err != nil {
			return 0, "", fmt.Errorf("scan legacy accounting archive: %w", err)
		}
		for _, value := range values {
			if err := hashLegacyAccountingValue(digest, value); err != nil {
				return 0, "", err
			}
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, "", fmt.Errorf("read legacy accounting archive: %w", err)
	}
	return count, hex.EncodeToString(digest.Sum(nil)), nil
}

func hashLegacyAccountingValue(digest hash.Hash, value any) error {
	var encoded [8]byte
	switch value := value.(type) {
	case nil:
		digest.Write([]byte{'n'})
	case int64:
		digest.Write([]byte{'i'})
		binary.BigEndian.PutUint64(encoded[:], uint64(value))
		digest.Write(encoded[:])
	case float64:
		digest.Write([]byte{'r'})
		binary.BigEndian.PutUint64(encoded[:], math.Float64bits(value))
		digest.Write(encoded[:])
	case string:
		digest.Write([]byte{'t'})
		binary.BigEndian.PutUint64(encoded[:], uint64(len(value)))
		digest.Write(encoded[:])
		digest.Write([]byte(value))
	case []byte:
		digest.Write([]byte{'b'})
		binary.BigEndian.PutUint64(encoded[:], uint64(len(value)))
		digest.Write(encoded[:])
		digest.Write(value)
	default:
		return fmt.Errorf("unsupported SQLite legacy accounting storage class %T", value)
	}
	return nil
}

func legacyAccountingIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
