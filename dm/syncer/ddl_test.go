// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package syncer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/DATA-DOG/go-sqlmock"
	. "github.com/pingcap/check"
	"github.com/pingcap/tidb/parser"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/util/filter"
	"go.uber.org/zap"

	regexprrouter "github.com/pingcap/tidb/util/regexpr-router"
	router "github.com/pingcap/tidb/util/table-router"
	"github.com/pingcap/tiflow/dm/dm/config"
	"github.com/pingcap/tiflow/dm/pkg/conn"
	tcontext "github.com/pingcap/tiflow/dm/pkg/context"
	"github.com/pingcap/tiflow/dm/pkg/log"
	parserpkg "github.com/pingcap/tiflow/dm/pkg/parser"
	"github.com/pingcap/tiflow/dm/pkg/terror"
	"github.com/pingcap/tiflow/dm/pkg/utils"
	onlineddl "github.com/pingcap/tiflow/dm/syncer/online-ddl-tools"
)

var _ = Suite(&testDDLSuite{})

type testDDLSuite struct{}

func (s *testDDLSuite) newSubTaskCfg(dbCfg config.DBConfig) *config.SubTaskConfig {
	return &config.SubTaskConfig{
		From:             dbCfg,
		To:               dbCfg,
		ServerID:         101,
		MetaSchema:       "test",
		Name:             "syncer_ut",
		Mode:             config.ModeIncrement,
		Flavor:           "mysql",
		ShadowTableRules: []string{config.DefaultShadowTableRules},
		TrashTableRules:  []string{config.DefaultTrashTableRules},
	}
}

func (s *testDDLSuite) TestAnsiQuotes(c *C) {
	ansiQuotesCases := []string{
		"create database `test`",
		"create table `test`.`test`(id int)",
		"create table `test`.\"test\" (id int)",
		"create table \"test\".`test` (id int)",
		"create table \"test\".\"test\"",
		"create table test.test (\"id\" int)",
		"insert into test.test (\"id\") values('a')",
	}

	db, mock, err := sqlmock.New()
	mock.ExpectQuery("SHOW VARIABLES LIKE").
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).
			AddRow("sql_mode", "ANSI_QUOTES"))
	c.Assert(err, IsNil)

	parser, err := utils.GetParser(context.Background(), db)
	c.Assert(err, IsNil)

	for _, sql := range ansiQuotesCases {
		_, err = parser.ParseOneStmt(sql, "", "")
		c.Assert(err, IsNil)
	}
}

func (s *testDDLSuite) TestDDLWithDashComments(c *C) {
	sql := `--
-- this is a comment.
--
CREATE TABLE test.test_table_with_c (id int);
`
	parser := parser.New()
	_, err := parserpkg.Parse(parser, sql, "", "")
	c.Assert(err, IsNil)
}

func (s *testDDLSuite) TestCommentQuote(c *C) {
	sql := "ALTER TABLE schemadb.ep_edu_course_message_auto_reply MODIFY answer JSON COMMENT '回复的内容-格式为list，有两个字段：\"answerType\"：//''发送客服消息类型：1-文本消息，2-图片，3-图文链接''；  answer：回复内容';"
	expectedSQL := "ALTER TABLE `schemadb`.`ep_edu_course_message_auto_reply` MODIFY COLUMN `answer` JSON COMMENT '回复的内容-格式为list，有两个字段：\"answerType\"：//''发送客服消息类型：1-文本消息，2-图片，3-图文链接''；  answer：回复内容'"

	tctx := tcontext.Background().WithLogger(log.With(zap.String("test", "TestCommentQuote")))
	testEC := &eventContext{
		tctx: tctx,
	}
	qec := &queryEventContext{
		eventContext: testEC,
		ddlSchema:    "schemadb",
		originSQL:    sql,
		p:            parser.New(),
	}
	stmt, err := parseOneStmt(qec)
	c.Assert(err, IsNil)

	qec.splitDDLs, err = parserpkg.SplitDDL(stmt, qec.ddlSchema)
	c.Assert(err, IsNil)

	syncer := NewSyncer(&config.SubTaskConfig{}, nil, nil)
	syncer.tctx = tctx
	c.Assert(syncer.genRouter(), IsNil)

	for _, sql := range qec.splitDDLs {
		sqls, err := syncer.processOneDDL(qec, sql)
		c.Assert(err, IsNil)
		qec.appliedDDLs = append(qec.appliedDDLs, sqls...)
	}
	c.Assert(len(qec.appliedDDLs), Equals, 1)
	c.Assert(qec.appliedDDLs[0], Equals, expectedSQL)
}

func (s *testDDLSuite) TestResolveDDLSQL(c *C) {
	// duplicate with pkg/parser
	sqls := []string{
		"create schema `s1`",
		"create schema if not exists `s1`",
		"drop schema `s1`",
		"drop schema if exists `s1`",
		"drop table `s1`.`t1`",
		"drop table `s1`.`t1`, `s2`.`t2`",
		"drop table `s1`.`t1`, `s2`.`t2`, `xx`",
		"create table `s1`.`t1` (id int)",
		"create table `t1` (id int)",
		"create table `t1` like `t2`",
		"create table `s1`.`t1` like `t2`",
		"create table `s1`.`t1` like `s1`.`t2`",
		"create table `t1` like `xx`.`t2`",
		"truncate table `t1`",
		"truncate table `s1`.`t1`",
		"rename table `s1`.`t1` to `s2`.`t2`",
		"rename table `t1` to `t2`, `s1`.`t1` to `s1`.`t2`",
		"drop index i1 on `s1`.`t1`",
		"drop index i1 on `t1`",
		"create index i1 on `t1`(`c1`)",
		"create index i1 on `s1`.`t1`(`c1`)",
		"alter table `t1` add column c1 int, drop column c2",
		"alter table `s1`.`t1` add column c1 int, rename to `t2`, drop column c2",
		"alter table `s1`.`t1` add column c1 int, rename to `xx`.`t2`, drop column c2",
	}

	expectedSQLs := [][]string{
		{"CREATE DATABASE IF NOT EXISTS `s1`"},
		{"CREATE DATABASE IF NOT EXISTS `s1`"},
		{"DROP DATABASE IF EXISTS `s1`"},
		{"DROP DATABASE IF EXISTS `s1`"},
		{"DROP TABLE IF EXISTS `s1`.`t1`"},
		{"DROP TABLE IF EXISTS `s1`.`t1`"},
		{"DROP TABLE IF EXISTS `s1`.`t1`"},
		{"CREATE TABLE IF NOT EXISTS `s1`.`t1` (`id` INT)"},
		{},
		{},
		{},
		{"CREATE TABLE IF NOT EXISTS `s1`.`t1` LIKE `s1`.`t2`"},
		{},
		{},
		{"TRUNCATE TABLE `s1`.`t1`"},
		{},
		{"RENAME TABLE `s1`.`t1` TO `s1`.`t2`"},
		{"DROP INDEX /*T! IF EXISTS  */`i1` ON `s1`.`t1`"},
		{},
		{},
		{"CREATE INDEX `i1` ON `s1`.`t1` (`c1`)"},
		{},
		{"ALTER TABLE `s1`.`t1` ADD COLUMN `c1` INT"},
		{"ALTER TABLE `s1`.`t1` ADD COLUMN `c1` INT"},
	}

	targetSQLs := [][]string{
		{"CREATE DATABASE IF NOT EXISTS `xs1`"},
		{"CREATE DATABASE IF NOT EXISTS `xs1`"},
		{"DROP DATABASE IF EXISTS `xs1`"},
		{"DROP DATABASE IF EXISTS `xs1`"},
		{"DROP TABLE IF EXISTS `xs1`.`t1`"},
		{"DROP TABLE IF EXISTS `xs1`.`t1`"},
		{"DROP TABLE IF EXISTS `xs1`.`t1`"},
		{"CREATE TABLE IF NOT EXISTS `xs1`.`t1` (`id` INT)"},
		{},
		{},
		{},
		{"CREATE TABLE IF NOT EXISTS `xs1`.`t1` LIKE `xs1`.`t2`"},
		{},
		{},
		{"TRUNCATE TABLE `xs1`.`t1`"},
		{},
		{"RENAME TABLE `xs1`.`t1` TO `xs1`.`t2`"},
		{"DROP INDEX /*T! IF EXISTS  */`i1` ON `xs1`.`t1`"},
		{},
		{},
		{"CREATE INDEX `i1` ON `xs1`.`t1` (`c1`)"},
		{},
		{"ALTER TABLE `xs1`.`t1` ADD COLUMN `c1` INT"},
		{"ALTER TABLE `xs1`.`t1` ADD COLUMN `c1` INT"},
	}
	tctx := tcontext.Background().WithLogger(log.With(zap.String("test", "TestResolveDDLSQL")))

	cfg := &config.SubTaskConfig{
		BAList: &filter.Rules{
			DoDBs: []string{"s1"},
		},
	}
	var err error
	syncer := NewSyncer(cfg, nil, nil)
	syncer.tctx = tctx
	syncer.baList, err = filter.New(syncer.cfg.CaseSensitive, syncer.cfg.BAList)
	c.Assert(err, IsNil)

	syncer.tableRouter, err = regexprrouter.NewRegExprRouter(false, []*router.TableRule{
		{
			SchemaPattern: "s1",
			TargetSchema:  "xs1",
		},
	})
	c.Assert(err, IsNil)

	testEC := &eventContext{
		tctx: tctx,
	}
	statusVars := []byte{4, 0, 0, 0, 0, 46, 0}
	syncer.idAndCollationMap = map[int]string{46: "utf8mb4_bin"}
	for i, sql := range sqls {
		qec := &queryEventContext{
			eventContext:    testEC,
			ddlSchema:       "test",
			originSQL:       sql,
			appliedDDLs:     make([]string, 0),
			p:               parser.New(),
			eventStatusVars: statusVars,
		}
		stmt, err := parseOneStmt(qec)
		c.Assert(err, IsNil)

		qec.splitDDLs, err = parserpkg.SplitDDL(stmt, qec.ddlSchema)
		c.Assert(err, IsNil)
		for _, sql2 := range qec.splitDDLs {
			sqls, err := syncer.processOneDDL(qec, sql2)
			c.Assert(err, IsNil)
			for _, sql3 := range sqls {
				if len(sql3) == 0 {
					continue
				}
				qec.appliedDDLs = append(qec.appliedDDLs, sql3)
			}
		}
		c.Assert(qec.appliedDDLs, DeepEquals, expectedSQLs[i])
		c.Assert(targetSQLs[i], HasLen, len(qec.appliedDDLs))
		for j, sql2 := range qec.appliedDDLs {
			ddlInfo, err2 := syncer.genDDLInfo(qec, sql2)
			c.Assert(err2, IsNil)
			c.Assert(targetSQLs[i][j], Equals, ddlInfo.routedDDL)
		}
	}
}

func (s *testDDLSuite) TestParseOneStmt(c *C) {
	cases := []struct {
		sql      string
		isDDL    bool
		hasError bool
	}{
		{
			sql:      "FLUSH",
			isDDL:    false,
			hasError: true,
		},
		{
			sql:      "BEGIN",
			isDDL:    false,
			hasError: false,
		},
		{
			sql:      "CREATE TABLE do_db.do_table (c1 INT)",
			isDDL:    true,
			hasError: false,
		},
		{
			sql:      "INSERT INTO do_db.do_table VALUES (1)",
			isDDL:    false,
			hasError: false,
		},
		{
			sql:      "INSERT INTO ignore_db.ignore_table VALUES (1)",
			isDDL:    false,
			hasError: false,
		},
		{
			sql:      "UPDATE `ignore_db`.`ignore_table` SET c1=2 WHERE c1=1",
			isDDL:    false,
			hasError: false,
		},
		{
			sql:      "DELETE FROM `ignore_table` WHERE c1=2",
			isDDL:    false,
			hasError: false,
		},
		{
			sql:      "SELECT * FROM ignore_db.ignore_table",
			isDDL:    false,
			hasError: false,
		},
		{
			sql:      "#",
			isDDL:    false,
			hasError: false,
		},
		{
			sql:      "# this is a comment",
			isDDL:    false,
			hasError: false,
		},
		{
			sql:      "# a comment with DDL\nCREATE TABLE do_db.do_table (c1 INT)",
			isDDL:    true,
			hasError: false,
		},
		{
			sql:      "# a comment with DML\nUPDATE `ignore_db`.`ignore_table` SET c1=2 WHERE c1=1",
			isDDL:    false,
			hasError: false,
		},
		{
			sql:      "NOT A SQL",
			isDDL:    false,
			hasError: true,
		},
	}
	tctx := tcontext.Background().WithLogger(log.With(zap.String("test", "TestparseOneStmt")))
	qec := &queryEventContext{
		eventContext: &eventContext{
			tctx: tctx,
		},
		p: parser.New(),
	}

	for _, cs := range cases {
		qec.originSQL = cs.sql
		stmt, err := parseOneStmt(qec)
		if cs.hasError {
			c.Assert(err, NotNil)
		} else {
			c.Assert(err, IsNil)
		}
		_, ok := stmt.(ast.DDLNode)
		c.Assert(ok, Equals, cs.isDDL)
	}
}

func (s *testDDLSuite) TestResolveGeneratedColumnSQL(c *C) {
	testCases := []struct {
		sql      string
		expected string
	}{
		{
			"ALTER TABLE `test`.`test` ADD COLUMN d int(11) GENERATED ALWAYS AS (c + 1) VIRTUAL",
			"ALTER TABLE `test`.`test` ADD COLUMN `d` INT(11) GENERATED ALWAYS AS(`c`+1) VIRTUAL",
		},
		{
			"ALTER TABLE `test`.`test` ADD COLUMN d int(11) AS (1 + 1) STORED",
			"ALTER TABLE `test`.`test` ADD COLUMN `d` INT(11) GENERATED ALWAYS AS(1+1) STORED",
		},
	}

	tctx := tcontext.Background().WithLogger(log.With(zap.String("test", "TestResolveGeneratedColumnSQL")))
	syncer := NewSyncer(&config.SubTaskConfig{}, nil, nil)
	syncer.tctx = tctx
	c.Assert(syncer.genRouter(), IsNil)
	p := parser.New()
	for _, tc := range testCases {
		qec := &queryEventContext{
			eventContext: &eventContext{
				tctx: tctx,
			},
			originSQL:   tc.sql,
			appliedDDLs: make([]string, 0),
			ddlSchema:   "test",
			p:           p,
		}
		stmt, err := parseOneStmt(qec)
		c.Assert(err, IsNil)

		qec.splitDDLs, err = parserpkg.SplitDDL(stmt, qec.ddlSchema)
		c.Assert(err, IsNil)
		for _, sql := range qec.splitDDLs {
			sqls, err := syncer.processOneDDL(qec, sql)
			c.Assert(err, IsNil)
			qec.appliedDDLs = append(qec.appliedDDLs, sqls...)
		}

		c.Assert(len(qec.appliedDDLs), Equals, 1)
		c.Assert(qec.appliedDDLs[0], Equals, tc.expected)
	}
}

func (s *testDDLSuite) TestResolveOnlineDDL(c *C) {
	cases := []struct {
		sql       string
		expectSQL string
	}{
		// *****GHOST*****
		// real table
		{
			sql:       "ALTER TABLE `test`.`t1` ADD COLUMN `n` INT",
			expectSQL: "ALTER TABLE `test`.`t1` ADD COLUMN `n` INT",
		},
		// trash table
		{
			sql: "CREATE TABLE IF NOT EXISTS `test`.`_t1_del` (`n` INT)",
		},
		// ghost table
		{
			sql: "ALTER TABLE `test`.`_t1_gho` ADD COLUMN `n` INT",
		},
		// *****PT*****
		// real table
		{
			sql:       "ALTER TABLE `test`.`t1` ADD COLUMN `n` INT",
			expectSQL: "ALTER TABLE `test`.`t1` ADD COLUMN `n` INT",
		},
		// trash table
		{
			sql: "CREATE TABLE IF NOT EXISTS `test`.`_t1_old` (`n` INT)",
		},
		// ghost table
		{
			sql: "ALTER TABLE `test`.`_t1_new` ADD COLUMN `n` INT",
		},
	}
	tctx := tcontext.Background().WithLogger(log.With(zap.String("test", "TestResolveOnlineDDL")))
	p := parser.New()

	testEC := &eventContext{tctx: tctx}
	cluster, err := conn.NewCluster()
	c.Assert(err, IsNil)
	c.Assert(cluster.Start(), IsNil)
	mockClusterPort := cluster.Port
	dbCfg := config.GetDBConfigForTest()
	dbCfg.Port = mockClusterPort
	dbCfg.Password = ""
	cfg := s.newSubTaskCfg(dbCfg)

	var qec *queryEventContext
	for _, ca := range cases {
		plugin, err := onlineddl.NewRealOnlinePlugin(tctx, cfg)
		c.Assert(err, IsNil)
		syncer := NewSyncer(cfg, nil, nil)
		syncer.tctx = tctx
		syncer.onlineDDL = plugin
		c.Assert(plugin.Clear(tctx), IsNil)
		c.Assert(syncer.genRouter(), IsNil)
		qec = &queryEventContext{
			eventContext: testEC,
			ddlSchema:    "test",
			appliedDDLs:  make([]string, 0),
			p:            p,
		}
		qec.originSQL = ca.sql
		stmt, err := parseOneStmt(qec)
		c.Assert(err, IsNil)
		_, ok := stmt.(ast.DDLNode)
		c.Assert(ok, IsTrue)
		qec.splitDDLs, err = parserpkg.SplitDDL(stmt, qec.ddlSchema)
		c.Assert(err, IsNil)
		for _, sql := range qec.splitDDLs {
			sqls, err := syncer.processOneDDL(qec, sql)
			c.Assert(err, IsNil)
			qec.appliedDDLs = append(qec.appliedDDLs, sqls...)
		}
		if len(ca.expectSQL) != 0 {
			c.Assert(qec.appliedDDLs, HasLen, 1)
			c.Assert(qec.appliedDDLs[0], Equals, ca.expectSQL)
		}
	}
	cluster.Stop()
}

func (s *testDDLSuite) TestMistakeOnlineDDLRegex(c *C) {
	cases := []struct {
		onlineType string
		trashName  string
		ghostname  string
		matchGho   bool
	}{
		{
			config.GHOST,
			"_t1_del",
			"_t1_gho_invalid",
			false,
		},
		{
			config.GHOST,
			"_t1_del_invalid",
			"_t1_gho",
			true,
		},
		{
			config.PT,
			"_t1_old",
			"_t1_new_invalid",
			false,
		},
		{
			config.PT,
			"_t1_old_invalid",
			"_t1_new",
			true,
		},
	}
	tctx := tcontext.Background().WithLogger(log.With(zap.String("test", "TestMistakeOnlineDDLRegex")))
	p := parser.New()

	ec := eventContext{tctx: tctx}
	cluster, err := conn.NewCluster()
	c.Assert(err, IsNil)
	c.Assert(cluster.Start(), IsNil)
	mockClusterPort := cluster.Port
	c.Assert(err, IsNil)
	dbCfg := config.GetDBConfigForTest()
	dbCfg.Port = mockClusterPort
	dbCfg.Password = ""
	cfg := s.newSubTaskCfg(dbCfg)
	for _, ca := range cases {
		plugin, err := onlineddl.NewRealOnlinePlugin(tctx, cfg)
		c.Assert(err, IsNil)
		syncer := NewSyncer(cfg, nil, nil)
		c.Assert(syncer.genRouter(), IsNil)
		syncer.onlineDDL = plugin
		c.Assert(plugin.Clear(tctx), IsNil)

		// ghost table
		sql := fmt.Sprintf("ALTER TABLE `test`.`%s` ADD COLUMN `n` INT", ca.ghostname)
		qec := &queryEventContext{
			eventContext: &ec,
			ddlSchema:    "test",
			p:            p,
		}
		sqls, err := syncer.processOneDDL(qec, sql)
		c.Assert(err, IsNil)
		table := ca.ghostname
		matchRules := config.ShadowTableRules
		if ca.matchGho {
			c.Assert(sqls, HasLen, 0)
			table = ca.trashName
			matchRules = config.TrashTableRules
		} else {
			c.Assert(sqls, HasLen, 1)
			c.Assert(sqls[0], Equals, sql)
		}
		sql = fmt.Sprintf("RENAME TABLE `test`.`t1` TO `test`.`%s`, `test`.`%s` TO `test`.`t1`", ca.trashName, ca.ghostname)
		qec = &queryEventContext{
			eventContext: &ec,
			ddlSchema:    "test",
			p:            p,
		}
		sqls, err = syncer.processOneDDL(qec, sql)
		c.Assert(terror.ErrConfigOnlineDDLMistakeRegex.Equal(err), IsTrue)
		c.Assert(sqls, HasLen, 0)
		c.Assert(err, ErrorMatches, ".*"+sql+".*"+table+".*"+matchRules+".*")
	}
	cluster.Stop()
}

func (s *testDDLSuite) TestDropSchemaInSharding(c *C) {
	var (
		targetTable = &filter.Table{
			Schema: "target_db",
			Name:   "tbl",
		}
		sourceDB = "db1"
		source1  = "`db1`.`tbl1`"
		source2  = "`db1`.`tbl2`"
		tctx     = tcontext.Background()
	)
	dbCfg := config.GetDBConfigForTest()
	cfg := s.newSubTaskCfg(dbCfg)
	cfg.ShardMode = config.ShardPessimistic
	syncer := NewSyncer(cfg, nil, nil)
	// nolint:dogsled
	_, _, _, _, err := syncer.sgk.AddGroup(targetTable, []string{source1}, nil, true)
	c.Assert(err, IsNil)
	// nolint:dogsled
	_, _, _, _, err = syncer.sgk.AddGroup(targetTable, []string{source2}, nil, true)
	c.Assert(err, IsNil)
	c.Assert(syncer.sgk.Groups(), HasLen, 2)
	c.Assert(syncer.dropSchemaInSharding(tctx, sourceDB), IsNil)
	c.Assert(syncer.sgk.Groups(), HasLen, 0)
}

func (s *testDDLSuite) TestClearOnlineDDL(c *C) {
	var (
		targetTable = &filter.Table{
			Schema: "target_db",
			Name:   "tbl",
		}
		source1 = "`db1`.`tbl1`"
		key1    = "db1tbl1"
		source2 = "`db1`.`tbl2`"
		key2    = "db1tbl2"
		tctx    = tcontext.Background()
	)
	dbCfg := config.GetDBConfigForTest()
	cfg := s.newSubTaskCfg(dbCfg)
	cfg.ShardMode = config.ShardPessimistic
	syncer := NewSyncer(cfg, nil, nil)
	mock := mockOnlinePlugin{
		map[string]struct{}{key1: {}, key2: {}},
	}
	syncer.onlineDDL = mock

	// nolint:dogsled
	_, _, _, _, err := syncer.sgk.AddGroup(targetTable, []string{source1}, nil, true)
	c.Assert(err, IsNil)
	// nolint:dogsled
	_, _, _, _, err = syncer.sgk.AddGroup(targetTable, []string{source2}, nil, true)
	c.Assert(err, IsNil)

	c.Assert(syncer.clearOnlineDDL(tctx, targetTable), IsNil)
	c.Assert(mock.toFinish, HasLen, 0)
}

func (s *testDDLSuite) TestAdjustDatabaseCollation(c *C) {
	statusVarsArray := [][]byte{
		{
			4, 0, 0, 0, 0, 46, 0,
		},
		{
			4, 0, 0, 0, 0, 21, 1,
		},
	}

	sqls := []string{
		"create database if not exists `test`",
		"create database `test` CHARACTER SET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"create database `test` CHARACTER SET=utf8mb4",
		"create database `test` COLLATE=utf8mb4_general_ci",
	}

	expectedSQLs := [][]string{
		{
			"CREATE DATABASE IF NOT EXISTS `test` COLLATE = utf8mb4_bin",
			"CREATE DATABASE `test` CHARACTER SET = utf8mb4 COLLATE = utf8mb4_general_ci",
			"CREATE DATABASE `test` CHARACTER SET = utf8mb4 COLLATE = utf8mb4_general_ci",
			"CREATE DATABASE `test` COLLATE = utf8mb4_general_ci",
		},
		{
			"CREATE DATABASE IF NOT EXISTS `test` COLLATE = utf8mb4_vi_0900_ai_ci",
			"CREATE DATABASE `test` CHARACTER SET = utf8mb4 COLLATE = utf8mb4_general_ci",
			"CREATE DATABASE `test` CHARACTER SET = utf8mb4 COLLATE = utf8mb4_general_ci",
			"CREATE DATABASE `test` COLLATE = utf8mb4_general_ci",
		},
	}

	tctx := tcontext.Background().WithLogger(log.With(zap.String("test", "TestAdjustTableCollation")))
	syncer := NewSyncer(&config.SubTaskConfig{CollationCompatible: config.StrictCollationCompatible}, nil, nil)
	syncer.tctx = tctx
	p := parser.New()
	tab := &filter.Table{
		Schema: "test",
		Name:   "t",
	}

	charsetAndDefaultCollationMap := map[string]string{"utf8mb4": "utf8mb4_general_ci"}
	idAndCollationMap := map[int]string{46: "utf8mb4_bin", 277: "utf8mb4_vi_0900_ai_ci"}
	for i, statusVars := range statusVarsArray {
		for j, sql := range sqls {
			ddlInfo := &ddlInfo{
				originDDL:    sql,
				routedDDL:    sql,
				sourceTables: []*filter.Table{tab},
				targetTables: []*filter.Table{tab},
			}
			stmt, err := p.ParseOneStmt(sql, "", "")
			c.Assert(err, IsNil)
			c.Assert(stmt, NotNil)
			ddlInfo.originStmt = stmt
			adjustCollation(tctx, ddlInfo, statusVars, charsetAndDefaultCollationMap, idAndCollationMap)
			routedDDL, err := parserpkg.RenameDDLTable(ddlInfo.originStmt, ddlInfo.targetTables)
			c.Assert(err, IsNil)
			c.Assert(routedDDL, Equals, expectedSQLs[i][j])
		}
	}
}

func (s *testDDLSuite) TestAdjustCollation(c *C) {
	sqls := []string{
		"create table `test`.`t1` (id int) CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"create table `test`.`t1` (id int) CHARSET=utf8mb4",
		"create table `test`.`t1` (id int) COLLATE=utf8mb4_general_ci",
		"create table `test`.`t1` (id int)",
		"create table `test`.`t1` (id int, name varchar(20) CHARACTER SET utf8mb4, work varchar(20))",
		"create table `test`.`t1` (id int, name varchar(20), work varchar(20))",
		"create table `test`.`t1` (id int, name varchar(20) COLLATE utf8mb4_general_ci, work varchar(20))",
		"create table `test`.`t1` (id int, name varchar(20) COLLATE utf8mb4_general_ci, work varchar(20) CHARACTER SET utf8mb4)",
		"create table `test`.`t1` (id int, name varchar(20) CHARACTER SET utf8mb4, work varchar(20)) COLLATE=utf8mb4_general_ci",
		"create table `test`.`t1` (id int, name varchar(20) CHARACTER SET utf8mb4, work varchar(20)) COLLATE=latin1_swedish_ci",
		"create table `test`.`t1` (id int, name varchar(20), work varchar(20)) COLLATE=utf8mb4_general_ci",
		"create table `test`.`t1` (id int, name varchar(20) COLLATE utf8mb4_general_ci, work varchar(20)) COLLATE=utf8mb4_general_ci",
		"create table `test`.`t1` (id int, name varchar(20) COLLATE utf8mb4_general_ci, work varchar(20) CHARACTER SET utf8mb4) COLLATE=utf8mb4_general_ci",
		"create table `test`.`t1` (id int, name varchar(20) CHARACTER SET utf8mb4, work varchar(20)) CHARSET=utf8mb4 ",
		"create table `test`.`t1` (id int, name varchar(20) CHARACTER SET latin1, work varchar(20)) CHARSET=utf8mb4 ",
		"create table `test`.`t1` (id int, name varchar(20), work varchar(20)) CHARSET=utf8mb4",
		"create table `test`.`t1` (id int, name varchar(20) COLLATE utf8mb4_general_ci, work varchar(20)) CHARSET=utf8mb4",
		"create table `test`.`t1` (id int, name varchar(20) COLLATE utf8mb4_general_ci, work varchar(20) CHARACTER SET utf8mb4) CHARSET=utf8mb4",
	}

	expectedSQLs := []string{
		"CREATE TABLE `test`.`t` (`id` INT) DEFAULT CHARACTER SET = UTF8MB4 DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT) DEFAULT CHARACTER SET = UTF8MB4 DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT) DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT)",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) CHARACTER SET UTF8MB4 COLLATE utf8mb4_general_ci,`work` VARCHAR(20))",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20),`work` VARCHAR(20))",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) COLLATE utf8mb4_general_ci,`work` VARCHAR(20))",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) COLLATE utf8mb4_general_ci,`work` VARCHAR(20) CHARACTER SET UTF8MB4 COLLATE utf8mb4_general_ci)",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) CHARACTER SET UTF8MB4 COLLATE utf8mb4_general_ci,`work` VARCHAR(20)) DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) CHARACTER SET UTF8MB4 COLLATE utf8mb4_general_ci,`work` VARCHAR(20)) DEFAULT COLLATE = LATIN1_SWEDISH_CI",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20),`work` VARCHAR(20)) DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) COLLATE utf8mb4_general_ci,`work` VARCHAR(20)) DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) COLLATE utf8mb4_general_ci,`work` VARCHAR(20) CHARACTER SET UTF8MB4 COLLATE utf8mb4_general_ci) DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) CHARACTER SET UTF8MB4 COLLATE utf8mb4_general_ci,`work` VARCHAR(20)) DEFAULT CHARACTER SET = UTF8MB4 DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) CHARACTER SET LATIN1 COLLATE latin1_swedish_ci,`work` VARCHAR(20)) DEFAULT CHARACTER SET = UTF8MB4 DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20),`work` VARCHAR(20)) DEFAULT CHARACTER SET = UTF8MB4 DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) COLLATE utf8mb4_general_ci,`work` VARCHAR(20)) DEFAULT CHARACTER SET = UTF8MB4 DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
		"CREATE TABLE `test`.`t` (`id` INT,`name` VARCHAR(20) COLLATE utf8mb4_general_ci,`work` VARCHAR(20) CHARACTER SET UTF8MB4 COLLATE utf8mb4_general_ci) DEFAULT CHARACTER SET = UTF8MB4 DEFAULT COLLATE = UTF8MB4_GENERAL_CI",
	}

	tctx := tcontext.Background().WithLogger(log.With(zap.String("test", "TestAdjustTableCollation")))
	syncer := NewSyncer(&config.SubTaskConfig{CollationCompatible: config.StrictCollationCompatible}, nil, nil)
	syncer.tctx = tctx
	p := parser.New()
	tab := &filter.Table{
		Schema: "test",
		Name:   "t",
	}
	statusVars := []byte{4, 0, 0, 0, 0, 46, 0}
	charsetAndDefaultCollationMap := map[string]string{"utf8mb4": "utf8mb4_general_ci", "latin1": "latin1_swedish_ci"}
	idAndCollationMap := map[int]string{46: "utf8mb4_bin"}
	for i, sql := range sqls {
		ddlInfo := &ddlInfo{
			originDDL:    sql,
			routedDDL:    sql,
			sourceTables: []*filter.Table{tab},
			targetTables: []*filter.Table{tab},
		}
		stmt, err := p.ParseOneStmt(sql, "", "")
		c.Assert(err, IsNil)
		c.Assert(stmt, NotNil)
		ddlInfo.originStmt = stmt
		adjustCollation(tctx, ddlInfo, statusVars, charsetAndDefaultCollationMap, idAndCollationMap)
		routedDDL, err := parserpkg.RenameDDLTable(ddlInfo.originStmt, ddlInfo.targetTables)
		c.Assert(err, IsNil)
		c.Assert(routedDDL, Equals, expectedSQLs[i])
	}
}

type mockOnlinePlugin struct {
	toFinish map[string]struct{}
}

func (m mockOnlinePlugin) Apply(tctx *tcontext.Context, tables []*filter.Table, statement string, stmt ast.StmtNode, p *parser.Parser) ([]string, error) {
	return nil, nil
}

func (m mockOnlinePlugin) Finish(tctx *tcontext.Context, table *filter.Table) error {
	tableID := table.Schema + table.Name
	if _, ok := m.toFinish[tableID]; !ok {
		return errors.New("finish table not found")
	}
	delete(m.toFinish, tableID)
	return nil
}

func (m mockOnlinePlugin) TableType(table string) onlineddl.TableType {
	// 5 is _ _gho/ghc/del or _ _old/new
	if len(table) > 5 && strings.HasPrefix(table, "_") {
		if strings.HasSuffix(table, "_gho") || strings.HasSuffix(table, "_new") {
			return onlineddl.GhostTable
		}

		if strings.HasSuffix(table, "_ghc") || strings.HasSuffix(table, "_del") || strings.HasSuffix(table, "_old") {
			return onlineddl.TrashTable
		}
	}
	return onlineddl.RealTable
}

func (m mockOnlinePlugin) RealName(table string) string {
	return ""
}

func (m mockOnlinePlugin) ResetConn(tctx *tcontext.Context) error {
	return nil
}

func (m mockOnlinePlugin) Clear(tctx *tcontext.Context) error {
	return nil
}

func (m mockOnlinePlugin) Close() {
}

func (m mockOnlinePlugin) CheckAndUpdate(tctx *tcontext.Context, schemas map[string]string, tables map[string]map[string]string) error {
	return nil
}

func (m mockOnlinePlugin) CheckRegex(stmt ast.StmtNode, schema string, flavor utils.LowerCaseTableNamesFlavor) error {
	return nil
}
