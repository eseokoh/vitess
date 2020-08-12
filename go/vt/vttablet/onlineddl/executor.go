/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package onlineddl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/timer"
	"vitess.io/vitess/go/vt/dbconnpool"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/schema"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/connpool"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"

	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

var (
	// ErrExecutorNotWritableTablet  is generated when executor is asked to run gh-ost on a read-only server
	ErrExecutorNotWritableTablet = errors.New("Cannot run gh-ost migration on non-writable tablet")
	// ErrExecutorMigrationAlreadyRunning is generated when an attempt is made to run an operation that conflicts with a running migration
	ErrExecutorMigrationAlreadyRunning = errors.New("Cannot run gh-ost migration since a migration is already running")
)

const (
	maxPasswordLength = 32 // MySQL's *replication* password may not exceed 32 characters
)

var (
	onlineDDLUser  = "vt-online-ddl-internal"
	onlineDDLGrant = fmt.Sprintf("'%s'@'%s'", onlineDDLUser, "%")
)

// Executor wraps and manages the execution of a gh-ost migration.
type Executor struct {
	env            tabletenv.Env
	pool           *connpool.Pool
	tabletTypeFunc func() topodatapb.TabletType
	ts             *topo.Server

	keyspace string
	shard    string
	dbName   string

	initMutex        sync.Mutex
	migrationMutex   sync.Mutex
	migrationRunning int64

	ticks  *timer.Timer
	isOpen bool
}

var (
	migrationCheckInterval = time.Second * 10
)

// GhostBinaryFileName returns the full path+name of the gh-ost binary
func GhostBinaryFileName() string {
	return path.Join(os.TempDir(), "vt-gh-ost")
}

// PTOSCFileName returns the full path+name of the pt-online-schema-change binary
func PTOSCFileName() string {
	return path.Join(os.TempDir(), "vt-pt-online-schema-change")
}

// NewExecutor creates a new gh-ost executor.
func NewExecutor(env tabletenv.Env, ts *topo.Server, tabletTypeFunc func() topodatapb.TabletType) *Executor {
	return &Executor{
		env: env,

		pool: connpool.NewPool(env, "ExecutorPool", tabletenv.ConnPoolConfig{
			Size:               1,
			IdleTimeoutSeconds: env.Config().OltpReadPool.IdleTimeoutSeconds,
		}),
		tabletTypeFunc: tabletTypeFunc,
		ts:             ts,
		ticks:          timer.NewTimer(migrationCheckInterval),
	}
}

func (e *Executor) execQuery(ctx context.Context, query string) (result *sqltypes.Result, err error) {
	defer e.env.LogError()

	conn, err := e.pool.Get(ctx)
	if err != nil {
		return result, err
	}
	defer conn.Recycle()
	return withDDL.Exec(ctx, query, conn.Exec)
}

func (e *Executor) initSchema(ctx context.Context) error {
	_, err := e.execQuery(ctx, sqlValidationQuery)
	return err
}

// InitDBConfig initializes keysapce
func (e *Executor) InitDBConfig(keyspace, shard, dbName string) {
	e.keyspace = keyspace
	e.shard = shard
	e.dbName = dbName
}

// Open opens database pool and initializes the schema
func (e *Executor) Open() error {
	e.initMutex.Lock()
	defer e.initMutex.Unlock()
	if e.isOpen {
		return nil
	}
	e.pool.Open(e.env.Config().DB.AppWithDB(), e.env.Config().DB.DbaWithDB(), e.env.Config().DB.AppDebugWithDB())
	e.ticks.Start(e.onMigrationCheckTick)
	e.isOpen = true

	return nil
}

// Close frees resources
func (e *Executor) Close() {
	e.initMutex.Lock()
	defer e.initMutex.Unlock()
	if !e.isOpen {
		return
	}

	e.ticks.Stop()
	e.pool.Close()
	e.isOpen = false
}

func (e *Executor) ghostPanicFlagFileName(onlineDDL *schema.OnlineDDL) string {
	return fmt.Sprintf("/tmp/ghost.%s.panic.flag", onlineDDL.UUID)
}

// readMySQLVariables contacts the backend MySQL server to read some of its configuration
func (e *Executor) readMySQLVariables(ctx context.Context) (host string, port int, readOnly bool, err error) {
	conn, err := e.pool.Get(ctx)
	if err != nil {
		return host, port, readOnly, err
	}
	defer conn.Recycle()

	tm, err := conn.Exec(ctx, "select @@global.hostname as hostname, @@global.port as port, @@global.read_only as read_only from dual", 1, true)
	if err != nil {
		return host, port, readOnly, vterrors.Errorf(vtrpcpb.Code_UNKNOWN, "could not read MySQL variables: %v", err)
	}
	row := tm.Named().Row()
	if row == nil {
		return host, port, readOnly, vterrors.Errorf(vtrpcpb.Code_UNKNOWN, "unexpected result for MySQL variables: %+v", tm.Rows)
	}
	host = row["hostname"].ToString()
	if p, err := row.ToInt64("port"); err != nil {
		return host, port, readOnly, vterrors.Errorf(vtrpcpb.Code_UNKNOWN, "could not parse @@global.port %v: %v", tm, err)
	} else {
		port = int(p)
	}
	if readOnly, err = row.ToBool("read_only"); err != nil {
		return host, port, readOnly, vterrors.Errorf(vtrpcpb.Code_UNKNOWN, "could not parse @@global.read_only %v: %v", tm, err)
	}
	return host, port, readOnly, nil
}

// createOnlineDDLUser creates a gh-ost user account with all neccessary privileges and with a random password
func (e *Executor) createOnlineDDLUser(ctx context.Context) (password string, err error) {
	conn, err := dbconnpool.NewDBConnection(ctx, e.env.Config().DB.DbaConnector())
	if err != nil {
		return password, err
	}
	defer conn.Close()

	password = RandomHash()[0:maxPasswordLength]

	for _, query := range sqlCreateOnlineDDLUser {
		parsed := sqlparser.BuildParsedQuery(query, onlineDDLGrant, password)
		if _, err := conn.ExecuteFetch(parsed.Query, 0, false); err != nil {
			return password, err
		}
	}
	for _, query := range sqlGrantOnlineDDLUser {
		parsed := sqlparser.BuildParsedQuery(query, onlineDDLGrant)
		if _, err := conn.ExecuteFetch(parsed.Query, 0, false); err != nil {
			return password, err
		}
	}
	return password, err
}

// dropOnlineDDLUser drops the given ddl user account at the end of migration
func (e *Executor) dropOnlineDDLUser(ctx context.Context, user string) error {
	conn, err := dbconnpool.NewDBConnection(ctx, e.env.Config().DB.DbaConnector())
	if err != nil {
		return err
	}
	defer conn.Close()

	parsed := sqlparser.BuildParsedQuery(sqlDropOnlineDDLUser, user)
	_, err = conn.ExecuteFetch(parsed.Query, 0, false)
	return err
}

// ExecuteWithGhost validates and runs a gh-ost process.
// Validation included testing the backend MySQL server and the gh-ost binray itself
// Execution runs first a dry run, then an actual migration
func (e *Executor) ExecuteWithGhost(ctx context.Context, onlineDDL *schema.OnlineDDL) error {
	e.migrationMutex.Lock()
	defer e.migrationMutex.Unlock()

	if atomic.LoadInt64(&e.migrationRunning) > 0 {
		return ErrExecutorMigrationAlreadyRunning
	}

	if e.tabletTypeFunc() != topodatapb.TabletType_MASTER {
		return ErrExecutorNotWritableTablet
	}
	mysqlHost, mysqlPort, readOnly, err := e.readMySQLVariables(ctx)
	if err != nil {
		log.Errorf("Error before running gh-ost: %+v", err)
		return err
	}
	if readOnly {
		err := fmt.Errorf("Error before running gh-ost: MySQL server is read_only")
		log.Errorf(err.Error())
		return err
	}
	onlineDDLPassword, err := e.createOnlineDDLUser(ctx)
	if err != nil {
		err := fmt.Errorf("Error creating gh-ost user: %+v", err)
		log.Errorf(err.Error())
		return err
	}
	tempDir, err := createTempDir(onlineDDL.UUID)
	if err != nil {
		log.Errorf("Error creating temporary directory: %+v", err)
		return err
	}
	credentialsConfigFileContent := fmt.Sprintf(`[client]
user=%s
password=${ONLINE_DDL_PASSWORD}
`, onlineDDLUser)
	credentialsConfigFileName, err := createTempScript(tempDir, "gh-ost-conf.cfg", credentialsConfigFileContent)
	if err != nil {
		log.Errorf("Error creating config file: %+v", err)
		return err
	}
	wrapperScriptContent := fmt.Sprintf(`#!/bin/bash
ghost_log_path="%s"
ghost_log_file=gh-ost.log

mkdir -p "$ghost_log_path"

export ONLINE_DDL_PASSWORD
%s "$@" > "$ghost_log_path/$ghost_log_file" 2>&1
	`, tempDir, GhostBinaryFileName(),
	)
	wrapperScriptFileName, err := createTempScript(tempDir, "gh-ost-wrapper.sh", wrapperScriptContent)
	if err != nil {
		log.Errorf("Error creating wrapper script: %+v", err)
		return err
	}
	onHookContent := func(status schema.OnlineDDLStatus) string {
		return fmt.Sprintf(`#!/bin/bash
curl -s 'http://localhost:%d/schema-migration/report-status?uuid=%s&status=%s&dryrun='"$GH_OST_DRY_RUN"
		`, *servenv.Port, onlineDDL.UUID, string(status))
	}
	if _, err := createTempScript(tempDir, "gh-ost-on-startup", onHookContent(schema.OnlineDDLStatusRunning)); err != nil {
		log.Errorf("Error creating script: %+v", err)
		return err
	}
	if _, err := createTempScript(tempDir, "gh-ost-on-status", onHookContent(schema.OnlineDDLStatusRunning)); err != nil {
		log.Errorf("Error creating script: %+v", err)
		return err
	}
	if _, err := createTempScript(tempDir, "gh-ost-on-success", onHookContent(schema.OnlineDDLStatusComplete)); err != nil {
		log.Errorf("Error creating script: %+v", err)
		return err
	}
	if _, err := createTempScript(tempDir, "gh-ost-on-failure", onHookContent(schema.OnlineDDLStatusFailed)); err != nil {
		log.Errorf("Error creating script: %+v", err)
		return err
	}
	// Validate gh-ost binary:
	log.Infof("Will now validate gh-ost binary")
	_, err = execCmd(
		"bash",
		[]string{
			wrapperScriptFileName,
			"--version",
		},
		os.Environ(),
		"/tmp",
		nil,
		nil,
	)
	if err != nil {
		log.Errorf("Error testing gh-ost binary: %+v", err)
		return err
	}
	log.Infof("+ OK")

	runGhost := func(execute bool) error {
		// Temporary hack (2020-08-11)
		// Because sqlparser does not do full blown ALTER TABLE parsing,
		// and because we don't want gh-ost to know about WITH_GHOST and WITH_PT syntax,
		// we resort to regexp-based parsing of the query.
		// TODO(shlomi): generate _alter options_ via sqlparser when it full supports ALTER TABLE syntax.
		_, _, alterOptions := schema.ParseAlterTableOptions(onlineDDL.SQL)

		os.Setenv("ONLINE_DDL_PASSWORD", onlineDDLPassword)
		_, err := execCmd(
			"bash",
			[]string{
				wrapperScriptFileName,
				fmt.Sprintf(`--host=%s`, mysqlHost),
				fmt.Sprintf(`--port=%d`, mysqlPort),
				fmt.Sprintf(`--conf=%s`, credentialsConfigFileName), // user & password found here
				`--allow-on-master`,
				`--max-load=Threads_running=100`,
				`--critical-load=Threads_running=200`,
				`--critical-load-hibernate-seconds=60`,
				`--approve-renamed-columns`,
				`--debug`,
				`--exact-rowcount`,
				`--timestamp-old-table`,
				`--initially-drop-ghost-table`,
				`--default-retries=120`,
				fmt.Sprintf("--hooks-path=%s", tempDir),
				fmt.Sprintf(`--hooks-hint=%s`, onlineDDL.UUID),
				fmt.Sprintf(`--database=%s`, e.dbName),
				fmt.Sprintf(`--table=%s`, onlineDDL.Table),
				fmt.Sprintf(`--alter=%s`, alterOptions),
				fmt.Sprintf(`--panic-flag-file=%s`, e.ghostPanicFlagFileName(onlineDDL)),
				fmt.Sprintf(`--execute=%t`, execute),
			},
			os.Environ(),
			"/tmp",
			nil,
			nil,
		)
		return err
	}

	atomic.StoreInt64(&e.migrationRunning, 1)
	go func() error {
		defer atomic.StoreInt64(&e.migrationRunning, 0)
		defer e.dropOnlineDDLUser(ctx, onlineDDLGrant)

		log.Infof("Will now dry-run gh-ost on: %s:%d", mysqlHost, mysqlPort)
		if err := runGhost(false); err != nil {
			// perhaps gh-ost was interrupted midway and didn't have the chance to send a "failes" status
			_ = e.updateMigrationStatus(ctx, onlineDDL.UUID, schema.OnlineDDLStatusFailed)
			log.Errorf("Error executing gh-ost dry run: %+v", err)
			return err
		}
		log.Infof("+ OK")

		log.Infof("Will now run gh-ost on: %s:%d", mysqlHost, mysqlPort)
		startedMigrations.Add(1)
		if err := runGhost(true); err != nil {
			// perhaps gh-ost was interrupted midway and didn't have the chance to send a "failes" status
			_ = e.updateMigrationStatus(ctx, onlineDDL.UUID, schema.OnlineDDLStatusFailed)
			failedMigrations.Add(1)
			log.Errorf("Error running gh-ost: %+v", err)
			return err
		}
		successfulMigrations.Add(1)
		log.Infof("+ OK")
		return nil
	}()
	return nil
}

// ExecuteWithPTOSC validates and runs a pt-online-schema-change process.
// Validation included testing the backend MySQL server and the pt-online-schema-change binary itself
// Execution runs first a dry run, then an actual migration
func (e *Executor) ExecuteWithPTOSC(ctx context.Context, onlineDDL *schema.OnlineDDL) error {
	e.migrationMutex.Lock()
	defer e.migrationMutex.Unlock()

	if atomic.LoadInt64(&e.migrationRunning) > 0 {
		return ErrExecutorMigrationAlreadyRunning
	}

	if e.tabletTypeFunc() != topodatapb.TabletType_MASTER {
		return ErrExecutorNotWritableTablet
	}
	mysqlHost, mysqlPort, readOnly, err := e.readMySQLVariables(ctx)
	if err != nil {
		log.Errorf("Error before running pt-online-schema-change: %+v", err)
		return err
	}
	if readOnly {
		err := fmt.Errorf("Error before running pt-online-schema-change: MySQL server is read_only")
		log.Errorf(err.Error())
		return err
	}
	onlineDDLPassword, err := e.createOnlineDDLUser(ctx)
	if err != nil {
		err := fmt.Errorf("Error creating pt-online-schema-change user: %+v", err)
		log.Errorf(err.Error())
		return err
	}
	tempDir, err := createTempDir(onlineDDL.UUID)
	if err != nil {
		log.Errorf("Error creating temporary directory: %+v", err)
		return err
	}
	credentialsConfigFileContent := fmt.Sprintf(`[client]
user=%s
password=%s
`, onlineDDLUser, onlineDDLPassword)
	credentialsConfigFileName, err := createTempScript(tempDir, "pt-online-schema-change-conf.cfg", credentialsConfigFileContent)
	if err != nil {
		log.Errorf("Error creating config file: %+v", err)
		return err
	}
	wrapperScriptContent := fmt.Sprintf(`#!/bin/bash
pt_log_path="%s"
pt_log_file=pt-online-schema-change.log

mkdir -p "$pt_log_path"

export ONLINE_DDL_PASSWORD
echo "running this" %s "$@" > /tmp/t.txt
%s "$@" > "$pt_log_path/$pt_log_file" 2>&1
	`, tempDir, PTOSCFileName(), PTOSCFileName(),
	)
	wrapperScriptFileName, err := createTempScript(tempDir, "pt-online-schema-change-wrapper.sh", wrapperScriptContent)
	if err != nil {
		log.Errorf("Error creating wrapper script: %+v", err)
		return err
	}
	pluginCode := `
	package pt_online_schema_change_plugin;

	use strict;
	use LWP::Simple;

	sub new {
	  my($class, % args) = @_;
	  my $self = {
	    % args
	  };
	  return bless $self, $class;
	}

	sub init {
	  my($self, % args) = @_;
	}

	sub before_create_new_table {
	  my($self, % args) = @_;
	  get("http://localhost:{{VTTABLET_PORT}}/schema-migration/report-status?uuid={{MIGRATION_UUID}}&status={{OnlineDDLStatusRunning}}&dryrun={{DRYRUN}}");
	}

	sub before_exit {
	  my($self, % args) = @_;
	  my $exit_status = $args {
	    exit_status
	  };
	  if ($exit_status == 0) {
	    get("http://localhost:{{VTTABLET_PORT}}/schema-migration/report-status?uuid={{MIGRATION_UUID}}&status={{OnlineDDLStatusComplete}}&dryrun={{DRYRUN}}");
	  } else {
	    get("http://localhost:{{VTTABLET_PORT}}/schema-migration/report-status?uuid={{MIGRATION_UUID}}&status={{OnlineDDLStatusFailed}}&dryrun={{DRYRUN}}");
	  }
	}

	1;
	`
	pluginCode = strings.ReplaceAll(pluginCode, "{{VTTABLET_PORT}}", fmt.Sprintf("%d", *servenv.Port))
	pluginCode = strings.ReplaceAll(pluginCode, "{{MIGRATION_UUID}}", onlineDDL.UUID)
	pluginCode = strings.ReplaceAll(pluginCode, "{{OnlineDDLStatusRunning}}", string(schema.OnlineDDLStatusRunning))
	pluginCode = strings.ReplaceAll(pluginCode, "{{OnlineDDLStatusComplete}}", string(schema.OnlineDDLStatusComplete))
	pluginCode = strings.ReplaceAll(pluginCode, "{{OnlineDDLStatusFailed}}", string(schema.OnlineDDLStatusFailed))

	// Validate pt-online-schema-change binary:
	log.Infof("Will now validate pt-online-schema-change binary")
	_, err = execCmd(
		"bash",
		[]string{
			wrapperScriptFileName,
			"--version",
		},
		os.Environ(),
		"/tmp",
		nil,
		nil,
	)
	if err != nil {
		log.Errorf("Error testing pt-online-schema-change binary: %+v", err)
		return err
	}
	log.Infof("+ OK")

	// Temporary hack (2020-08-11)
	// Because sqlparser does not do full blown ALTER TABLE parsing,
	// and because pt-online-schema-change requires only the table options part of the ALTER TABLE statement,
	// we resort to regexp-based parsing of the query.
	// TODO(shlomi): generate _alter options_ via sqlparser when it full supports ALTER TABLE syntax.
	_, _, alterOptions := schema.ParseAlterTableOptions(onlineDDL.SQL)

	runPTOSC := func(execute bool) error {
		os.Setenv("ONLINE_DDL_PASSWORD", onlineDDLPassword)
		executeFlag := "--dry-run"
		if execute {
			executeFlag = "--execute"
		}
		finalPluginCode := strings.ReplaceAll(pluginCode, "{{DRYRUN}}", fmt.Sprintf("%t", !execute))
		pluginFile, err := createTempScript(tempDir, "pt-online-schema-change-plugin", finalPluginCode)
		if err != nil {
			log.Errorf("Error creating script: %+v", err)
			return err
		}
		_, err = execCmd(
			"bash",
			[]string{
				wrapperScriptFileName,
				`--plugin`,
				pluginFile,
				`--alter`,
				alterOptions,
				executeFlag,
				fmt.Sprintf(`h=%s,P=%d,D=%s,t=%s,F=%s`, mysqlHost, mysqlPort, e.dbName, onlineDDL.Table, credentialsConfigFileName),
			},
			os.Environ(),
			"/tmp",
			nil,
			nil,
		)
		return err
	}

	atomic.StoreInt64(&e.migrationRunning, 1)
	go func() error {
		defer atomic.StoreInt64(&e.migrationRunning, 0)
		defer e.dropOnlineDDLUser(ctx, onlineDDLGrant)

		log.Infof("Will now dry-run pt-online-schema-change on: %s:%d", mysqlHost, mysqlPort)
		if err := runPTOSC(false); err != nil {
			// perhaps pt-osc was interrupted midway and didn't have the chance to send a "failes" status
			_ = e.updateMigrationStatus(ctx, onlineDDL.UUID, schema.OnlineDDLStatusFailed)
			log.Errorf("Error executing pt-online-schema-change dry run: %+v", err)
			return err
		}
		log.Infof("+ OK")

		log.Infof("Will now run pt-online-schema-change on: %s:%d", mysqlHost, mysqlPort)
		startedMigrations.Add(1)
		if err := runPTOSC(true); err != nil {
			// perhaps pt-osc was interrupted midway and didn't have the chance to send a "failes" status
			_ = e.updateMigrationStatus(ctx, onlineDDL.UUID, schema.OnlineDDLStatusFailed)
			failedMigrations.Add(1)
			log.Errorf("Error running pt-online-schema-change: %+v", err)
			return err
		}
		successfulMigrations.Add(1)
		log.Infof("+ OK")
		return nil
	}()
	return nil
}

// Cancel attempts to abort a running migration by touching the panic flag file
func (e *Executor) Cancel(onlineDDL *schema.OnlineDDL) error {
	file, err := os.OpenFile(e.ghostPanicFlagFileName(onlineDDL), os.O_RDONLY|os.O_CREATE, 0666)
	if file != nil {
		defer file.Close()
	}
	return err
}

func (e *Executor) writeMigrationJob(ctx context.Context, onlineDDL *schema.OnlineDDL) error {
	parsed := sqlparser.BuildParsedQuery(sqlInsertSchemaMigration, "_vt",
		":migration_uuid",
		":keyspace",
		":shard",
		":mysql_table",
		":migration_statement",
		":strategy",
		":requested_timestamp",
		":migration_status",
	)
	bindVars := map[string]*querypb.BindVariable{
		"migration_uuid":      sqltypes.StringBindVariable(onlineDDL.UUID),
		"keyspace":            sqltypes.StringBindVariable(onlineDDL.Keyspace),
		"shard":               sqltypes.StringBindVariable(e.shard),
		"mysql_table":         sqltypes.StringBindVariable(onlineDDL.Table),
		"migration_statement": sqltypes.StringBindVariable(onlineDDL.SQL),
		"strategy":            sqltypes.StringBindVariable(string(onlineDDL.Strategy)),
		"requested_timestamp": sqltypes.Int64BindVariable(onlineDDL.RequestTimeSeconds()),
		"migration_status":    sqltypes.StringBindVariable(string(onlineDDL.Status)),
	}

	bound, err := parsed.GenerateQuery(bindVars, nil)
	if err != nil {
		return err
	}
	_, err = e.execQuery(ctx, bound)
	if err != nil {
		return err
	}

	return nil
}

// reviewMigrationJobs reads Topo's listing of migrations for this keyspace/shard,
// and persists them in _vt.schema_migrations. Some of those jobs may be new, some
// perhaps already known, it doesn't matter.
func (e *Executor) reviewMigrationJobs(ctx context.Context) error {
	if atomic.LoadInt64(&e.migrationRunning) > 0 {
		// Just to save some cycles here. If there's a running migration, skip reading global topo:
		// even if global topo has new jobs for us, we wouldn't be able to run them, anyway.
		return nil
	}

	conn, err := e.ts.ConnForCell(ctx, topo.GlobalCell)
	if err != nil {
		log.Errorf("Executor.reviewMigrationRequests ConnForCell error: %s", err.Error())
		return err
	}

	dirPath := schema.MigrationJobsKeyspaceShardPath(e.keyspace, e.shard)
	entries, err := conn.ListDir(ctx, dirPath, false)
	if err != nil {
		log.Errorf("Executor.reviewMigrationRequests listDir error: %s", err.Error())
		return err
	}
	for _, entry := range entries {
		entryPath := fmt.Sprintf("%s/%s", dirPath, entry.Name)
		onlineDDL, err := schema.ReadTopo(ctx, conn, entryPath)
		if err != nil {
			log.Errorf("reviewMigrationRequests.ReadTopo error: %+v", err)
			continue
		}
		if err := e.writeMigrationJob(ctx, onlineDDL); err != nil {
			log.Errorf("reviewMigrationRequests.writeMigrationJob error: %+v", err)
			continue
		}
		log.Infof("Found schema migration job: %+v", onlineDDL)
	}
	return nil
}

// scheduleNextMigration attemps to schedule a single migration to run next.
// possibly there's no migrations to run. Possibly there's a migration running right now,
// in which cases nothing happens.
func (e *Executor) scheduleNextMigration(ctx context.Context) error {
	e.migrationMutex.Lock()
	defer e.migrationMutex.Unlock()

	if atomic.LoadInt64(&e.migrationRunning) > 0 {
		return ErrExecutorMigrationAlreadyRunning
	}

	{
		parsed := sqlparser.BuildParsedQuery(sqlSelectCountReadyMigrations, "_vt")
		r, err := e.execQuery(ctx, parsed.Query)
		if err != nil {
			return err
		}
		row := r.Named().Row()
		countReady, err := row.ToInt64("count_ready")
		if err != nil {
			return err
		}
		if countReady > 0 {
			// seems like there's already one migration that's good to go
			return nil
		}
	} // Cool, seems like no migration is ready. Let's try and make a single 'queued' migration 'ready'

	parsed := sqlparser.BuildParsedQuery(sqlScheduleSingleMigration, "_vt")
	_, err := e.execQuery(ctx, parsed.Query)

	return err
}

func (e *Executor) runNextMigration(ctx context.Context) error {
	e.migrationMutex.Lock()
	defer e.migrationMutex.Unlock()

	if atomic.LoadInt64(&e.migrationRunning) > 0 {
		return ErrExecutorMigrationAlreadyRunning
	}

	parsed := sqlparser.BuildParsedQuery(sqlSelectReadyMigration, "_vt")
	r, err := e.execQuery(ctx, parsed.Query)
	if err != nil {
		return err
	}
	named := r.Named()
	for i, row := range named.Rows {
		onlineDDL := &schema.OnlineDDL{
			Keyspace: row["keyspace"].ToString(),
			Table:    row["mysql_table"].ToString(),
			SQL:      row["migration_statement"].ToString(),
			UUID:     row["migration_uuid"].ToString(),
			Strategy: sqlparser.DDLStrategy(row["strategy"].ToString()),
			Status:   schema.OnlineDDLStatus(row["migration_status"].ToString()),
		}
		switch onlineDDL.Strategy {
		case schema.DDLStrategyGhost:
			go func() {
				if err := e.ExecuteWithGhost(ctx, onlineDDL); err != nil {
					_ = e.updateMigrationStatus(ctx, onlineDDL.UUID, schema.OnlineDDLStatusFailed)
				}
			}()
		case schema.DDLStrategyPTOSC:
			go func() {
				if err := e.ExecuteWithPTOSC(ctx, onlineDDL); err != nil {
					_ = e.updateMigrationStatus(ctx, onlineDDL.UUID, schema.OnlineDDLStatusFailed)
				}
			}()
		default:
			{
				_ = e.updateMigrationStatus(ctx, onlineDDL.UUID, schema.OnlineDDLStatusFailed)
				return fmt.Errorf("Unsupported strategy: %+v", onlineDDL.Strategy)
			}
		}
		// the query should only ever return a single row at the most
		// but let's make it also explicit here that we only run a single migration
		if i == 0 {
			break
		}
	}

	return nil
}

func (e *Executor) onMigrationCheckTick() {
	if e.tabletTypeFunc() != topodatapb.TabletType_MASTER {
		return
	}
	if e.keyspace == "" {
		log.Errorf("Executor.onMigrationCheckTick(): empty keyspace")
		return
	}
	ctx := context.Background()
	e.initSchema(ctx)

	e.reviewMigrationJobs(ctx)
	e.scheduleNextMigration(ctx)
	e.runNextMigration(ctx)
}

func (e *Executor) updateMigrationStartedTimestamp(ctx context.Context, uuid string) error {
	parsed := sqlparser.BuildParsedQuery(sqlUpdateMigrationStartedTimestamp, "_vt",
		":migration_uuid",
	)
	bindVars := map[string]*querypb.BindVariable{
		"migration_uuid": sqltypes.StringBindVariable(uuid),
	}
	bound, err := parsed.GenerateQuery(bindVars, nil)
	if err != nil {
		return err
	}
	_, err = e.execQuery(ctx, bound)
	return err
}

func (e *Executor) updateMigrationTimestamp(ctx context.Context, timestampColumn string, uuid string) error {
	parsed := sqlparser.BuildParsedQuery(sqlUpdateMigrationTimestamp, "_vt", timestampColumn,
		":migration_uuid",
	)
	bindVars := map[string]*querypb.BindVariable{
		"migration_uuid": sqltypes.StringBindVariable(uuid),
	}
	bound, err := parsed.GenerateQuery(bindVars, nil)
	if err != nil {
		return err
	}
	_, err = e.execQuery(ctx, bound)
	return err
}

func (e *Executor) updateMigrationStatus(ctx context.Context, uuid string, status schema.OnlineDDLStatus) error {
	parsed := sqlparser.BuildParsedQuery(sqlUpdateMigrationStatus, "_vt",
		":migration_status",
		":migration_uuid",
	)
	bindVars := map[string]*querypb.BindVariable{
		"migration_status": sqltypes.StringBindVariable(string(status)),
		"migration_uuid":   sqltypes.StringBindVariable(uuid),
	}
	bound, err := parsed.GenerateQuery(bindVars, nil)
	if err != nil {
		return err
	}
	_, err = e.execQuery(ctx, bound)
	return err
}

// OnSchemaMigrationStatus is called by TabletServer's API, which is invoked by a running gh-ost migration's hooks.
func (e *Executor) OnSchemaMigrationStatus(ctx context.Context, uuidParam, statusParam, dryrunParam string) (err error) {
	status := schema.OnlineDDLStatus(statusParam)
	dryRun := (dryrunParam == "true")

	if dryRun && status != schema.OnlineDDLStatusFailed {
		// We don't consider dry-run reports unless there's a failure
		return nil
	}
	switch status {
	case schema.OnlineDDLStatusReady:
		{
			err = e.updateMigrationTimestamp(ctx, "ready_timestamp", uuidParam)
		}
	case schema.OnlineDDLStatusRunning:
		{
			_ = e.updateMigrationStartedTimestamp(ctx, uuidParam)
			err = e.updateMigrationTimestamp(ctx, "liveness_timestamp", uuidParam)
		}
	case schema.OnlineDDLStatusComplete:
		{
			_ = e.updateMigrationStartedTimestamp(ctx, uuidParam)
			err = e.updateMigrationTimestamp(ctx, "completed_timestamp", uuidParam)
		}
	case schema.OnlineDDLStatusFailed:
		{
			_ = e.updateMigrationStartedTimestamp(ctx, uuidParam)
			err = e.updateMigrationTimestamp(ctx, "completed_timestamp", uuidParam)
		}
	}
	if err != nil {
		return err
	}
	if err = e.updateMigrationStatus(ctx, uuidParam, status); err != nil {
		return err
	}

	return nil
}
