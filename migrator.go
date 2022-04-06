package clickhouse

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

// Errors enumeration
var (
	ErrRenameColumnUnsupported = errors.New("renaming column is not supported in your clickhouse version < 20.4.")
	ErrRenameIndexUnsupported  = errors.New("renaming index is not supported")
	ErrCreateIndexFailed       = errors.New("failed to create index with name")
)

type Migrator struct {
	migrator.Migrator
	Dialector
}

// Database

func (m Migrator) CurrentDatabase() (name string) {
	m.DB.Raw("SELECT currentDatabase()").Row().Scan(&name)
	return
}

func (m Migrator) FullDataTypeOf(field *schema.Field) (expr clause.Expr) {
	// Infer the ClickHouse datatype from schema.Field information
	expr.SQL = m.Migrator.DataTypeOf(field)

	// NOTE:
	// NULL and UNIQUE keyword is not supported in clickhouse.
	// Hence, skipping checks for field.Unique and field.NotNull

	// Build DEFAULT clause after DataTypeOf() expression optionally
	if field.HasDefaultValue && (field.DefaultValueInterface != nil || field.DefaultValue != "") {
		if field.DefaultValueInterface != nil {
			expr.SQL += " DEFAULT " + m.Dialector.Explain("?", field.DefaultValueInterface)
		} else if field.DefaultValue != "(-)" {
			expr.SQL += " DEFAULT " + field.DefaultValue
		}
	}

	// Build COMMENT clause optionally after DEFAULT
	if comment, ok := field.TagSettings["COMMENT"]; ok {
		expr.SQL += " COMMENT " + m.Dialector.Explain("?", comment)
	}

	// Build TTl clause optionally after COMMENT
	if ttl, ok := field.TagSettings["TTL"]; ok && ttl != "" {
		expr.SQL += " TTL " + ttl
	}

	// Build CODEC compression algorithm optionally
	// NOTE: the codec algo name is case sensitive!
	if codecstr, ok := field.TagSettings["CODEC"]; ok && codecstr != "" {
		// parse codec one by one in the codec option
		codecSlice := make([]string, 0, 10)
		for _, codec := range strings.Split(codecstr, ",") {
			codecSlice = append(codecSlice, codec)
		}
		codecArgsSQL := m.Dialector.DefaultCompression
		if len(codecSlice) > 0 {
			codecArgsSQL = strings.Join(codecSlice, ",")
		}
		codecSQL := fmt.Sprintf(" CODEC(%s) ", codecArgsSQL)
		expr.SQL += codecSQL
	}

	return expr
}

// Tables

func (m Migrator) CreateTable(models ...interface{}) error {
	for _, model := range m.ReorderModels(models, false) {
		tx := m.DB.Session(new(gorm.Session))
		if err := m.RunWithValue(model, func(stmt *gorm.Statement) (err error) {
			var (
				createTableSQL = "CREATE TABLE ?%s(%s %s %s) %s"
				args           = []interface{}{clause.Table{Name: stmt.Table}}
			)

			// Step 1. Build column datatype SQL string
			columnSlice := make([]string, 0, len(stmt.Schema.DBNames))
			for _, dbName := range stmt.Schema.DBNames {
				field := stmt.Schema.FieldsByDBName[dbName]
				columnSlice = append(columnSlice, "? ?")
				args = append(args,
					clause.Column{Name: dbName},
					m.FullDataTypeOf(field),
				)
			}
			columnStr := strings.Join(columnSlice, ",")

			// Step 2. Build constraint check SQL string if any constraint
			constrSlice := make([]string, 0, len(columnSlice))
			for _, check := range stmt.Schema.ParseCheckConstraints() {
				constrSlice = append(constrSlice, "CONSTRAINT ? CHECK ?")
				args = append(args,
					clause.Column{Name: check.Name},
					clause.Expr{SQL: check.Constraint},
				)
			}
			constrStr := strings.Join(constrSlice, ",")
			if len(constrSlice) > 0 {
				constrStr = ", " + constrStr
			}

			// Step 3. Build index SQL string
			// NOTE: clickhouse does not support for index class.
			indexSlice := make([]string, 0, 10)
			for _, index := range stmt.Schema.ParseIndexes() {
				if m.CreateIndexAfterCreateTable {
					defer func(model interface{}, indexName string) {
						// TODO (iqdf): what if there are multiple errors
						// when creating indices after create table?
						err = tx.Migrator().CreateIndex(model, indexName)
					}(model, index.Name)
					continue
				}
				// TODO(iqdf): support primary key by put it as pass the fieldname
				// as MergeTree(...) parameters. But somehow it complained.
				// Note that primary key doesn't ensure uniqueness

				// Get indexing type `gorm:"index,type:minmax"`
				// Choice: minmax | set(n) | ngrambf_v1(n, size, hash, seed) | bloomfilter()
				indexType := m.Dialector.DefaultIndexType
				if index.Type != "" {
					indexType = index.Type
				}

				// Get expression for index options
				// Syntax: (`colname1`, ...)
				buildIndexOptions := tx.Migrator().(migrator.BuildIndexOptionsInterface)
				indexOptions := buildIndexOptions.BuildIndexOptions(index.Fields, stmt)

				// Stringify index builder
				// TODO (iqdf): support granularity
				str := fmt.Sprintf("INDEX $1 $2 TYPE %s GRANULARITY %d", indexType, m.getIndexGranularityOption(index.Fields))
				indexSlice = append(indexSlice, str)
				args = append(args, clause.Expr{SQL: index.Name}, indexOptions)
			}
			indexStr := strings.Join(indexSlice, ", ")
			if len(indexSlice) > 0 {
				indexStr = ", " + indexStr
			}

			// Step 4. Finally assemble CREATE TABLE ... SQL string
			engineOpts := m.Dialector.DefaultTableEngineOpts
			if tableOption, ok := m.DB.Get("gorm:table_options"); ok {
				engineOpts = fmt.Sprint(tableOption)
			}

			clusterOpts := ""
			if clusterOption, ok := m.DB.Get("gorm:table_cluster_options"); ok {
				clusterOpts = " " + fmt.Sprint(clusterOption) + " "
			}

			createTableSQL = fmt.Sprintf(createTableSQL, clusterOpts, columnStr, constrStr, indexStr, engineOpts)

			err = tx.Exec(createTableSQL, args...).Error

			return
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m Migrator) HasTable(value interface{}) bool {
	var count int64
	m.RunWithValue(value, func(stmt *gorm.Statement) error {
		currentDatabase := m.DB.Migrator().CurrentDatabase()
		return m.DB.Raw(
			"SELECT count(*) FROM system.tables WHERE database = $1 AND name = $2 AND is_temporary = $3",
			currentDatabase,
			stmt.Table,
			uint8(0)).Row().Scan(&count)
	})
	return count > 0
}

// Columns

func (m Migrator) AddColumn(value interface{}, field string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if field := stmt.Schema.LookUpField(field); field != nil {
			return m.DB.Exec(
				"ALTER TABLE ? ADD COLUMN IF NOT EXISTS ? ?",
				clause.Table{Name: stmt.Table}, clause.Column{Name: field.DBName},
				m.FullDataTypeOf(field),
			).Error
		}
		return fmt.Errorf("failed to look up field with name: %s", field)
	})
}

func (m Migrator) DropColumn(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if field := stmt.Schema.LookUpField(name); field != nil {
			name = field.DBName
		}
		return m.DB.Exec(
			"ALTER TABLE $1 DROP COLUMN $2",
			clause.Table{Name: stmt.Table}, clause.Column{Name: name},
		).Error
	})
}

func (m Migrator) AlterColumn(value interface{}, field string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if field := stmt.Schema.LookUpField(field); field != nil {
			return m.DB.Exec(
				"ALTER TABLE $1 MODIFY COLUMN $2 $3",
				clause.Table{Name: stmt.Table},
				clause.Column{Name: field.DBName},
				m.FullDataTypeOf(field),
			).Error
		}
		return fmt.Errorf("altercolumn() failed to look up column with name: %s", field)
	})
}

// NOTE: Only supported after ClickHouse 20.4 and above.
// See: https://github.com/ClickHouse/ClickHouse/issues/146
func (m Migrator) RenameColumn(value interface{}, oldName, newName string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if !m.Dialector.DontSupportRenameColumn {
			var field *schema.Field
			if f := stmt.Schema.LookUpField(oldName); f != nil {
				oldName = f.DBName
				field = f
			}
			if f := stmt.Schema.LookUpField(newName); f != nil {
				newName = f.DBName
				field = f
			}
			if field != nil {
				return m.DB.Exec(
					"ALTER TABLE $1 RENAME COLUMN $2 TO $3",
					clause.Table{Name: stmt.Table},
					clause.Column{Name: oldName},
					clause.Column{Name: newName},
				).Error
			}
			return fmt.Errorf("renamecolumn() failed to look up column with name: %s", oldName)
		}
		return ErrRenameIndexUnsupported
	})
}

func (m Migrator) HasColumn(value interface{}, field string) bool {
	var count int64
	m.RunWithValue(value, func(stmt *gorm.Statement) error {
		currentDatabase := m.DB.Migrator().CurrentDatabase()
		name := field

		if stmt.Schema != nil {
			if field := stmt.Schema.LookUpField(field); field != nil {
				name = field.DBName
			}
		}

		return m.DB.Raw(
			"SELECT count(*) FROM system.columns WHERE database = $1 AND table = $2 AND name = $3",
			currentDatabase, stmt.Table, name,
		).Row().Scan(&count)
	})

	return count > 0
}

// ColumnTypes return columnTypes []gorm.ColumnType and execErr error
func (m Migrator) ColumnTypes(value interface{}) ([]gorm.ColumnType, error) {
	columnTypes := make([]gorm.ColumnType, 0)
	execErr := m.RunWithValue(value, func(stmt *gorm.Statement) (err error) {
		rows, err := m.DB.Session(&gorm.Session{}).Table(stmt.Table).Limit(1).Rows()
		if err != nil {
			return err
		}

		defer func() {
			err = rows.Close()
		}()

		var rawColumnTypes []*sql.ColumnType
		rawColumnTypes, err = rows.ColumnTypes()

		columnTypeSQL := "SELECT name, type, default_expression, comment, is_in_primary_key, character_octet_length, numeric_precision, numeric_precision_radix, numeric_scale, datetime_precision FROM system.columns WHERE database = $1 AND table = $2"
		columns, rowErr := m.DB.Raw(columnTypeSQL, m.CurrentDatabase(), stmt.Table).Rows()
		if rowErr != nil {
			return rowErr
		}

		defer columns.Close()

		for columns.Next() {
			var (
				column            migrator.ColumnType
				datetimePrecision sql.NullInt64
				radixValue        sql.NullInt64
				values            = []interface{}{
					&column.NameValue, &column.DataTypeValue, &column.DefaultValueValue, &column.CommentValue, &column.PrimaryKeyValue, &column.LengthValue, &column.DecimalSizeValue, &radixValue, &column.ScaleValue, &datetimePrecision,
				}
			)

			if scanErr := columns.Scan(values...); scanErr != nil {
				return scanErr
			}

			column.ColumnTypeValue = column.DataTypeValue

			if datetimePrecision.Valid {
				column.DecimalSizeValue = datetimePrecision
			}

			if column.DefaultValueValue.Valid {
				column.DefaultValueValue.String = strings.Trim(column.DefaultValueValue.String, "'")
			}

			for _, c := range rawColumnTypes {
				if c.Name() == column.NameValue.String {
					column.SQLColumnType = c
					break
				}
			}

			columnTypes = append(columnTypes, column)
		}

		return
	})

	return columnTypes, execErr
}

// Indexes

func (m Migrator) BuildIndexOptions(opts []schema.IndexOption, stmt *gorm.Statement) (results []interface{}) {
	for _, indexOpt := range opts {
		str := stmt.Quote(indexOpt.DBName)
		if indexOpt.Expression != "" {
			str = indexOpt.Expression
		}
		results = append(results, clause.Expr{SQL: str})
	}
	return
}

func (m Migrator) CreateIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if index := stmt.Schema.LookIndex(name); index != nil {
			opts := m.BuildIndexOptions(index.Fields, stmt)
			values := []interface{}{
				clause.Table{Name: stmt.Table},
				clause.Column{Name: index.Name},
				opts,
			}

			// Get indexing type `gorm:"index,type:minmax"`
			// Choice: minmax | set(n) | ngrambf_v1(n, size, hash, seed) | bloomfilter()
			indexType := m.Dialector.DefaultIndexType
			if index.Type != "" {
				indexType = index.Type
			}

			// NOTE: concept of UNIQUE | FULLTEXT | SPATIAL index
			// is NOT supported in clickhouse
			createIndexSQL := "ALTER TABLE $1 ADD INDEX $2 $3 TYPE %s GRANULARITY %d"                          // TODO(iqdf): how to inject Granularity
			createIndexSQL = fmt.Sprintf(createIndexSQL, indexType, m.getIndexGranularityOption(index.Fields)) // Granularity: 1 (default)
			return m.DB.Exec(createIndexSQL, values...).Error
		}
		return ErrCreateIndexFailed
	})
}

func (m Migrator) RenameIndex(value interface{}, oldName, newName string) error {
	// TODO(iqdf): drop index and add the index again with different name
	// DROP INDEX ?
	// ADD INDEX ? TYPE ? GRANULARITY ?
	return ErrRenameIndexUnsupported
}

func (m Migrator) DropIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if idx := stmt.Schema.LookIndex(name); idx != nil {
				name = idx.Name
			}
		}
		dropIndexSQL := "ALTER TABLE $1 DROP INDEX $2"
		return m.DB.Exec(dropIndexSQL,
			clause.Table{Name: stmt.Table},
			clause.Column{Name: name}).Error
	})
}

// Helper

// Index

func (m Migrator) getIndexGranularityOption(opts []schema.IndexOption) int {
	for _, indexOpt := range opts {
		if settingStr, ok := indexOpt.Field.TagSettings["INDEX"]; ok {
			// e.g. settingStr: "a,expression:u64*i32,type:minmax,granularity:3"
			for _, str := range strings.Split(settingStr, ",") {
				// e.g. str: "granularity:3"
				keyVal := strings.Split(str, ":")
				if len(keyVal) > 1 && strings.ToLower(keyVal[0]) == "granularity" {
					if len(keyVal) < 2 {
						// continue search for other setting which
						// may contain granularity:<num>
						continue
					}
					// try to convert <num> into an integer > 0
					// if check fails, continue search for other
					// settings which may contain granularity:<num>
					num, err := strconv.Atoi(keyVal[1])
					if err != nil || num < 0 {
						continue
					}
					return num
				}
			}
		}
	}
	return m.Dialector.DefaultGranularity
}
