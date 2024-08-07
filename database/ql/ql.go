package ql

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/XSAM/otelsql"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"io"
	nurl "net/url"
	"strings"

	"github.com/hashicorp/go-multierror"
	"go.uber.org/atomic"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	_ "modernc.org/ql/driver"
)

func init() {
	database.Register("ql", &Ql{})
}

var DefaultMigrationsTable = "schema_migrations"
var (
	ErrDatabaseDirty  = fmt.Errorf("database is dirty")
	ErrNilConfig      = fmt.Errorf("no config")
	ErrNoDatabaseName = fmt.Errorf("no database name")
	ErrAppendPEM      = fmt.Errorf("failed to append PEM")
)

type Config struct {
	MigrationsTable string
	DatabaseName    string
}

type Ql struct {
	db       *sql.DB
	isLocked atomic.Bool

	config *Config
}

func WithInstance(ctx context.Context, instance *sql.DB, config *Config) (database.Driver, error) {
	if config == nil {
		return nil, ErrNilConfig
	}

	if err := instance.Ping(); err != nil {
		return nil, err
	}

	if len(config.MigrationsTable) == 0 {
		config.MigrationsTable = DefaultMigrationsTable
	}

	mx := &Ql{
		db:     instance,
		config: config,
	}
	if err := mx.ensureVersionTable(ctx); err != nil {
		return nil, err
	}
	return mx, nil
}

// ensureVersionTable checks if versions table exists and, if not, creates it.
// Note that this function locks the database, which deviates from the usual
// convention of "caller locks" in the Ql type.
func (m *Ql) ensureVersionTable(ctx context.Context) (err error) {
	if err = m.Lock(ctx); err != nil {
		return err
	}

	defer func() {
		if e := m.Unlock(ctx); e != nil {
			if err == nil {
				err = e
			} else {
				err = multierror.Append(err, e)
			}
		}
	}()

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`
	CREATE TABLE IF NOT EXISTS %s (version uint64, dirty bool);
	CREATE UNIQUE INDEX IF NOT EXISTS version_unique ON %s (version);
`, m.config.MigrationsTable, m.config.MigrationsTable)); err != nil {
		if err := tx.Rollback(); err != nil {
			return err
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (m *Ql) Open(ctx context.Context, url string) (database.Driver, error) {
	purl, err := nurl.Parse(url)
	if err != nil {
		return nil, err
	}
	dbfile := strings.Replace(migrate.FilterCustomQuery(purl).String(), "ql://", "", 1)
	db, err := otelsql.Open("ql", dbfile,
		otelsql.WithAttributes(semconv.DBSystemKey.String("ql")))
	if err != nil {
		return nil, err
	}

	err = otelsql.RegisterDBStatsMetrics(db,
		otelsql.WithAttributes(semconv.DBSystemKey.String("ql")))
	if err != nil {
		return nil, err
	}

	migrationsTable := purl.Query().Get("x-migrations-table")
	if len(migrationsTable) == 0 {
		migrationsTable = DefaultMigrationsTable
	}
	mx, err := WithInstance(ctx, db, &Config{
		DatabaseName:    purl.Path,
		MigrationsTable: migrationsTable,
	})
	if err != nil {
		return nil, err
	}
	return mx, nil
}
func (m *Ql) Close(ctx context.Context) error {
	return m.db.Close()
}
func (m *Ql) Drop(ctx context.Context) (err error) {
	query := `SELECT Name FROM __Table`
	tables, err := m.db.Query(query)
	if err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	defer func() {
		if errClose := tables.Close(); errClose != nil {
			err = multierror.Append(err, errClose)
		}
	}()

	tableNames := make([]string, 0)
	for tables.Next() {
		var tableName string
		if err := tables.Scan(&tableName); err != nil {
			return err
		}
		if len(tableName) > 0 {
			if !strings.HasPrefix(tableName, "__") {
				tableNames = append(tableNames, tableName)
			}
		}
	}
	if err := tables.Err(); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	if len(tableNames) > 0 {
		for _, t := range tableNames {
			query := "DROP TABLE " + t
			err = m.executeQuery(query)
			if err != nil {
				return &database.Error{OrigErr: err, Query: []byte(query)}
			}
		}
	}

	return nil
}
func (m *Ql) Lock(ctx context.Context) error {
	if !m.isLocked.CAS(false, true) {
		return database.ErrLocked
	}
	return nil
}
func (m *Ql) Unlock(ctx context.Context) error {
	if !m.isLocked.CAS(true, false) {
		return database.ErrNotLocked
	}
	return nil
}
func (m *Ql) Run(ctx context.Context, migration io.Reader) error {
	migr, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	query := string(migr[:])

	return m.executeQuery(query)
}
func (m *Ql) executeQuery(query string) error {
	tx, err := m.db.Begin()
	if err != nil {
		return &database.Error{OrigErr: err, Err: "transaction start failed"}
	}
	if _, err := tx.Exec(query); err != nil {
		if errRollback := tx.Rollback(); errRollback != nil {
			err = multierror.Append(err, errRollback)
		}
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	if err := tx.Commit(); err != nil {
		return &database.Error{OrigErr: err, Err: "transaction commit failed"}
	}
	return nil
}
func (m *Ql) SetVersion(ctx context.Context, version int, dirty bool) error {
	tx, err := m.db.Begin()
	if err != nil {
		return &database.Error{OrigErr: err, Err: "transaction start failed"}
	}

	query := "TRUNCATE TABLE " + m.config.MigrationsTable
	if _, err := tx.Exec(query); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	// Also re-write the schema version for nil dirty versions to prevent
	// empty schema version for failed down migration on the first migration
	// See: https://github.com/golang-migrate/migrate/issues/330
	if version >= 0 || (version == database.NilVersion && dirty) {
		query := fmt.Sprintf(`INSERT INTO %s (version, dirty) VALUES (uint64(?1), ?2)`,
			m.config.MigrationsTable)
		if _, err := tx.Exec(query, version, dirty); err != nil {
			if errRollback := tx.Rollback(); errRollback != nil {
				err = multierror.Append(err, errRollback)
			}
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}
	}

	if err := tx.Commit(); err != nil {
		return &database.Error{OrigErr: err, Err: "transaction commit failed"}
	}

	return nil
}

func (m *Ql) Version(ctx context.Context) (version int, dirty bool, err error) {
	query := "SELECT version, dirty FROM " + m.config.MigrationsTable + " LIMIT 1"
	err = m.db.QueryRow(query).Scan(&version, &dirty)
	if err != nil {
		return database.NilVersion, false, nil
	}
	return version, dirty, nil
}
