package postgres

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/amacneil/dbmate/v2/pkg/dbmate"
	"github.com/amacneil/dbmate/v2/pkg/dbutil"

	"github.com/lib/pq"
)

func init() {
	dbmate.RegisterDriver(NewDriver, "postgres")
	dbmate.RegisterDriver(NewDriver, "postgresql")
}

// Driver provides top level database functions
type Driver struct {
	migrationsTableName string
	databaseURL         *url.URL
	databaseName        string
	log                 io.Writer
}

// NewDriver initializes the driver
func NewDriver(config dbmate.DriverConfig) dbmate.Driver {
	return &Driver{
		migrationsTableName: config.MigrationsTableName,
		databaseURL:         config.DatabaseURL,
		databaseName:        resolveDatabaseName(config.DatabaseURL),
		log:                 config.Log,
	}
}

func resolveDatabaseName(u *url.URL) (name string) {
	// https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-CONNECT-DBNAME

	if name = u.Path; len(name) > 0 && name[:1] == "/" {
		name = name[1:]
	}
	if name == "" {
		// if there's no path in the URL, but there's an environment variable so use it
		name = os.Getenv("PGDATABASE")
	}
	if name == "" && u.User != nil {
		// if there's no path and no environment variable, fallback to the user's name (if any)
		name = u.User.Username()
	}

	return
}

func connectionArgsForDump(u *url.URL) (args []string) {
	u = dbutil.MustParseURL(u.String()) // clone the URL

	// find schemas from search_path
	query := u.Query()
	schemas := strings.Split(query.Get("search_path"), ",")
	query.Del("search_path")
	u.RawQuery = query.Encode()

	for _, schema := range schemas {
		if schema = strings.TrimSpace(schema); schema != "" {
			args = append(args, "--schema", schema)
		}
	}
	args = append(args, u.String())

	return args
}

// Open creates a new database connection
func (drv *Driver) Open() (*sql.DB, error) {
	return sql.Open("postgres", drv.databaseURL.String())
}

func (drv *Driver) openMaintenanceDB() (*sql.DB, error) {
	u := dbutil.MustParseURL(drv.databaseURL.String()) // clone the URL

	// connect to the maintenance database (default: "postgres")
	u.Path = "postgres"

	return sql.Open("postgres", u.String())
}

// CreateDatabase creates the specified database
func (drv *Driver) CreateDatabase() (err error) {
	fmt.Fprintf(drv.log, "Creating: %s\n", drv.databaseName)

	var db *sql.DB
	if db, err = drv.openMaintenanceDB(); err == nil {
		defer dbutil.MustClose(db)

		_, err = db.Exec(fmt.Sprintf("create database %s;", pq.QuoteIdentifier(drv.databaseName)))
	}

	return
}

// DropDatabase drops the specified database (if it exists)
func (drv *Driver) DropDatabase() (err error) {
	fmt.Fprintf(drv.log, "Dropping: %s\n", drv.databaseName)

	var db *sql.DB
	if db, err = drv.openMaintenanceDB(); err == nil {
		defer dbutil.MustClose(db)

		_, err = db.Exec(fmt.Sprintf("drop database if exists %s;", pq.QuoteIdentifier(drv.databaseName)))
	}

	return
}

func (drv *Driver) schemaMigrationsDump(db *sql.DB) ([]byte, error) {
	migrationsTable, err := drv.quotedMigrationsTableName(db)
	if err != nil {
		return nil, err
	}

	// load applied migrations
	migrations, err := dbutil.QueryColumn(db,
		"select quote_literal(version) from "+migrationsTable+" order by version asc")
	if err != nil {
		return nil, err
	}

	// build migrations table data
	var buf bytes.Buffer
	buf.WriteString("\n--\n-- Dbmate schema migrations\n--\n\n")

	if len(migrations) > 0 {
		buf.WriteString("INSERT INTO " + migrationsTable + " (version) VALUES\n    (" +
			strings.Join(migrations, "),\n    (") +
			");\n")
	}

	return buf.Bytes(), nil
}

// DumpSchema returns the current database schema
func (drv *Driver) DumpSchema(db *sql.DB) ([]byte, error) {
	args := append([]string{
		"--format=plain",
		"--encoding=UTF8",
		"--schema-only",
		"--no-privileges",
		"--no-owner",
	}, connectionArgsForDump(drv.databaseURL)...)

	schema, err := dbutil.RunCommand("pg_dump", args...)
	if err != nil {
		return nil, err
	}

	migrations, err := drv.schemaMigrationsDump(db)
	if err != nil {
		return nil, err
	}

	schema = append(schema, migrations...)
	return dbutil.TrimLeadingSQLComments(schema)
}

// DatabaseExists determines whether the database exists
func (drv *Driver) DatabaseExists() (bool, error) {
	err := drv.ping()
	if err == nil {
		return true, nil
	}

	var pqerr *pq.Error
	if errors.As(err, &pqerr) && pqerr.Code == "3D000" {
		return false, nil
	}
	return false, err
}

// MigrationsTableExists checks if the schema_migrations table exists
func (drv *Driver) MigrationsTableExists(db *sql.DB) (bool, error) {
	schema, migrationsTableNameParts, err := drv.migrationsTableNameParts(db)
	if err != nil {
		return false, err
	}

	migrationsTable := strings.Join(migrationsTableNameParts, ".")
	exists := false
	err = db.QueryRow("SELECT 1 FROM information_schema.tables "+
		"WHERE  table_schema = $1 "+
		"AND    table_name   = $2",
		schema, migrationsTable).
		Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}

	return exists, err
}

// CreateMigrationsTable creates the schema_migrations table
func (drv *Driver) CreateMigrationsTable(db *sql.DB) error {
	schema, migrationsTable, err := drv.quotedMigrationsTableNameParts(db)
	if err != nil {
		return err
	}

	// first attempt at creating migrations table
	createTableStmt := fmt.Sprintf(
		"create table if not exists %s.%s (version varchar(128) primary key)",
		schema, migrationsTable)
	_, err = db.Exec(createTableStmt)
	if err == nil {
		// table exists or created successfully
		return nil
	}

	// catch 'schema does not exist' error
	pqErr, ok := err.(*pq.Error)
	if !ok || pqErr.Code != "3F000" {
		// unknown error
		return err
	}

	// in theory we could attempt to create the schema every time, but we avoid that
	// in case the user doesn't have permissions to create schemas
	fmt.Fprintf(drv.log, "Creating schema: %s\n", schema)
	_, err = db.Exec(fmt.Sprintf("create schema if not exists %s", schema))
	if err != nil {
		return err
	}

	// second and final attempt at creating migrations table
	_, err = db.Exec(createTableStmt)
	return err
}

// SelectMigrations returns a list of applied migrations
// with an optional limit (in descending order)
func (drv *Driver) SelectMigrations(db *sql.DB, limit int) (map[string]bool, error) {
	migrationsTable, err := drv.quotedMigrationsTableName(db)
	if err != nil {
		return nil, err
	}

	query := "select version from " + migrationsTable + " order by version desc"
	if limit >= 0 {
		query = fmt.Sprintf("%s limit %d", query, limit)
	}
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}

	defer dbutil.MustClose(rows)

	migrations := map[string]bool{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}

		migrations[version] = true
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return migrations, nil
}

// InsertMigration adds a new migration record
func (drv *Driver) InsertMigration(db dbutil.Transaction, version string) error {
	migrationsTable, err := drv.quotedMigrationsTableName(db)
	if err != nil {
		return err
	}

	_, err = db.Exec("insert into "+migrationsTable+" (version) values ($1)", version)

	return err
}

// DeleteMigration removes a migration record
func (drv *Driver) DeleteMigration(db dbutil.Transaction, version string) error {
	migrationsTable, err := drv.quotedMigrationsTableName(db)
	if err != nil {
		return err
	}

	_, err = db.Exec("delete from "+migrationsTable+" where version = $1", version)

	return err
}

// Ping verifies a connection to the database server. It does not verify whether the
// specified database exists.
func (drv *Driver) Ping() (err error) {
	var pqerr *pq.Error
	if errors.As(drv.ping(), &pqerr) && pqerr.Code == "3D000" {
		err = nil // ignore 'database does not exist' error
	}

	return
}

func (drv *Driver) ping() (err error) {
	var db *sql.DB
	if db, err = drv.Open(); err == nil {
		defer dbutil.MustClose(db)

		err = db.Ping()
	}
	return
}

// Return a normalized version of the driver-specific error type.
func (drv *Driver) QueryError(query string, err error) error {
	position := 0

	if pqErr, ok := err.(*pq.Error); ok {
		if pos, err := strconv.Atoi(pqErr.Position); err == nil {
			position = pos
		}
	}

	return &dbmate.QueryError{Err: err, Query: query, Position: position}
}

func (drv *Driver) quotedMigrationsTableName(db dbutil.Transaction) (string, error) {
	schema, name, err := drv.quotedMigrationsTableNameParts(db)
	if err != nil {
		return "", err
	}

	return schema + "." + name, nil
}

func (drv *Driver) migrationsTableNameParts(db dbutil.Transaction) (string, []string, error) {
	schema := ""
	tableNameParts := strings.Split(drv.migrationsTableName, ".")
	if len(tableNameParts) > 1 {
		// schema specified as part of table name
		schema, tableNameParts = tableNameParts[0], tableNameParts[1:]
	}

	if schema == "" {
		// no schema specified with table name, try URL search path if available
		searchPath := strings.Split(drv.databaseURL.Query().Get("search_path"), ",")
		schema = strings.TrimSpace(searchPath[0])
	}

	var err error
	if schema == "" {
		// if no URL available, use current schema
		// this is a hack because we don't always have the URL context available
		schema, err = dbutil.QueryValue(db, "select current_schema()")
		if err != nil {
			return "", nil, err
		}
	}

	// fall back to public schema as last resort
	if schema == "" {
		schema = "public"
	}

	return schema, tableNameParts, nil
}

func (drv *Driver) quotedMigrationsTableNameParts(db dbutil.Transaction) (string, string, error) {
	schema, tableNameParts, err := drv.migrationsTableNameParts(db)

	if err != nil {
		return "", "", err
	}

	// quote all parts
	// use server rather than client to do this to avoid unnecessary quotes
	// (which would change schema.sql diff)
	tableNameParts = append([]string{schema}, tableNameParts...)
	quotedNameParts, err := dbutil.QueryColumn(db, "select quote_ident(unnest($1::text[]))", pq.Array(tableNameParts))
	if err != nil {
		return "", "", err
	}

	// if more than one part, we already have a schema
	return quotedNameParts[0], strings.Join(quotedNameParts[1:], "."), nil
}
