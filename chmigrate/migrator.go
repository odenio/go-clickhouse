package chmigrate

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/uptrace/go-clickhouse/ch"
)

type MigratorOption func(m *Migrator)

func WithTableName(table string) MigratorOption {
	return func(m *Migrator) {
		m.table = table
	}
}

func WithLocksTableName(table string) MigratorOption {
	return func(m *Migrator) {
		m.locksTable = table
	}
}

// WithApplyOnSuccess sets the migrator to only mark migrations as applied/unapplied
// when their up/down is successful
func WithApplyOnSuccess() MigratorOption {
	return func(m *Migrator) {
		m.applyOnSuccess = true
	}
}

type Migrator struct {
	db         *ch.DB
	migrations *Migrations

	ms MigrationSlice

	table          string
	locksTable     string
	applyOnSuccess bool
}

func NewMigrator(db *ch.DB, migrations *Migrations, opts ...MigratorOption) *Migrator {
	m := &Migrator{
		db:         db,
		migrations: migrations,

		ms: migrations.ms,

		table:      "ch_migrations",
		locksTable: "ch_migration_locks",
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func (m *Migrator) DB() *ch.DB {
	return m.db
}

// MigrationsWithStatus returns migrations with status in ascending order.
func (m *Migrator) MigrationsWithStatus(ctx context.Context) (MigrationSlice, error) {
	sorted, _, err := m.migrationsWithStatus(ctx)
	return sorted, err
}

func (m *Migrator) migrationsWithStatus(ctx context.Context) (MigrationSlice, int64, error) {
	sorted := m.migrations.Sorted()

	applied, err := m.selectAppliedMigrations(ctx)
	if err != nil {
		return nil, 0, err
	}

	appliedMap := migrationMap(applied)
	for i := range sorted {
		m1 := &sorted[i]
		if m2, ok := appliedMap[m1.Name]; ok {
			m1.GroupID = m2.GroupID
			m1.MigratedAt = m2.MigratedAt
		}
	}

	return sorted, applied.LastGroupID(), nil
}

func (m *Migrator) Init(ctx context.Context) error {
	if _, err := m.db.NewCreateTable().
		Model((*Migration)(nil)).
		ModelTableExpr(m.table).
		IfNotExists().
		Exec(ctx); err != nil {
		return err
	}
	if _, err := m.db.NewCreateTable().
		Model((*migrationLock)(nil)).
		ModelTableExpr(m.locksTable).
		IfNotExists().
		Exec(ctx); err != nil {
		return err
	}
	return nil
}

func (m *Migrator) Reset(ctx context.Context) error {
	if _, err := m.db.NewDropTable().
		Model((*Migration)(nil)).
		ModelTableExpr(m.table).
		IfExists().
		Exec(ctx); err != nil {
		return err
	}
	if _, err := m.db.NewDropTable().
		Model((*migrationLock)(nil)).
		ModelTableExpr(m.locksTable).
		IfExists().
		Exec(ctx); err != nil {
		return err
	}
	return m.Init(ctx)
}

// Migrate runs unapplied migrations. If a migration fails, migrate immediately exits.
func (m *Migrator) Migrate(ctx context.Context, opts ...MigrationOption) (*MigrationGroup, error) {
	cfg := newMigrationConfig(opts)

	if err := m.validate(); err != nil {
		return nil, err
	}

	if err := m.Lock(ctx); err != nil {
		return nil, err
	}
	defer m.Unlock(ctx) //nolint:errcheck

	migrations, lastGroupID, err := m.migrationsWithStatus(ctx)
	if err != nil {
		return nil, err
	}

	group := &MigrationGroup{
		Migrations: migrations.Unapplied(),
	}
	if len(group.Migrations) == 0 {
		return group, nil
	}
	group.ID = lastGroupID + 1

	for i := range group.Migrations {
		migration := &group.Migrations[i]
		migration.GroupID = group.ID

		var err error
		if !cfg.nop || migration.Up != nil {
			err = migration.Up(ctx, m.db)
			// If the migration failed and we only apply on success, return error
			if err != nil && m.applyOnSuccess {
				return group, err
			}
		}

		if applyErr := m.MarkApplied(ctx, migration); err != nil {
			return group, applyErr
		}

		// If the migration failed, return error
		if err != nil {
			return group, err
		}
	}

	return group, nil
}

func (m *Migrator) Rollback(ctx context.Context, opts ...MigrationOption) (*MigrationGroup, error) {
	cfg := newMigrationConfig(opts)

	if err := m.validate(); err != nil {
		return nil, err
	}

	if err := m.Lock(ctx); err != nil {
		return nil, err
	}
	defer m.Unlock(ctx) //nolint:errcheck

	migrations, err := m.MigrationsWithStatus(ctx)
	if err != nil {
		return nil, err
	}

	lastGroup := migrations.LastGroup()

	for i := len(lastGroup.Migrations) - 1; i >= 0; i-- {
		migration := &lastGroup.Migrations[i]

		var err error
		if !cfg.nop && migration.Down != nil {
			err = migration.Down(ctx, m.db)
			// If the rollback failed and we only mark unapplied on success, return error
			if err != nil && m.applyOnSuccess {
				return lastGroup, err
			}
		}

		if unapplyErr := m.MarkUnapplied(ctx, migration); err != nil {
			return lastGroup, unapplyErr
		}

		// If the rollback failed, return error
		if err != nil {
			return lastGroup, err
		}
	}

	return lastGroup, nil
}

type goMigrationConfig struct {
	packageName string
}

type GoMigrationOption func(cfg *goMigrationConfig)

func WithPackageName(name string) GoMigrationOption {
	return func(cfg *goMigrationConfig) {
		cfg.packageName = name
	}
}

// CreateGoMigration creates a Go migration file.
func (m *Migrator) CreateGoMigration(
	ctx context.Context, name string, opts ...GoMigrationOption,
) (*MigrationFile, error) {
	cfg := &goMigrationConfig{
		packageName: "migrations",
	}
	for _, opt := range opts {
		opt(cfg)
	}

	name, err := m.genMigrationName(name)
	if err != nil {
		return nil, err
	}

	fname := name + ".go"
	fpath := filepath.Join(m.migrations.getDirectory(), fname)
	content := fmt.Sprintf(goTemplate, cfg.packageName)

	if err := ioutil.WriteFile(fpath, []byte(content), 0o644); err != nil {
		return nil, err
	}

	mf := &MigrationFile{
		Name:    fname,
		Path:    fpath,
		Content: content,
	}
	return mf, nil
}

// CreateSQLMigrations creates an up and down SQL migration files.
func (m *Migrator) CreateSQLMigrations(ctx context.Context, name string) ([]*MigrationFile, error) {
	name, err := m.genMigrationName(name)
	if err != nil {
		return nil, err
	}

	up, err := m.createSQL(ctx, name+".up.sql")
	if err != nil {
		return nil, err
	}

	down, err := m.createSQL(ctx, name+".down.sql")
	if err != nil {
		return nil, err
	}

	return []*MigrationFile{up, down}, nil
}

func (m *Migrator) createSQL(ctx context.Context, fname string) (*MigrationFile, error) {
	fpath := filepath.Join(m.migrations.getDirectory(), fname)

	if err := ioutil.WriteFile(fpath, []byte(sqlTemplate), 0o644); err != nil {
		return nil, err
	}

	mf := &MigrationFile{
		Name:    fname,
		Path:    fpath,
		Content: goTemplate,
	}
	return mf, nil
}

var nameRE = regexp.MustCompile(`^[0-9a-z_\-]+$`)

func (m *Migrator) genMigrationName(name string) (string, error) {
	const timeFormat = "20060102150405"

	if name == "" {
		return "", errors.New("chmigrate: migration name can't be empty")
	}
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("chmigrate: invalid migration name: %q", name)
	}

	version := time.Now().UTC().Format(timeFormat)
	return fmt.Sprintf("%s_%s", version, name), nil
}

// MarkApplied marks the migration as applied (completed).
func (m *Migrator) MarkApplied(ctx context.Context, migration *Migration) error {
	migration.Sign = 1
	migration.MigratedAt = time.Now()
	_, err := m.db.NewInsert().
		Model(migration).
		ModelTableExpr(m.table).
		Exec(ctx)
	return err
}

// MarkUnapplied marks the migration as unapplied (new).
func (m *Migrator) MarkUnapplied(ctx context.Context, migration *Migration) error {
	migration.Sign = -1
	_, err := m.db.NewInsert().
		Model(migration).
		ModelTableExpr(m.table).
		Exec(ctx)
	return err
}

// selectAppliedMigrations selects applied (applied) migrations in descending order.
func (m *Migrator) selectAppliedMigrations(ctx context.Context) (MigrationSlice, error) {
	var ms MigrationSlice
	if err := m.db.NewSelect().
		ColumnExpr("*").
		Model(&ms).
		ModelTableExpr(m.table).
		Final().
		Scan(ctx); err != nil {
		return nil, err
	}
	return ms, nil
}

func (m *Migrator) formattedTableName(db *ch.DB) string {
	return db.Formatter().FormatQuery(m.table)
}

func (m *Migrator) validate() error {
	if len(m.ms) == 0 {
		return errors.New("chmigrate: there are no any migrations")
	}
	return nil
}

//------------------------------------------------------------------------------

type migrationLock struct {
	A int8
}

func (m *Migrator) Lock(ctx context.Context) error {
	if _, err := m.db.ExecContext(
		ctx,
		"ALTER TABLE ? ADD COLUMN ? Int8",
		ch.Safe(m.locksTable), ch.Safe("lock"),
	); err != nil {
		return fmt.Errorf("chmigrate: migrations table is already locked (%w)", err)
	}
	return nil
}

func (m *Migrator) Unlock(ctx context.Context) error {
	if _, err := m.db.ExecContext(
		ctx,
		"ALTER TABLE ? DROP COLUMN ?",
		ch.Safe(m.locksTable), ch.Safe("lock"),
	); err != nil && !strings.Contains(err.Error(), "Cannot find column") {
		return fmt.Errorf("chmigrate: migrations table is already unlocked (%w)", err)
	}
	return nil
}

func migrationMap(ms MigrationSlice) map[string]*Migration {
	mp := make(map[string]*Migration)
	for i := range ms {
		m := &ms[i]
		mp[m.Name] = m
	}
	return mp
}
