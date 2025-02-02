package pitr

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/WangXiangUSTC/tidb-lite"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/parser"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/model"
	"go.uber.org/zap"
)

const (
	colsSQL = `
SELECT column_name, extra FROM information_schema.columns
WHERE table_schema = ? AND table_name = ?;`
	uniqKeysSQL = `
SELECT non_unique, index_name, seq_in_index, column_name 
FROM information_schema.statistics
WHERE table_schema = ? AND table_name = ?
ORDER BY seq_in_index ASC;`
	alldatabases   = `SHOW DATABASES;`
	alltables      = `SHOW TABLES;`
	createMapDB    = `CREATE DATABASE _interval_map_;`
	createMapTable = `USE _interval_map_; create table _inter_map_ (curKey varchar(128) unique, srcKey varchar(128));`
	useIntervalDB  = `USE _interval_map_;`
)

var (
	// ErrTableNotExist means the table not exist.
	ErrTableNotExist = errors.New("table not exist")

	// used for run a mock tidb
	defaultTiDBDir  = "/tmp/pitr_tidb"
	defaultTiDBPort = 40404
)

// DDLHandle used to handle ddl, and privide the table info
type DDLHandle struct {
	db *sql.DB

	tableInfos sync.Map

	tidbServer *tidblite.TiDBServer

	historyDDLs []*model.Job

	lastDBInfoMap map[string]*model.DBInfo

	// whether try to accelerate ddl history process.
	accelerateEnable bool
}

func NewDDLHandle() (*DDLHandle, error) {
	// run a mock tidb in local, used to execute ddl and get table info
	if err := os.Mkdir(defaultTiDBDir, os.ModePerm); err != nil {
		return nil, err
	}
	tidbServer, err := tidblite.NewTiDBServer(tidblite.NewOptions(defaultTiDBDir).WithPort(defaultTiDBPort))
	if err != nil {
		return nil, err
	}

	var dbConn *sql.DB
	for i := 0; i < 5; i++ {
		dbConn, err = tidbServer.CreateConn()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}

	ddlHandle := &DDLHandle{
		db:               dbConn,
		tidbServer:       tidbServer,
		accelerateEnable: true,
		lastDBInfoMap:    make(map[string]*model.DBInfo),
	}

	return ddlHandle, nil
}

func (d *DDLHandle) ExecuteHistoryDDLs(historyDDLs []*model.Job) error {
	for _, ddl := range historyDDLs {
		if skipJob(ddl) {
			continue
		}

		schemaName := ""
		if ddl.BinlogInfo != nil && ddl.BinlogInfo.DBInfo != nil {
			schemaName = ddl.BinlogInfo.DBInfo.Name.O
		}
		if d.accelerateEnable {
			if err := d.AccelerateHistoryDDLs(ddl); err != nil {
				return errors.Trace(err)
			}
		} else {
			if err := d.ExecuteDDL(schemaName, ddl.Query); err != nil {
				return errors.Trace(err)
			}
		}
	}

	return nil
}

/*
 * Scan the ddl history job slice, record the last state & tableInfo for every tableInfo.
 * Example:
 * create table A  -> statePublic tableInfo A:(a int, b int)
 * create table B  -> statePublic tableInfo B:(a char(1))
 * A add column c  -> statePublic tableInfo A:(a int, b int, c char(10))
 * A drop column a -> statePublic tableInfo A:(b int, c char(10))
 * drop table B    -> stateNone tableInfo B:(a char(1))
 *
 * Every ddl job will record the final state and tableInfo after executed.
 */
func (d *DDLHandle) AccelerateHistoryDDLs(job *model.Job) error {
	switch job.Type {
	case model.ActionCreateSchema, model.ActionModifySchemaCharsetAndCollate, model.ActionDropSchema:
		if job.BinlogInfo.DBInfo.State == model.StatePublic {
			// Take if not exists into consideration, we will override there.
			d.lastDBInfoMap[quoteDB(job.BinlogInfo.DBInfo.Name.L)] = job.BinlogInfo.DBInfo
		}
		if job.BinlogInfo.DBInfo.State == model.StateNone {
			delete(d.lastDBInfoMap, quoteDB(job.BinlogInfo.DBInfo.Name.L))
		}
		return nil
	case model.ActionCreateTable, model.ActionCreateView, model.ActionDropTable, model.ActionDropView,
		model.ActionDropTablePartition, model.ActionTruncateTablePartition, model.ActionAddColumn,
		model.ActionDropColumn, model.ActionModifyColumn, model.ActionSetDefaultValue, model.ActionAddIndex,
		model.ActionDropIndex, model.ActionRenameIndex, model.ActionAddForeignKey, model.ActionDropForeignKey,
		model.ActionTruncateTable, model.ActionRebaseAutoID, model.ActionRenameTable, model.ActionShardRowID,
		model.ActionModifyTableComment, model.ActionAddTablePartition, model.ActionModifyTableCharsetAndCollate,
		model.ActionRecoverTable:
		if job.BinlogInfo.TableInfo.State == model.StatePublic {
			v, ok := d.lastDBInfoMap[quoteDB(strings.ToLower(job.SchemaName))]
			if !ok {
				return errors.New(fmt.Sprintf("database %s haven't exist in ddl history before use it", job.SchemaName))
			}
			// substitute the latest tableInfo for the old one in lastDBInfoMap.
			newTableInfo := job.BinlogInfo.TableInfo
			for i, t := range v.Tables {
				if t.ID == newTableInfo.ID {
					v.Tables[i] = newTableInfo
					return nil
				}
			}
			// the tableInfo maybe just created, add it in lastDBInfoMap.
			v.Tables = append(v.Tables, newTableInfo)
		} else if job.BinlogInfo.TableInfo.State == model.StateNone {
			// stateNone means the table has been dropped, remove it in lastDBInfoMap.
			v, ok := d.lastDBInfoMap[quoteDB(strings.ToLower(job.SchemaName))]
			if !ok {
				return errors.New(fmt.Sprintf("database %s haven't exist in ddl history before use it", job.SchemaName))
			}
			stateNoneTableInfo := job.BinlogInfo.TableInfo
			for i, t := range v.Tables {
				if t.ID == stateNoneTableInfo.ID {
					v.Tables = append(v.Tables[:i], v.Tables[i+1:]...)
					return nil
				}
			}
			log.Warn("table haven't exist in ddl history before drop it", zap.String("table", stateNoneTableInfo.Name.L))
		} else {
			return errors.New(fmt.Sprintf("unknown table state %s", job.BinlogInfo.TableInfo.State.String()))
		}
	default:
		return errors.New(fmt.Sprintf("unknown ddl action type %s", job.Type.String()))
	}
	return nil
}

// ExecuteDDL executes ddl, and then update the table's info
func (d *DDLHandle) ExecuteDDL(schema string, ddl string) error {
	log.Info("execute ddl", zap.String("ddl", ddl))

	if len(ddl) == 0 {
		return nil
	}
	schemaInDDL, table, err := parserSchemaTableFromDDL(ddl)
	if err != nil {
		return errors.Trace(err)
	}

	if len(schema) == 0 {
		schema = schemaInDDL
	}

	if _, err := d.db.Exec(ddl); err != nil {
		if strings.Contains(err.Error(), "Unknown database") {
			err := d.ExecuteDDL(schema, fmt.Sprintf("create database if not exists `%s`", schema))
			if err != nil {
				return errors.Trace(err)
			}

			return d.ExecuteDDL(schema, ddl)
		} else if strings.Contains(err.Error(), "No database selected") {
			if len(schema) != 0 {
				return d.ExecuteDDL(schema, fmt.Sprintf("use %s; %s", schema, ddl))
			}
		} else if strings.Contains(err.Error(), "already exists") {
			return nil
		}

		return errors.Trace(err)
	}

	info, err := getTableInfo(d.db, schema, table)
	if err != nil {
		// ddl drop table
		if err == ErrTableNotExist {
			return nil
		}
		return errors.Trace(err)
	}
	d.tableInfos.Store(quoteSchema(schema, table), info)

	return nil
}

// GetTableInfo get table's info
func (d *DDLHandle) GetTableInfo(schema, table string) (*tableInfo, error) {
	v, ok := d.tableInfos.Load(quoteSchema(schema, table))
	if ok {
		info := v.(*tableInfo)
		return info, nil
	}
	log.Warn("table info not in memory, will get from local tidb")

	return getTableInfo(d.db, schema, table)
}

func (d *DDLHandle) getAllDatabaseNames() ([]string, error) {
	rows, err := d.db.Query(alldatabases)
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer rows.Close()

	var names []string

	for rows.Next() {
		var name string
		err = rows.Scan(&name)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if strings.EqualFold(name, "mysql") || strings.EqualFold(name, "INFORMATION_SCHEMA") || strings.EqualFold(name, "PERFORMANCE_SCHEMA") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

func (d *DDLHandle) ResetDB() error {
	names, err := d.getAllDatabaseNames()
	if err != nil {
		return err
	}
	var sql string
	for _, v := range names {
		sql = fmt.Sprintf("DROP DATABASE %s", v)
		err = d.ExecuteDDL(v, sql)
		if err != nil {
			return err
		}
	}

	sql = "CREATE DATABASE IF NOT EXISTS test"
	return d.ExecuteDDL("test", sql)
}

func (d *DDLHandle) Close() {
	d.tidbServer.Close()

	if err := os.RemoveAll(defaultTiDBDir); err != nil {
		log.Warn("remove temp dir", zap.String("dir", defaultTiDBDir), zap.Error(err))
	}
}

type tableInfo struct {
	schema string
	table  string

	columns    []string
	primaryKey *indexInfo
	// include primary key if have
	uniqueKeys []indexInfo
}

type indexInfo struct {
	name    string
	columns []string
}

// getTableInfo returns information like (non-generated) column names and
// unique keys about the specified table
func getTableInfo(db *sql.DB, schema string, table string) (info *tableInfo, err error) {
	info = &tableInfo{
		schema: schema,
		table:  table,
	}

	if info.columns, err = getColsOfTbl(db, schema, table); err != nil {
		return nil, errors.Trace(err)
	}

	if info.uniqueKeys, err = getUniqKeys(db, schema, table); err != nil {
		return nil, errors.Trace(err)
	}

	// put primary key at first place
	// and set primaryKey
	for i := 0; i < len(info.uniqueKeys); i++ {
		if info.uniqueKeys[i].name == "PRIMARY" {
			info.uniqueKeys[i], info.uniqueKeys[0] = info.uniqueKeys[0], info.uniqueKeys[i]
			info.primaryKey = &info.uniqueKeys[0]
			break
		}
	}

	return
}

// getColsOfTbl returns a slice of the names of all columns,
// generated columns are excluded.
// https://dev.mysql.com/doc/mysql-infoschema-excerpt/5.7/en/columns-table.html
func getColsOfTbl(db *sql.DB, schema, table string) ([]string, error) {
	rows, err := db.Query(colsSQL, schema, table)
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer rows.Close()

	cols := make([]string, 0, 1)
	for rows.Next() {
		var name, extra string
		err = rows.Scan(&name, &extra)
		if err != nil {
			return nil, errors.Trace(err)
		}
		isGenerated := strings.Contains(extra, "VIRTUAL GENERATED") || strings.Contains(extra, "STORED GENERATED")
		if isGenerated {
			continue
		}
		cols = append(cols, name)
	}

	if err = rows.Err(); err != nil {
		return nil, errors.Trace(err)
	}

	// if no any columns returns, means the table not exist.
	if len(cols) == 0 {
		return nil, ErrTableNotExist
	}

	return cols, nil
}

// https://dev.mysql.com/doc/mysql-infoschema-excerpt/5.7/en/statistics-table.html
func getUniqKeys(db *sql.DB, schema, table string) (uniqueKeys []indexInfo, err error) {
	rows, err := db.Query(uniqKeysSQL, schema, table)
	if err != nil {
		err = errors.Trace(err)
		return
	}
	defer rows.Close()

	var nonUnique int
	var keyName string
	var columnName string
	var seqInIndex int // start at 1

	// get pk and uk
	// key for PRIMARY or other index name
	for rows.Next() {
		err = rows.Scan(&nonUnique, &keyName, &seqInIndex, &columnName)
		if err != nil {
			err = errors.Trace(err)
			return
		}

		if nonUnique == 1 {
			continue
		}

		var i int
		// Search for indexInfo with the current keyName
		for i = 0; i < len(uniqueKeys); i++ {
			if uniqueKeys[i].name == keyName {
				uniqueKeys[i].columns = append(uniqueKeys[i].columns, columnName)
				break
			}
		}
		// If we don't find the indexInfo with the loop above, create a new one
		if i == len(uniqueKeys) {
			uniqueKeys = append(uniqueKeys, indexInfo{keyName, []string{columnName}})
		}
	}

	if err = rows.Err(); err != nil {
		return nil, errors.Trace(err)
	}

	return
}

// parserSchemaTableFromDDL parses ddl query to get schema and table
// ddl like `use test; create table`
func parserSchemaTableFromDDL(ddlQuery string) (schema, table string, err error) {
	stmts, _, err := parser.New().Parse(ddlQuery, "", "")
	if err != nil {
		return "", "", err
	}

	haveUseStmt := false

	for _, stmt := range stmts {
		switch node := stmt.(type) {
		case *ast.UseStmt:
			haveUseStmt = true
			schema = node.DBName
		case *ast.CreateDatabaseStmt:
			schema = node.Name
		case *ast.DropDatabaseStmt:
			schema = node.Name
		case *ast.TruncateTableStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.CreateIndexStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.CreateTableStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.DropIndexStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.AlterTableStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.DropTableStmt:
			// FIXME: may drop more than one table in a ddl
			if len(node.Tables[0].Schema.O) != 0 {
				schema = node.Tables[0].Schema.O
			}
			table = node.Tables[0].Name.O
		case *ast.RenameTableStmt:
			if len(node.NewTable.Schema.O) != 0 {
				schema = node.NewTable.Schema.O
			}
			table = node.NewTable.Name.O
		default:
			return "", "", errors.Errorf("unknown ddl type, ddl: %s", ddlQuery)
		}
	}

	if haveUseStmt {
		if len(stmts) != 2 {
			return "", "", errors.Errorf("invalid ddl %s", ddlQuery)
		}
	} else {
		if len(stmts) != 1 {
			return "", "", errors.Errorf("invalid ddl %s", ddlQuery)
		}
	}

	return
}

func (d *DDLHandle) getAllTableNames(schema string) ([]string, error) {
	udb := fmt.Sprintf("USE %s;", schema)
	rows, err := d.db.Query(udb + alltables)
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		err = rows.Scan(&name)
		if err != nil {
			return nil, errors.Trace(err)
		}
		names = append(names, name)
	}
	return names, nil
}

func (d *DDLHandle) createMapTable() error {
	err := d.ExecuteDDL("", createMapDB)
	if err != nil {
		return err
	}
	return d.ExecuteDDL("", createMapTable)
}

func (d *DDLHandle) fetchMapKeyFromDB(key string) (string, error) {
	sel := fmt.Sprintf(`SELECT srcKey FROM _inter_map_ WHERE curKey = '%s'`, key)
	rows, err := d.db.Query(useIntervalDB + sel)
	if err != nil {
		return "", errors.Trace(err)
	}
	defer rows.Close()

	var cKey string
	rows.Next()
	err = rows.Scan(&cKey)
	if err != nil {
		return "", nil
	}
	return cKey, nil
}

func (d *DDLHandle) insertMapKeyFromDB(newKey, oldKey string) error {
	s, err := d.fetchMapKeyFromDB(oldKey)
	if err != nil {
		return err
	}
	var ins string
	if s != "" {
		ins = fmt.Sprintf(`INSERT INTO _interval_map_._inter_map_ VALUES ('%s', '%s')`, newKey, s)

	} else {
		ins = fmt.Sprintf(`INSERT INTO _interval_map_._inter_map_ VALUES ('%s', '%s')`, newKey, oldKey)
	}
	_, err = d.db.Exec(ins)
	return err
}

// TiDB write DDL Binlog for every DDL Job, we must ignore jobs that are cancelled or rollback
// For older version TiDB, it write DDL Binlog in the txn that the state of job is changed to *synced*
// Now, it write DDL Binlog in the txn that the state of job is changed to *done* (before change to *synced*)
// At state *done*, it will be always and only changed to *synced*.
func skipJob(job *model.Job) bool {
	return !job.IsSynced() && !job.IsDone()
}

func (d *DDLHandle) ShiftMetaToTiDB() error {
	var DBInfos []*model.DBInfo
	for _, v := range d.lastDBInfoMap {
		DBInfos = append(DBInfos, v)
	}
	return d.tidbServer.SetDBInfoMetaAndReload(DBInfos)
}

func (d *DDLHandle) SetServerHistoryAccelerate(server *tidblite.TiDBServer, jobs []*model.Job, m map[string]*model.DBInfo, ac bool) {
	d.tidbServer = server
	d.historyDDLs = jobs
	d.lastDBInfoMap = m
	d.accelerateEnable = ac
}
