// Copyright 2015 PingCAP, Inc.
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

package ddl_test

import (
	"fmt"
	"io"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juju/errors"
	. "github.com/pingcap/check"
	gofail "github.com/pingcap/gofail/runtime"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	tmysql "github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/store/mockstore"
	"github.com/pingcap/tidb/store/mockstore/mocktikv"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/table/tables"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/admin"
	"github.com/pingcap/tidb/util/mock"
	"github.com/pingcap/tidb/util/testkit"
	"github.com/pingcap/tidb/util/testleak"
	"github.com/pingcap/tidb/util/testutil"
	"golang.org/x/net/context"
)

const (
	// waitForCleanDataRound indicates how many times should we check data is cleaned or not.
	waitForCleanDataRound = 150
	// waitForCleanDataInterval is a min duration between 2 check for data clean.
	waitForCleanDataInterval = time.Millisecond * 100
)

var _ = Suite(&testDBSuite{})

const defaultBatchSize = 2048

type testDBSuite struct {
	cluster    *mocktikv.Cluster
	mvccStore  mocktikv.MVCCStore
	store      kv.Storage
	dom        *domain.Domain
	schemaName string
	tk         *testkit.TestKit
	s          session.Session
	lease      time.Duration
	autoIDStep int64
}

func (s *testDBSuite) SetUpSuite(c *C) {
	var err error

	testleak.BeforeTest()

	s.lease = 200 * time.Millisecond
	session.SetSchemaLease(s.lease)
	session.SetStatsLease(0)
	s.schemaName = "test_db"
	s.autoIDStep = autoid.GetStep()
	autoid.SetStep(5000)

	s.cluster = mocktikv.NewCluster()
	mocktikv.BootstrapWithSingleStore(s.cluster)
	s.mvccStore = mocktikv.MustNewMVCCStore()
	store, err := mockstore.NewMockTikvStore(
		mockstore.WithCluster(s.cluster),
		mockstore.WithMVCCStore(s.mvccStore),
	)
	c.Assert(err, IsNil)

	s.store = store
	c.Assert(err, IsNil)

	s.dom, err = session.BootstrapSession(s.store)
	c.Assert(err, IsNil)

	s.s, err = session.CreateSession4Test(s.store)
	c.Assert(err, IsNil)

	_, err = s.s.Execute(context.Background(), "create database test_db")
	c.Assert(err, IsNil)

	s.tk = testkit.NewTestKit(c, s.store)
}

func (s *testDBSuite) TearDownSuite(c *C) {
	s.s.Execute(context.Background(), "drop database if exists test_db")
	s.s.Close()
	s.dom.Close()
	s.store.Close()
	testleak.AfterTest(c)()
	autoid.SetStep(s.autoIDStep)
}

func (s *testDBSuite) testErrorCode(c *C, sql string, errCode int) {
	_, err := s.tk.Exec(sql)
	c.Assert(err, NotNil)
	originErr := errors.Cause(err)
	tErr, ok := originErr.(*terror.Error)
	c.Assert(ok, IsTrue, Commentf("err: %T", originErr))
	c.Assert(tErr.ToSQLError().Code, DeepEquals, uint16(errCode), Commentf("MySQL code:%v", tErr.ToSQLError()))
}

func (s *testDBSuite) TestMySQLErrorCode(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	// create database
	sql := "create database aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s.testErrorCode(c, sql, tmysql.ErrTooLongIdent)
	sql = "create database test"
	s.testErrorCode(c, sql, tmysql.ErrDBCreateExists)
	sql = "create database test1 character set uft8;"
	s.testErrorCode(c, sql, tmysql.ErrUnknownCharacterSet)
	sql = "create database test2 character set gkb;"
	s.testErrorCode(c, sql, tmysql.ErrUnknownCharacterSet)
	sql = "create database test3 character set laitn1;"
	s.testErrorCode(c, sql, tmysql.ErrUnknownCharacterSet)
	// drop database
	sql = "drop database db_not_exist"
	s.testErrorCode(c, sql, tmysql.ErrDBDropExists)
	// crate table
	s.tk.MustExec("create table test_error_code_succ (c1 int, c2 int, c3 int, primary key(c3))")
	sql = "create table test_error_code_succ (c1 int, c2 int, c3 int)"
	s.testErrorCode(c, sql, tmysql.ErrTableExists)
	sql = "create table test_error_code1 (c1 int, c2 int, c2 int)"
	s.testErrorCode(c, sql, tmysql.ErrDupFieldName)
	sql = "create table test_error_code1 (c1 int, aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa int)"
	s.testErrorCode(c, sql, tmysql.ErrTooLongIdent)
	sql = "create table aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa(a int)"
	s.testErrorCode(c, sql, tmysql.ErrTooLongIdent)
	sql = "create table test_error_code1 (c1 int, c2 int, key aa (c1, c2), key aa (c1))"
	s.testErrorCode(c, sql, tmysql.ErrDupKeyName)
	sql = "create table test_error_code1 (c1 int, c2 int, c3 int, key(c_not_exist))"
	s.testErrorCode(c, sql, tmysql.ErrKeyColumnDoesNotExits)
	sql = "create table test_error_code1 (c1 int, c2 int, c3 int, primary key(c_not_exist))"
	s.testErrorCode(c, sql, tmysql.ErrKeyColumnDoesNotExits)
	sql = "create table test_error_code1 (c1 int not null default '')"
	s.testErrorCode(c, sql, tmysql.ErrInvalidDefault)
	sql = "CREATE TABLE `t` (`a` double DEFAULT 1.0 DEFAULT 2.0 DEFAULT now());"
	s.testErrorCode(c, sql, tmysql.ErrInvalidDefault)
	sql = "CREATE TABLE `t` (`a` double DEFAULT now());"
	s.testErrorCode(c, sql, tmysql.ErrInvalidDefault)
	sql = "create table t1(a int) character set uft8;"
	s.testErrorCode(c, sql, tmysql.ErrUnknownCharacterSet)
	sql = "create table t1(a int) character set gkb;"
	s.testErrorCode(c, sql, tmysql.ErrUnknownCharacterSet)
	sql = "create table t1(a int) character set laitn1;"
	s.testErrorCode(c, sql, tmysql.ErrUnknownCharacterSet)

	// add column
	sql = "alter table test_error_code_succ add column c1 int"
	s.testErrorCode(c, sql, tmysql.ErrDupFieldName)
	sql = "alter table test_error_code_succ add column aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa int"
	s.testErrorCode(c, sql, tmysql.ErrTooLongIdent)
	sql = "alter table test_comment comment 'test comment'"
	s.testErrorCode(c, sql, tmysql.ErrNoSuchTable)

	// drop column
	sql = "alter table test_error_code_succ drop c_not_exist"
	s.testErrorCode(c, sql, tmysql.ErrCantDropFieldOrKey)
	// add index
	sql = "alter table test_error_code_succ add index idx (c_not_exist)"
	s.testErrorCode(c, sql, tmysql.ErrKeyColumnDoesNotExits)
	s.tk.Exec("alter table test_error_code_succ add index idx (c1)")
	sql = "alter table test_error_code_succ add index idx (c1)"
	s.testErrorCode(c, sql, tmysql.ErrDupKeyName)
	// drop index
	sql = "alter table test_error_code_succ drop index idx_not_exist"
	s.testErrorCode(c, sql, tmysql.ErrCantDropFieldOrKey)
	sql = "alter table test_error_code_succ drop column c3"
	s.testErrorCode(c, sql, int(tmysql.ErrUnknown))
	// modify column
	sql = "alter table test_error_code_succ modify testx.test_error_code_succ.c1 bigint"
	s.testErrorCode(c, sql, tmysql.ErrWrongDBName)
	sql = "alter table test_error_code_succ modify t.c1 bigint"
	s.testErrorCode(c, sql, tmysql.ErrWrongTableName)
}

func (s *testDBSuite) TestAddIndexAfterAddColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.tk.MustExec("create table test_add_index_after_add_col(a int, b int not null default '0')")
	s.tk.MustExec("insert into test_add_index_after_add_col values(1, 2),(2,2)")
	s.tk.MustExec("alter table test_add_index_after_add_col add column c int not null default '0'")
	sql := "alter table test_add_index_after_add_col add unique index cc(c) "
	s.testErrorCode(c, sql, tmysql.ErrDupEntry)
}

func (s *testDBSuite) TestAddIndexWithPK(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.tk.MustExec("create table test_add_index_with_pk(a int not null, b int not null default '0', primary key(a))")
	s.tk.MustExec("insert into test_add_index_with_pk values(1, 2)")
	s.tk.MustExec("alter table test_add_index_with_pk add index idx (a)")
	s.tk.MustQuery("select a from test_add_index_with_pk").Check(testkit.Rows("1"))
	s.tk.MustExec("insert into test_add_index_with_pk values(2, 2)")
	s.tk.MustExec("alter table test_add_index_with_pk add index idx1 (a, b)")
	s.tk.MustQuery("select * from test_add_index_with_pk").Check(testkit.Rows("1 2", "2 2"))
	s.tk.MustExec("create table test_add_index_with_pk1(a int not null, b int not null default '0', c int, d int, primary key(c))")
	s.tk.MustExec("insert into test_add_index_with_pk1 values(1, 1, 1, 1)")
	s.tk.MustExec("alter table test_add_index_with_pk1 add index idx (c)")
	s.tk.MustExec("insert into test_add_index_with_pk1 values(2, 2, 2, 2)")
	s.tk.MustQuery("select * from test_add_index_with_pk1").Check(testkit.Rows("1 1 1 1", "2 2 2 2"))
	s.tk.MustExec("create table test_add_index_with_pk2(a int not null, b int not null default '0', c int unsigned, d int, primary key(c))")
	s.tk.MustExec("insert into test_add_index_with_pk2 values(1, 1, 1, 1)")
	s.tk.MustExec("alter table test_add_index_with_pk2 add index idx (c)")
	s.tk.MustExec("insert into test_add_index_with_pk2 values(2, 2, 2, 2)")
	s.tk.MustQuery("select * from test_add_index_with_pk2").Check(testkit.Rows("1 1 1 1", "2 2 2 2"))
}

func (s *testDBSuite) testGetTable(c *C, name string) table.Table {
	ctx := s.s.(sessionctx.Context)
	dom := domain.GetDomain(ctx)
	// Make sure the table schema is the new schema.
	err := dom.Reload()
	c.Assert(err, IsNil)
	tbl, err := dom.InfoSchema().TableByName(model.NewCIStr(s.schemaName), model.NewCIStr(name))
	c.Assert(err, IsNil)
	return tbl
}

func backgroundExec(s kv.Storage, sql string, done chan error) {
	se, err := session.CreateSession4Test(s)
	if err != nil {
		done <- errors.Trace(err)
		return
	}
	defer se.Close()
	_, err = se.Execute(context.Background(), "use test_db")
	if err != nil {
		done <- errors.Trace(err)
		return
	}
	_, err = se.Execute(context.Background(), sql)
	done <- errors.Trace(err)
}

func (s *testDBSuite) TestAddUniqueIndexRollback(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.mustExec(c, "use test_db")
	s.mustExec(c, "drop table if exists t1")
	s.mustExec(c, "create table t1 (c1 int, c2 int, c3 int, primary key(c1))")
	// defaultBatchSize is equal to ddl.defaultBatchSize
	base := defaultBatchSize * 2
	count := base
	// add some rows
	for i := 0; i < count; i++ {
		s.mustExec(c, "insert into t1 values (?, ?, ?)", i, i, i)
	}
	// add some duplicate rows
	for i := count - 10; i < count; i++ {
		s.mustExec(c, "insert into t1 values (?, ?, ?)", i+10, i, i)
	}

	done := make(chan error, 1)
	go backgroundExec(s.store, "create unique index c3_index on t1 (c3)", done)

	times := 0
	ticker := time.NewTicker(s.lease / 2)
	defer ticker.Stop()
LOOP:
	for {
		select {
		case err := <-done:
			c.Assert(err, NotNil)
			c.Assert(err.Error(), Equals, "[kv:1062]Duplicate for key c3_index", Commentf("err:%v", err))
			break LOOP
		case <-ticker.C:
			if times >= 10 {
				break
			}
			step := 10
			// delete some rows, and add some data
			for i := count; i < count+step; i++ {
				n := rand.Intn(count)
				s.mustExec(c, "delete from t1 where c1 = ?", n)
				s.mustExec(c, "insert into t1 values (?, ?, ?)", i+10, i, i)
			}
			count += step
			times++
		}
	}

	t := s.testGetTable(c, "t1")
	for _, tidx := range t.Indices() {
		c.Assert(strings.EqualFold(tidx.Meta().Name.L, "c3_index"), IsFalse)
	}

	// delete duplicate rows, then add index
	for i := base - 10; i < base; i++ {
		s.mustExec(c, "delete from t1 where c1 = ?", i+10)
	}
	sessionExec(c, s.store, "create index c3_index on t1 (c3)")
	s.mustExec(c, "drop table t1")
}

func (s *testDBSuite) TestCancelAddIndex(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.mustExec(c, "use test_db")
	s.mustExec(c, "drop table if exists t1")
	s.mustExec(c, "create table t1 (c1 int, c2 int, c3 int, primary key(c1))")
	// defaultBatchSize is equal to ddl.defaultBatchSize
	base := defaultBatchSize * 2
	count := base
	// add some rows
	for i := 0; i < count; i++ {
		s.mustExec(c, "insert into t1 values (?, ?, ?)", i, i, i)
	}

	var checkErr error
	var c3IdxInfo *model.IndexInfo
	hook := &ddl.TestDDLCallback{}
	first := true
	oldReorgWaitTimeout := ddl.ReorgWaitTimeout
	// let hook.OnJobUpdatedExported has chance to cancel the job.
	// the hook.OnJobUpdatedExported is called when the job is updated, runReorgJob will wait ddl.ReorgWaitTimeout, then return the ddl.runDDLJob.
	// After that ddl call d.hook.OnJobUpdated(job), so that we can canceled the job in this test case.
	ddl.ReorgWaitTimeout = 50 * time.Millisecond
	hook.OnJobUpdatedExported = func(job *model.Job) {
		addIndexNotFirstReorg := job.Type == model.ActionAddIndex && job.SchemaState == model.StateWriteReorganization && job.SnapshotVer != 0
		// If the action is adding index and the state is writing reorganization, it want to test the case of cancelling the job when backfilling indexes.
		// When the job satisfies this case of addIndexNotFirstReorg, the worker will start to backfill indexes.
		if !addIndexNotFirstReorg {
			// Get the index's meta.
			if c3IdxInfo != nil {
				return
			}
			t := s.testGetTable(c, "t1")
			for _, index := range t.WritableIndices() {
				if index.Meta().Name.L == "c3_index" {
					c3IdxInfo = index.Meta()
				}
			}
			return
		}
		// The job satisfies the case of addIndexNotFirst for the first time, the worker hasn't finished a batch of backfill indexes.
		if first {
			first = false
			return
		}
		if checkErr != nil {
			return
		}
		hookCtx := mock.NewContext()
		hookCtx.Store = s.store
		err := hookCtx.NewTxn()
		if err != nil {
			checkErr = errors.Trace(err)
			return
		}
		txn, err := hookCtx.Txn(true)
		if err != nil {
			checkErr = errors.Trace(err)
			return
		}
		jobIDs := []int64{job.ID}
		errs, err := admin.CancelJobs(txn, jobIDs)
		if err != nil {
			checkErr = errors.Trace(err)
			return
		}
		// It only tests cancel one DDL job.
		if errs[0] != nil {
			checkErr = errors.Trace(errs[0])
			return
		}
		err = txn.Commit(context.Background())
		if err != nil {
			checkErr = errors.Trace(err)
		}
	}
	originHook := s.dom.DDL().GetHook()
	s.dom.DDL().SetHook(hook)
	defer s.dom.DDL().SetHook(originHook)
	done := make(chan error, 1)
	go backgroundExec(s.store, "create unique index c3_index on t1 (c3)", done)

	times := 0
	ticker := time.NewTicker(s.lease / 2)
	defer ticker.Stop()
LOOP:
	for {
		select {
		case err := <-done:
			c.Assert(checkErr, IsNil)
			c.Assert(err, NotNil)
			c.Assert(err.Error(), Equals, "[ddl:12]cancelled DDL job")
			break LOOP
		case <-ticker.C:
			if times >= 10 {
				break
			}
			step := 10
			// delete some rows, and add some data
			for i := count; i < count+step; i++ {
				n := rand.Intn(count)
				s.mustExec(c, "delete from t1 where c1 = ?", n)
				s.mustExec(c, "insert into t1 values (?, ?, ?)", i+10, i, i)
			}
			count += step
			times++
		}
	}

	t := s.testGetTable(c, "t1")
	for _, tidx := range t.Indices() {
		c.Assert(strings.EqualFold(tidx.Meta().Name.L, "c3_index"), IsFalse)
	}

	ctx := s.s.(sessionctx.Context)
	idx := tables.NewIndex(t.Meta(), c3IdxInfo)
	checkDelRangeDone(c, ctx, idx)

	s.mustExec(c, "drop table t1")
	ddl.ReorgWaitTimeout = oldReorgWaitTimeout
}

// TestCancelAddIndex1 tests canceling ddl job when the add index worker is not started.
func (s *testDBSuite) TestCancelAddIndex1(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.mustExec(c, "use test_db")
	s.mustExec(c, "drop table if exists t")
	s.mustExec(c, "create table t(c1 int, c2 int)")
	defer s.mustExec(c, "drop table t;")
	for i := 0; i < 50; i++ {
		s.mustExec(c, "insert into t values (?, ?)", i, i)
	}
	var checkErr error
	oldReorgWaitTimeout := ddl.ReorgWaitTimeout
	ddl.ReorgWaitTimeout = 50 * time.Millisecond
	defer func() { ddl.ReorgWaitTimeout = oldReorgWaitTimeout }()
	hook := &ddl.TestDDLCallback{}
	hook.OnJobRunBeforeExported = func(job *model.Job) {
		if job.Type == model.ActionAddIndex && job.State == model.JobStateRunning && job.SchemaState == model.StateWriteReorganization && job.SnapshotVer == 0 {
			jobIDs := []int64{job.ID}
			hookCtx := mock.NewContext()
			hookCtx.Store = s.store
			err := hookCtx.NewTxn()
			if err != nil {
				checkErr = errors.Trace(err)
				return
			}
			txn, err := hookCtx.Txn(true)
			if err != nil {
				checkErr = errors.Trace(err)
				return
			}
			errs, err := admin.CancelJobs(txn, jobIDs)
			if err != nil {
				checkErr = errors.Trace(err)
				return
			}
			if errs[0] != nil {
				checkErr = errors.Trace(errs[0])
				return
			}
			checkErr = txn.Commit(context.Background())
		}
	}
	s.dom.DDL().SetHook(hook)
	rs, err := s.tk.Exec("alter table t add index idx_c2(c2)")
	if rs != nil {
		rs.Close()
	}
	c.Assert(checkErr, IsNil)
	c.Assert(err, NotNil)
	c.Assert(err.Error(), Equals, "[ddl:12]cancelled DDL job")
	s.dom.DDL().SetHook(&ddl.TestDDLCallback{})
	t := s.testGetTable(c, "t")
	for _, idx := range t.Indices() {
		c.Assert(strings.EqualFold(idx.Meta().Name.L, "idx_c2"), IsFalse)
	}

	s.mustExec(c, "alter table t add index idx_c2(c2)")
	s.mustExec(c, "alter table t drop index idx_c2")
}

func (s *testDBSuite) TestAddAnonymousIndex(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.mustExec(c, "create table t_anonymous_index (c1 int, c2 int, C3 int)")
	s.mustExec(c, "alter table t_anonymous_index add index (c1, c2)")
	// for dropping empty index
	_, err := s.tk.Exec("alter table t_anonymous_index drop index")
	c.Assert(err, NotNil)
	// The index name is c1 when adding index (c1, c2).
	s.mustExec(c, "alter table t_anonymous_index drop index c1")
	t := s.testGetTable(c, "t_anonymous_index")
	c.Assert(t.Indices(), HasLen, 0)
	// for adding some indices that the first column name is c1
	s.mustExec(c, "alter table t_anonymous_index add index (c1)")
	_, err = s.tk.Exec("alter table t_anonymous_index add index c1 (c2)")
	c.Assert(err, NotNil)
	t = s.testGetTable(c, "t_anonymous_index")
	c.Assert(t.Indices(), HasLen, 1)
	idx := t.Indices()[0].Meta().Name.L
	c.Assert(idx, Equals, "c1")
	// The MySQL will be a warning.
	s.mustExec(c, "alter table t_anonymous_index add index c1_3 (c1)")
	s.mustExec(c, "alter table t_anonymous_index add index (c1, c2, C3)")
	// The MySQL will be a warning.
	s.mustExec(c, "alter table t_anonymous_index add index (c1)")
	t = s.testGetTable(c, "t_anonymous_index")
	c.Assert(t.Indices(), HasLen, 4)
	s.mustExec(c, "alter table t_anonymous_index drop index c1")
	s.mustExec(c, "alter table t_anonymous_index drop index c1_2")
	s.mustExec(c, "alter table t_anonymous_index drop index c1_3")
	s.mustExec(c, "alter table t_anonymous_index drop index c1_4")
	// for case insensitive
	s.mustExec(c, "alter table t_anonymous_index add index (C3)")
	s.mustExec(c, "alter table t_anonymous_index drop index c3")
	s.mustExec(c, "alter table t_anonymous_index add index c3 (C3)")
	s.mustExec(c, "alter table t_anonymous_index drop index C3")
	// for anonymous index with column name `primary`
	s.mustExec(c, "create table t_primary (`primary` int, key (`primary`))")
	t = s.testGetTable(c, "t_primary")
	c.Assert(t.Indices()[0].Meta().Name.String(), Equals, "primary_2")
	s.mustExec(c, "create table t_primary_2 (`primary` int, key primary_2 (`primary`), key (`primary`))")
	t = s.testGetTable(c, "t_primary_2")
	c.Assert(t.Indices()[0].Meta().Name.String(), Equals, "primary_2")
	c.Assert(t.Indices()[1].Meta().Name.String(), Equals, "primary_3")
	s.mustExec(c, "create table t_primary_3 (`primary_2` int, key(`primary_2`), `primary` int, key(`primary`));")
	t = s.testGetTable(c, "t_primary_3")
	c.Assert(t.Indices()[0].Meta().Name.String(), Equals, "primary_2")
	c.Assert(t.Indices()[1].Meta().Name.String(), Equals, "primary_3")
}

// TestModifyColumnAfterAddIndex Issue 5134
func (s *testDBSuite) TestModifyColumnAfterAddIndex(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.mustExec(c, "create table city (city VARCHAR(2) KEY);")
	s.mustExec(c, "alter table city change column city city varchar(50);")
	s.mustExec(c, `insert into city values ("abc"), ("abd");`)
}

func (s *testDBSuite) testAlterLock(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.mustExec(c, "create table t_index_lock (c1 int, c2 int, C3 int)")
	s.mustExec(c, "alter table t_indx_lock add index (c1, c2), lock=none")
}

func (s *testDBSuite) TestAddMultiColumnsIndex(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.tk.MustExec("drop database if exists tidb;")
	s.tk.MustExec("create database tidb;")
	s.tk.MustExec("use tidb;")
	s.tk.MustExec("create table tidb.test (a int auto_increment primary key, b int);")
	s.tk.MustExec("insert tidb.test values (1, 1);")
	s.tk.MustExec("update tidb.test set b = b + 1 where a = 1;")
	s.tk.MustExec("insert into tidb.test values (2, 2);")
	// Test that the b value is nil.
	s.tk.MustExec("insert into tidb.test (a) values (3);")
	s.tk.MustExec("insert into tidb.test values (4, 4);")
	// Test that the b value is nil again.
	s.tk.MustExec("insert into tidb.test (a) values (5);")
	s.tk.MustExec("insert tidb.test values (6, 6);")
	s.tk.MustExec("alter table tidb.test add index idx1 (a, b);")
	s.tk.MustExec("admin check table test")
}

func (s *testDBSuite) TestAddIndex(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.tk.MustExec("create table test_add_index (c1 bigint, c2 bigint, c3 bigint, primary key(c1))")

	done := make(chan error, 1)
	start := -10
	num := defaultBatchSize
	// first add some rows
	for i := start; i < num; i++ {
		sql := fmt.Sprintf("insert into test_add_index values (%d, %d, %d)", i, i, i)
		s.mustExec(c, sql)
	}
	// Add some discrete rows.
	maxBatch := 20
	batchCnt := 100
	otherKeys := make([]int, 0, batchCnt*maxBatch)
	// Make sure there are no duplicate keys.
	base := defaultBatchSize * 20
	for i := 1; i < batchCnt; i++ {
		n := base + i*defaultBatchSize + i
		for j := 0; j < rand.Intn(maxBatch); j++ {
			n += j
			sql := fmt.Sprintf("insert into test_add_index values (%d, %d, %d)", n, n, n)
			s.mustExec(c, sql)
			otherKeys = append(otherKeys, n)
		}
	}
	// Encounter the value of math.MaxInt64 in middle of
	v := math.MaxInt64 - defaultBatchSize/2
	sql := fmt.Sprintf("insert into test_add_index values (%d, %d, %d)", v, v, v)
	s.mustExec(c, sql)
	otherKeys = append(otherKeys, v)

	sessionExecInGoroutine(c, s.store, "create index c3_index on test_add_index (c3)", done)

	deletedKeys := make(map[int]struct{})

	ticker := time.NewTicker(s.lease / 2)
	defer ticker.Stop()
LOOP:
	for {
		select {
		case err := <-done:
			if err == nil {
				break LOOP
			}
			c.Assert(err, IsNil, Commentf("err:%v", errors.ErrorStack(err)))
		case <-ticker.C:
			// When the server performance is particularly poor,
			// the adding index operation can not be completed.
			// So here is a limit to the number of rows inserted.
			if num > defaultBatchSize*10 {
				break
			}
			step := 10
			// delete some rows, and add some data
			for i := num; i < num+step; i++ {
				n := rand.Intn(num)
				deletedKeys[n] = struct{}{}
				sql := fmt.Sprintf("delete from test_add_index where c1 = %d", n)
				s.mustExec(c, sql)
				sql = fmt.Sprintf("insert into test_add_index values (%d, %d, %d)", i, i, i)
				s.mustExec(c, sql)
			}
			num += step
		}
	}

	// get exists keys
	keys := make([]int, 0, num)
	for i := start; i < num; i++ {
		if _, ok := deletedKeys[i]; ok {
			continue
		}
		keys = append(keys, i)
	}
	keys = append(keys, otherKeys...)

	// test index key
	expectedRows := make([][]interface{}, 0, len(keys))
	for _, key := range keys {
		expectedRows = append(expectedRows, []interface{}{key})
	}
	rows := s.mustQuery(c, fmt.Sprintf("select c1 from test_add_index where c3 >= %d", start))
	matchRows(c, rows, expectedRows)

	// test index range
	for i := 0; i < 100; i++ {
		index := rand.Intn(len(keys) - 3)
		rows := s.mustQuery(c, "select c1 from test_add_index where c3 >= ? limit 3", keys[index])
		matchRows(c, rows, [][]interface{}{{keys[index]}, {keys[index+1]}, {keys[index+2]}})
	}

	// TODO: Support explain in future.
	// rows := s.mustQuery(c, "explain select c1 from test_add_index where c3 >= 100")

	// ay := dumpRows(c, rows)
	// c.Assert(strings.Contains(fmt.Sprintf("%v", ay), "c3_index"), IsTrue)

	// get all row handles
	ctx := s.s.(sessionctx.Context)
	c.Assert(ctx.NewTxn(), IsNil)
	t := s.testGetTable(c, "test_add_index")
	handles := make(map[int64]struct{})
	startKey := t.RecordKey(math.MinInt64)
	err := t.IterRecords(ctx, startKey, t.Cols(),
		func(h int64, data []types.Datum, cols []*table.Column) (bool, error) {
			handles[h] = struct{}{}
			return true, nil
		})
	c.Assert(err, IsNil)

	// check in index
	var nidx table.Index
	for _, tidx := range t.Indices() {
		if tidx.Meta().Name.L == "c3_index" {
			nidx = tidx
			break
		}
	}
	// Make sure there is index with name c3_index.
	c.Assert(nidx, NotNil)
	c.Assert(nidx.Meta().ID, Greater, int64(0))
	txn, err := ctx.Txn(true)
	c.Assert(err, IsNil)
	txn.Rollback()

	c.Assert(ctx.NewTxn(), IsNil)
	defer txn.Rollback()

	it, err := nidx.SeekFirst(txn)
	c.Assert(err, IsNil)
	defer it.Close()

	for {
		_, h, err := it.Next()
		if terror.ErrorEqual(err, io.EOF) {
			break
		}

		c.Assert(err, IsNil)
		_, ok := handles[h]
		c.Assert(ok, IsTrue)
		delete(handles, h)
	}
	c.Assert(handles, HasLen, 0)

	s.tk.MustExec("drop table test_add_index")
}

func (s *testDBSuite) TestDropIndex(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.tk.MustExec("drop table if exists test_drop_index")
	s.tk.MustExec("create table test_drop_index (c1 int, c2 int, c3 int, primary key(c1))")
	s.tk.MustExec("create index c3_index on test_drop_index (c3)")
	done := make(chan error, 1)
	s.mustExec(c, "delete from test_drop_index")

	num := 100
	//  add some rows
	for i := 0; i < num; i++ {
		s.mustExec(c, "insert into test_drop_index values (?, ?, ?)", i, i, i)
	}
	t := s.testGetTable(c, "test_drop_index")
	var c3idx table.Index
	for _, tidx := range t.Indices() {
		if tidx.Meta().Name.L == "c3_index" {
			c3idx = tidx
			break
		}
	}
	c.Assert(c3idx, NotNil)

	sessionExecInGoroutine(c, s.store, "drop index c3_index on test_drop_index", done)

	ticker := time.NewTicker(s.lease / 2)
	defer ticker.Stop()
LOOP:
	for {
		select {
		case err := <-done:
			if err == nil {
				break LOOP
			}
			c.Assert(err, IsNil, Commentf("err:%v", errors.ErrorStack(err)))
		case <-ticker.C:
			step := 10
			// delete some rows, and add some data
			for i := num; i < num+step; i++ {
				n := rand.Intn(num)
				s.mustExec(c, "update test_drop_index set c2 = 1 where c1 = ?", n)
				s.mustExec(c, "insert into test_drop_index values (?, ?, ?)", i, i, i)
			}
			num += step
		}
	}

	rows := s.mustQuery(c, "explain select c1 from test_drop_index where c3 >= 0")
	c.Assert(strings.Contains(fmt.Sprintf("%v", rows), "c3_index"), IsFalse)

	// check in index, must no index in kv
	ctx := s.s.(sessionctx.Context)

	// Make sure there is no index with name c3_index.
	t = s.testGetTable(c, "test_drop_index")
	var nidx table.Index
	for _, tidx := range t.Indices() {
		if tidx.Meta().Name.L == "c3_index" {
			nidx = tidx
			break
		}
	}
	c.Assert(nidx, IsNil)

	idx := tables.NewIndex(t.Meta(), c3idx.Meta())
	checkDelRangeDone(c, ctx, idx)
	s.tk.MustExec("drop table test_drop_index")
}

func checkDelRangeDone(c *C, ctx sessionctx.Context, idx table.Index) {
	startTime := time.Now()
	f := func() map[int64]struct{} {
		handles := make(map[int64]struct{})

		c.Assert(ctx.NewTxn(), IsNil)
		txn, err := ctx.Txn(true)
		c.Assert(err, IsNil)
		defer txn.Rollback()

		txn, err = ctx.Txn(true)
		c.Assert(err, IsNil)
		it, err := idx.SeekFirst(txn)
		c.Assert(err, IsNil)
		defer it.Close()

		for {
			_, h, err := it.Next()
			if terror.ErrorEqual(err, io.EOF) {
				break
			}

			c.Assert(err, IsNil)
			handles[h] = struct{}{}
		}
		return handles
	}

	var handles map[int64]struct{}
	for i := 0; i < waitForCleanDataRound; i++ {
		handles = f()
		if len(handles) != 0 {
			time.Sleep(waitForCleanDataInterval)
		} else {
			break
		}
	}
	c.Assert(handles, HasLen, 0, Commentf("take time %v", time.Since(startTime)))
}

func (s *testDBSuite) TestAddIndexWithDupCols(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	err1 := infoschema.ErrColumnExists.GenByArgs("b")
	err2 := infoschema.ErrColumnExists.GenByArgs("B")

	s.tk.MustExec("create table test_add_index_with_dup (a int, b int)")
	_, err := s.tk.Exec("create index c on test_add_index_with_dup(b, a, b)")
	c.Check(err1.Equal(err), Equals, true)

	_, err = s.tk.Exec("create index c on test_add_index_with_dup(b, a, B)")
	c.Check(err2.Equal(err), Equals, true)

	_, err = s.tk.Exec("alter table test_add_index_with_dup add index c (b, a, b)")
	c.Check(err1.Equal(err), Equals, true)

	_, err = s.tk.Exec("alter table test_add_index_with_dup add index c (b, a, B)")
	c.Check(err2.Equal(err), Equals, true)

	s.tk.MustExec("drop table test_add_index_with_dup")
}

func (s *testDBSuite) showColumns(c *C, tableName string) [][]interface{} {
	return s.mustQuery(c, fmt.Sprintf("show columns from %s", tableName))
}

func (s *testDBSuite) TestIssue2293(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.tk.MustExec("create table t_issue_2293 (a int)")
	sql := "alter table t_issue_2293 add b int not null default 'a'"
	s.testErrorCode(c, sql, tmysql.ErrInvalidDefault)
	s.tk.MustExec("insert into t_issue_2293 value(1)")
	s.tk.MustQuery("select * from t_issue_2293").Check(testkit.Rows("1"))
}

func (s *testDBSuite) TestIssue6101(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.tk.MustExec("create table t1 (quantity decimal(2) unsigned);")
	_, err := s.tk.Exec("insert into t1 values (500), (-500), (~0), (-1);")
	terr := errors.Trace(err).(*errors.Err).Cause().(*terror.Error)
	c.Assert(terr.Code(), Equals, terror.ErrCode(tmysql.ErrWarnDataOutOfRange))
	s.tk.MustExec("drop table t1")

	s.tk.MustExec("set sql_mode=''")
	s.tk.MustExec("create table t1 (quantity decimal(2) unsigned);")
	s.tk.MustExec("insert into t1 values (500), (-500), (~0), (-1);")
	s.tk.MustQuery("select * from t1").Check(testkit.Rows("99", "0", "99", "0"))
	s.tk.MustExec("drop table t1")
}

func (s *testDBSuite) TestCreateIndexType(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	sql := `CREATE TABLE test_index (
		price int(5) DEFAULT '0' NOT NULL,
		area varchar(40) DEFAULT '' NOT NULL,
		type varchar(40) DEFAULT '' NOT NULL,
		transityes set('a','b'),
		shopsyes enum('Y','N') DEFAULT 'Y' NOT NULL,
		schoolsyes enum('Y','N') DEFAULT 'Y' NOT NULL,
		petsyes enum('Y','N') DEFAULT 'Y' NOT NULL,
		KEY price (price,area,type,transityes,shopsyes,schoolsyes,petsyes));`
	s.tk.MustExec(sql)
}

func (s *testDBSuite) TestIssue3833(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.tk.MustExec("create table issue3833 (b char(0))")
	s.testErrorCode(c, "create index idx on issue3833 (b)", tmysql.ErrWrongKeyColumn)
	s.testErrorCode(c, "alter table issue3833 add index idx (b)", tmysql.ErrWrongKeyColumn)
	s.testErrorCode(c, "create table issue3833_2 (b char(0), index (b))", tmysql.ErrWrongKeyColumn)
}

func (s *testDBSuite) TestColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.tk.MustExec("create table t2 (c1 int, c2 int, c3 int)")
	s.testAddColumn(c)
	s.testDropColumn(c)
	s.testDropColumn2(c)
	s.tk.MustExec("drop table t2")
}

func sessionExec(c *C, s kv.Storage, sql string) {
	se, err := session.CreateSession4Test(s)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test_db")
	c.Assert(err, IsNil)
	rs, err := se.Execute(context.Background(), sql)
	c.Assert(err, IsNil, Commentf("err:%v", errors.ErrorStack(err)))
	c.Assert(rs, IsNil)
	se.Close()
}

func sessionExecInGoroutine(c *C, s kv.Storage, sql string, done chan error) {
	execMultiSQLInGoroutine(c, s, []string{sql}, done)
}

func execMultiSQLInGoroutine(c *C, s kv.Storage, multiSQL []string, done chan error) {
	go func() {
		se, err := session.CreateSession4Test(s)
		if err != nil {
			done <- errors.Trace(err)
			return
		}
		defer se.Close()
		_, err = se.Execute(context.Background(), "use test_db")
		if err != nil {
			done <- errors.Trace(err)
			return
		}
		for _, sql := range multiSQL {
			rs, err := se.Execute(context.Background(), sql)
			if err != nil {
				done <- errors.Trace(err)
				return
			}
			if rs != nil {
				done <- errors.Errorf("RecordSet should be empty.")
				return
			}
			done <- nil
		}
	}()
}

func (s *testDBSuite) testAddColumn(c *C) {
	done := make(chan error, 1)

	num := defaultBatchSize + 10
	// add some rows
	for i := 0; i < num; i++ {
		s.mustExec(c, "insert into t2 values (?, ?, ?)", i, i, i)
	}

	sessionExecInGoroutine(c, s.store, "alter table t2 add column c4 int default -1", done)

	ticker := time.NewTicker(s.lease / 2)
	defer ticker.Stop()
	step := 10
LOOP:
	for {
		select {
		case err := <-done:
			if err == nil {
				break LOOP
			}
			c.Assert(err, IsNil, Commentf("err:%v", errors.ErrorStack(err)))
		case <-ticker.C:
			// delete some rows, and add some data
			for i := num; i < num+step; i++ {
				n := rand.Intn(num)
				s.tk.MustExec("begin")
				s.tk.MustExec("delete from t2 where c1 = ?", n)
				s.tk.MustExec("commit")

				// Make sure that statement of insert and show use the same infoSchema.
				s.tk.MustExec("begin")
				_, err := s.tk.Exec("insert into t2 values (?, ?, ?)", i, i, i)
				if err != nil {
					// if err is failed, the column number must be 4 now.
					values := s.showColumns(c, "t2")
					c.Assert(values, HasLen, 4, Commentf("err:%v", errors.ErrorStack(err)))
				}
				s.tk.MustExec("commit")
			}
			num += step
		}
	}

	// add data, here c4 must exist
	for i := num; i < num+step; i++ {
		s.tk.MustExec("insert into t2 values (?, ?, ?, ?)", i, i, i, i)
	}

	rows := s.mustQuery(c, "select count(c4) from t2")
	c.Assert(rows, HasLen, 1)
	c.Assert(rows[0], HasLen, 1)
	count, err := strconv.ParseInt(rows[0][0].(string), 10, 64)
	c.Assert(err, IsNil)
	c.Assert(count, Greater, int64(0))

	rows = s.mustQuery(c, "select count(c4) from t2 where c4 = -1")
	matchRows(c, rows, [][]interface{}{{count - int64(step)}})

	for i := num; i < num+step; i++ {
		rows = s.mustQuery(c, "select c4 from t2 where c4 = ?", i)
		matchRows(c, rows, [][]interface{}{{i}})
	}

	ctx := s.s.(sessionctx.Context)
	t := s.testGetTable(c, "t2")
	i := 0
	j := 0
	ctx.NewTxn()
	defer func() {
		if txn, err1 := ctx.Txn(true); err1 == nil {
			txn.Rollback()
		}
	}()
	err = t.IterRecords(ctx, t.FirstKey(), t.Cols(),
		func(h int64, data []types.Datum, cols []*table.Column) (bool, error) {
			i++
			// c4 must be -1 or > 0
			v, err1 := data[3].ToInt64(ctx.GetSessionVars().StmtCtx)
			c.Assert(err1, IsNil)
			if v == -1 {
				j++
			} else {
				c.Assert(v, Greater, int64(0))
			}
			return true, nil
		})
	c.Assert(err, IsNil)
	c.Assert(i, Equals, int(count))
	c.Assert(i, LessEqual, num+step)
	c.Assert(j, Equals, int(count)-step)

	// for modifying columns after adding columns
	s.tk.MustExec("alter table t2 modify c4 int default 11")
	for i := num + step; i < num+step+10; i++ {
		s.mustExec(c, "insert into t2 values (?, ?, ?, ?)", i, i, i, i)
	}
	rows = s.mustQuery(c, "select count(c4) from t2 where c4 = -1")
	matchRows(c, rows, [][]interface{}{{count - int64(step)}})
}

func (s *testDBSuite) testDropColumn(c *C) {
	done := make(chan error, 1)
	s.mustExec(c, "delete from t2")

	num := 100
	// add some rows
	for i := 0; i < num; i++ {
		s.mustExec(c, "insert into t2 values (?, ?, ?, ?)", i, i, i, i)
	}

	// get c4 column id
	sessionExecInGoroutine(c, s.store, "alter table t2 drop column c4", done)

	ticker := time.NewTicker(s.lease / 2)
	defer ticker.Stop()
	step := 10
LOOP:
	for {
		select {
		case err := <-done:
			if err == nil {
				break LOOP
			}
			c.Assert(err, IsNil, Commentf("err:%v", errors.ErrorStack(err)))
		case <-ticker.C:
			// delete some rows, and add some data
			for i := num; i < num+step; i++ {
				// Make sure that statement of insert and show use the same infoSchema.
				s.tk.MustExec("begin")
				_, err := s.tk.Exec("insert into t2 values (?, ?, ?)", i, i, i)
				if err != nil {
					// If executing is failed, the column number must be 4 now.
					values := s.showColumns(c, "t2")
					c.Assert(values, HasLen, 4, Commentf("err:%v", errors.ErrorStack(err)))
				}
				s.tk.MustExec("commit")
			}
			num += step
		}
	}

	// add data, here c4 must not exist
	for i := num; i < num+step; i++ {
		s.mustExec(c, "insert into t2 values (?, ?, ?)", i, i, i)
	}

	rows := s.mustQuery(c, "select count(*) from t2")
	c.Assert(rows, HasLen, 1)
	c.Assert(rows[0], HasLen, 1)
	count, err := strconv.ParseInt(rows[0][0].(string), 10, 64)
	c.Assert(err, IsNil)
	c.Assert(count, Greater, int64(0))
}

// testDropColumn2 is for inserting value with a to-be-dropped column when do drop column.
// Column info from schema in build-insert-plan should be public only,
// otherwise they will not be consist with Table.Col(), then the server will panic.
func (s *testDBSuite) testDropColumn2(c *C) {
	num := 100
	dmlDone := make(chan error, num)
	ddlDone := make(chan error, num)
	s.mustExec(c, "delete from t2")

	multiDDL := make([]string, 0, num)
	for i := 0; i < num/2; i++ {
		multiDDL = append(multiDDL, "alter table t2 add column c4 int", "alter table t2 drop column c4")
	}
	execMultiSQLInGoroutine(c, s.store, multiDDL, ddlDone)
	for i := 0; i < num; i++ {
		sessionExecInGoroutine(c, s.store, "insert into t2 set c1 = 1, c2 = 1, c3 = 1, c4 = 1", dmlDone)
	}
	for i := 0; i < num; i++ {
		select {
		case err := <-ddlDone:
			c.Assert(err, IsNil, Commentf("err:%v", errors.ErrorStack(err)))
		}
	}
}

func (s *testDBSuite) TestPrimaryKey(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.mustExec(c, "create table primary_key_test (a int, b varchar(10))")
	_, err := s.tk.Exec("alter table primary_key_test add primary key(a)")
	c.Assert(ddl.ErrUnsupportedModifyPrimaryKey.Equal(err), IsTrue)
	_, err = s.tk.Exec("alter table primary_key_test drop primary key")
	c.Assert(ddl.ErrUnsupportedModifyPrimaryKey.Equal(err), IsTrue)
}

func (s *testDBSuite) TestChangeColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.mustExec(c, "create table t3 (a int default '0', b varchar(10), d int not null default '0')")
	s.mustExec(c, "insert into t3 set b = 'a'")
	s.tk.MustQuery("select a from t3").Check(testkit.Rows("0"))
	s.mustExec(c, "alter table t3 change a aa bigint")
	s.mustExec(c, "insert into t3 set b = 'b'")
	s.tk.MustQuery("select aa from t3").Check(testkit.Rows("0", "<nil>"))
	// for no default flag
	s.mustExec(c, "alter table t3 change d dd bigint not null")
	ctx := s.tk.Se.(sessionctx.Context)
	is := domain.GetDomain(ctx).InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("t3"))
	c.Assert(err, IsNil)
	tblInfo := tbl.Meta()
	colD := tblInfo.Columns[2]
	hasNoDefault := tmysql.HasNoDefaultValueFlag(colD.Flag)
	c.Assert(hasNoDefault, IsTrue)
	// for the following definitions: 'not null', 'null', 'default value' and 'comment'
	s.mustExec(c, "alter table t3 change b b varchar(20) null default 'c' comment 'my comment'")
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("t3"))
	c.Assert(err, IsNil)
	tblInfo = tbl.Meta()
	colB := tblInfo.Columns[1]
	c.Assert(colB.Comment, Equals, "my comment")
	hasNotNull := tmysql.HasNotNullFlag(colB.Flag)
	c.Assert(hasNotNull, IsFalse)
	s.mustExec(c, "insert into t3 set aa = 3, dd = 5")
	s.tk.MustQuery("select b from t3").Check(testkit.Rows("a", "b", "c"))
	// for timestamp
	s.mustExec(c, "alter table t3 add column c timestamp not null")
	s.mustExec(c, "alter table t3 change c c timestamp null default '2017-02-11' comment 'col c comment' on update current_timestamp")
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("t3"))
	c.Assert(err, IsNil)
	tblInfo = tbl.Meta()
	colC := tblInfo.Columns[3]
	c.Assert(colC.Comment, Equals, "col c comment")
	hasNotNull = tmysql.HasNotNullFlag(colC.Flag)
	c.Assert(hasNotNull, IsFalse)
	// for enum
	s.mustExec(c, "alter table t3 add column en enum('a', 'b', 'c') not null default 'a'")

	// for failing tests
	sql := "alter table t3 change aa a bigint default ''"
	s.testErrorCode(c, sql, tmysql.ErrInvalidDefault)
	sql = "alter table t3 change a testx.t3.aa bigint"
	s.testErrorCode(c, sql, tmysql.ErrWrongDBName)
	sql = "alter table t3 change t.a aa bigint"
	s.testErrorCode(c, sql, tmysql.ErrWrongTableName)
	sql = "alter table t3 change aa a bigint not null"
	s.testErrorCode(c, sql, tmysql.ErrUnknown)
	sql = "alter table t3 modify en enum('a', 'z', 'b', 'c') not null default 'a'"
	s.testErrorCode(c, sql, tmysql.ErrUnknown)
	// Rename to an existing column.
	s.mustExec(c, "alter table t3 add column a bigint")
	sql = "alter table t3 change aa a bigint"
	s.testErrorCode(c, sql, tmysql.ErrDupFieldName)

	s.tk.MustExec("drop table t3")
}

func (s *testDBSuite) TestAlterColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.mustExec(c, "create table test_alter_column (a int default 111, b varchar(8), c varchar(8) not null, d timestamp on update current_timestamp)")
	s.mustExec(c, "insert into test_alter_column set b = 'a', c = 'aa'")
	s.tk.MustQuery("select a from test_alter_column").Check(testkit.Rows("111"))
	ctx := s.tk.Se.(sessionctx.Context)
	is := domain.GetDomain(ctx).InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("test_alter_column"))
	c.Assert(err, IsNil)
	tblInfo := tbl.Meta()
	colA := tblInfo.Columns[0]
	hasNoDefault := tmysql.HasNoDefaultValueFlag(colA.Flag)
	c.Assert(hasNoDefault, IsFalse)
	s.mustExec(c, "alter table test_alter_column alter column a set default 222")
	s.mustExec(c, "insert into test_alter_column set b = 'b', c = 'bb'")
	s.tk.MustQuery("select a from test_alter_column").Check(testkit.Rows("111", "222"))
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("test_alter_column"))
	c.Assert(err, IsNil)
	tblInfo = tbl.Meta()
	colA = tblInfo.Columns[0]
	hasNoDefault = tmysql.HasNoDefaultValueFlag(colA.Flag)
	c.Assert(hasNoDefault, IsFalse)
	s.mustExec(c, "alter table test_alter_column alter column b set default null")
	s.mustExec(c, "insert into test_alter_column set c = 'cc'")
	s.tk.MustQuery("select b from test_alter_column").Check(testkit.Rows("a", "b", "<nil>"))
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("test_alter_column"))
	c.Assert(err, IsNil)
	tblInfo = tbl.Meta()
	colC := tblInfo.Columns[2]
	hasNoDefault = tmysql.HasNoDefaultValueFlag(colC.Flag)
	c.Assert(hasNoDefault, IsTrue)
	s.mustExec(c, "alter table test_alter_column alter column c set default 'xx'")
	s.mustExec(c, "insert into test_alter_column set a = 123")
	s.tk.MustQuery("select c from test_alter_column").Check(testkit.Rows("aa", "bb", "cc", "xx"))
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test_db"), model.NewCIStr("test_alter_column"))
	c.Assert(err, IsNil)
	tblInfo = tbl.Meta()
	colC = tblInfo.Columns[2]
	hasNoDefault = tmysql.HasNoDefaultValueFlag(colC.Flag)
	c.Assert(hasNoDefault, IsFalse)
	// TODO: After fix issue 2606.
	// s.mustExec(c, "alter table test_alter_column alter column d set default null")
	s.mustExec(c, "alter table test_alter_column alter column a drop default")
	s.mustExec(c, "insert into test_alter_column set b = 'd', c = 'dd'")
	s.tk.MustQuery("select a from test_alter_column").Check(testkit.Rows("111", "222", "222", "123", "<nil>"))

	// for failing tests
	sql := "alter table db_not_exist.test_alter_column alter column b set default 'c'"
	s.testErrorCode(c, sql, tmysql.ErrNoSuchTable)
	sql = "alter table test_not_exist alter column b set default 'c'"
	s.testErrorCode(c, sql, tmysql.ErrNoSuchTable)
	sql = "alter table test_alter_column alter column col_not_exist set default 'c'"
	s.testErrorCode(c, sql, tmysql.ErrBadField)
	sql = "alter table test_alter_column alter column c set default null"
	s.testErrorCode(c, sql, tmysql.ErrInvalidDefault)

	// The followings tests whether adding constraints via change / modify column
	// is forbidden as expected.
	s.mustExec(c, "drop table if exists mc")
	s.mustExec(c, "create table mc(a int key, b int, c int)")
	_, err = s.tk.Exec("alter table mc modify column a int key") // Adds a new primary key
	c.Assert(err, NotNil)
	_, err = s.tk.Exec("alter table mc modify column c int unique") // Adds a new unique key
	c.Assert(err, NotNil)
	result := s.tk.MustQuery("show create table mc")
	createSQL := result.Rows()[0][1]
	expected := "CREATE TABLE `mc` (\n  `a` int(11) NOT NULL,\n  `b` int(11) DEFAULT NULL,\n  `c` int(11) DEFAULT NULL,\n  PRIMARY KEY (`a`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin"
	c.Assert(createSQL, Equals, expected)

	// Change / modify column should preserve index options.
	s.mustExec(c, "drop table if exists mc")
	s.mustExec(c, "create table mc(a int key, b int, c int unique)")
	s.mustExec(c, "alter table mc modify column a bigint") // NOT NULL & PRIMARY KEY should be preserved
	s.mustExec(c, "alter table mc modify column b bigint")
	s.mustExec(c, "alter table mc modify column c bigint") // Unique should be preserved
	result = s.tk.MustQuery("show create table mc")
	createSQL = result.Rows()[0][1]
	expected = "CREATE TABLE `mc` (\n  `a` bigint(20) NOT NULL,\n  `b` bigint(20) DEFAULT NULL,\n  `c` bigint(20) DEFAULT NULL,\n  PRIMARY KEY (`a`),\n  UNIQUE KEY `c` (`c`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin"
	c.Assert(createSQL, Equals, expected)

	// Dropping or keeping auto_increment is allowed, however adding is not allowed.
	s.mustExec(c, "drop table if exists mc")
	s.mustExec(c, "create table mc(a int key auto_increment, b int)")
	s.mustExec(c, "alter table mc modify column a bigint auto_increment") // Keeps auto_increment
	result = s.tk.MustQuery("show create table mc")
	createSQL = result.Rows()[0][1]
	expected = "CREATE TABLE `mc` (\n  `a` bigint(20) NOT NULL AUTO_INCREMENT,\n  `b` int(11) DEFAULT NULL,\n  PRIMARY KEY (`a`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin"
	s.mustExec(c, "alter table mc modify column a bigint") // Drops auto_increment
	result = s.tk.MustQuery("show create table mc")
	createSQL = result.Rows()[0][1]
	expected = "CREATE TABLE `mc` (\n  `a` bigint(20) NOT NULL,\n  `b` int(11) DEFAULT NULL,\n  PRIMARY KEY (`a`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin"
	c.Assert(createSQL, Equals, expected)
	_, err = s.tk.Exec("alter table mc modify column a bigint auto_increment") // Adds auto_increment should throw error
	c.Assert(err, NotNil)
}

func (s *testDBSuite) mustExec(c *C, query string, args ...interface{}) {
	s.tk.MustExec(query, args...)
}

func (s *testDBSuite) mustQuery(c *C, query string, args ...interface{}) [][]interface{} {
	r := s.tk.MustQuery(query, args...)
	return r.Rows()
}

func matchRows(c *C, rows [][]interface{}, expected [][]interface{}) {
	c.Assert(len(rows), Equals, len(expected), Commentf("got %v, expected %v", rows, expected))
	for i := range rows {
		match(c, rows[i], expected[i]...)
	}
}

func match(c *C, row []interface{}, expected ...interface{}) {
	c.Assert(len(row), Equals, len(expected))
	for i := range row {
		got := fmt.Sprintf("%v", row[i])
		need := fmt.Sprintf("%v", expected[i])
		c.Assert(got, Equals, need)
	}
}

func (s *testDBSuite) TestUpdateMultipleTable(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table t1 (c1 int, c2 int)")
	tk.MustExec("insert t1 values (1, 1), (2, 2)")
	tk.MustExec("create table t2 (c1 int, c2 int)")
	tk.MustExec("insert t2 values (1, 3), (2, 5)")
	ctx := tk.Se.(sessionctx.Context)
	dom := domain.GetDomain(ctx)
	is := dom.InfoSchema()
	db, ok := is.SchemaByName(model.NewCIStr("test"))
	c.Assert(ok, IsTrue)
	t1Tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t1"))
	c.Assert(err, IsNil)
	t1Info := t1Tbl.Meta()

	// Add a new column in write only state.
	newColumn := &model.ColumnInfo{
		ID:                 100,
		Name:               model.NewCIStr("c3"),
		Offset:             2,
		DefaultValue:       9,
		OriginDefaultValue: 9,
		FieldType:          *types.NewFieldType(tmysql.TypeLonglong),
		State:              model.StateWriteOnly,
	}
	t1Info.Columns = append(t1Info.Columns, newColumn)

	kv.RunInNewTxn(s.store, false, func(txn kv.Transaction) error {
		m := meta.NewMeta(txn)
		_, err = m.GenSchemaVersion()
		c.Assert(err, IsNil)
		c.Assert(m.UpdateTable(db.ID, t1Info), IsNil)
		return nil
	})
	err = dom.Reload()
	c.Assert(err, IsNil)

	tk.MustExec("update t1, t2 set t1.c1 = 8, t2.c2 = 10 where t1.c2 = t2.c1")
	tk.MustQuery("select * from t1").Check(testkit.Rows("8 1", "8 2"))
	tk.MustQuery("select * from t2").Check(testkit.Rows("1 10", "2 10"))

	newColumn.State = model.StatePublic

	kv.RunInNewTxn(s.store, false, func(txn kv.Transaction) error {
		m := meta.NewMeta(txn)
		_, err = m.GenSchemaVersion()
		c.Assert(err, IsNil)
		c.Assert(m.UpdateTable(db.ID, t1Info), IsNil)
		return nil
	})
	err = dom.Reload()
	c.Assert(err, IsNil)

	tk.MustQuery("select * from t1").Check(testkit.Rows("8 1 9", "8 2 9"))
	tk.MustExec("drop table t1, t2")
}

func (s *testDBSuite) TestCreateTableTooLarge(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")

	sql := "create table t_too_large ("
	cnt := 3000
	for i := 1; i <= cnt; i++ {
		sql += fmt.Sprintf("a%d double, b%d double, c%d double, d%d double", i, i, i, i)
		if i != cnt {
			sql += ","
		}
	}
	sql += ");"
	s.testErrorCode(c, sql, tmysql.ErrTooManyFields)

	originLimit := ddl.TableColumnCountLimit
	ddl.TableColumnCountLimit = cnt * 4
	_, err := s.tk.Exec(sql)
	c.Assert(kv.ErrEntryTooLarge.Equal(err), IsTrue, Commentf("err:%v", err))
	ddl.TableColumnCountLimit = originLimit
}

func (s *testDBSuite) TestCreateTableWithLike(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	// for the same database
	s.tk.MustExec("use test")
	s.tk.MustExec("create table tt(id int primary key)")
	s.tk.MustExec("create table t (c1 int not null auto_increment, c2 int, constraint cc foreign key (c2) references tt(id), primary key(c1)) auto_increment = 10")
	s.tk.MustExec("insert into t set c2=1")
	s.tk.MustExec("create table t1 like test.t")
	s.tk.MustExec("insert into t1 set c2=11")
	s.tk.MustQuery("select * from t").Check(testkit.Rows("10 1"))
	s.tk.MustQuery("select * from t1").Check(testkit.Rows("1 11"))
	ctx := s.tk.Se.(sessionctx.Context)
	is := domain.GetDomain(ctx).InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t1"))
	c.Assert(err, IsNil)
	tblInfo := tbl.Meta()
	c.Assert(tblInfo.ForeignKeys, IsNil)
	c.Assert(tblInfo.PKIsHandle, Equals, true)
	col := tblInfo.Columns[0]
	hasNotNull := tmysql.HasNotNullFlag(col.Flag)
	c.Assert(hasNotNull, IsTrue)

	s.tk.MustExec("drop table tt, t1")

	// for different databases
	s.tk.MustExec("create database test1")
	s.tk.MustExec("use test1")
	s.tk.MustExec("create table t1 like test.t")
	s.tk.MustExec("insert into t1 set c2=11")
	s.tk.MustQuery("select * from t1").Check(testkit.Rows("1 11"))
	is = domain.GetDomain(ctx).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test1"), model.NewCIStr("t1"))
	c.Assert(err, IsNil)
	c.Assert(tbl.Meta().ForeignKeys, IsNil)

	// for failure cases
	failSQL := fmt.Sprintf("create table t1 like test_not_exist.t")
	s.testErrorCode(c, failSQL, tmysql.ErrNoSuchTable)
	failSQL = fmt.Sprintf("create table t1 like test.t_not_exist")
	s.testErrorCode(c, failSQL, tmysql.ErrNoSuchTable)
	failSQL = fmt.Sprintf("create table test_not_exis.t1 like test.t")
	s.testErrorCode(c, failSQL, tmysql.ErrBadDB)
	failSQL = fmt.Sprintf("create table t1 like test.t")
	s.testErrorCode(c, failSQL, tmysql.ErrTableExists)

	s.tk.MustExec("drop database test1")
	s.tk.MustExec("drop table test.t")
}

func (s *testDBSuite) TestCreateTable(c *C) {
	s.tk.MustExec("use test")
	s.tk.MustExec("CREATE TABLE `t` (`a` double DEFAULT 1.0 DEFAULT now() DEFAULT 2.0 );")
	s.tk.MustExec("CREATE TABLE IF NOT EXISTS `t` (`a` double DEFAULT 1.0 DEFAULT now() DEFAULT 2.0 );")
	ctx := s.tk.Se.(sessionctx.Context)
	is := domain.GetDomain(ctx).InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t"))
	c.Assert(err, IsNil)
	cols := tbl.Cols()

	c.Assert(len(cols), Equals, 1)
	col := cols[0]
	c.Assert(col.Name.L, Equals, "a")
	d, ok := col.DefaultValue.(string)
	c.Assert(ok, IsTrue)
	c.Assert(d, Equals, "2.0")

	s.tk.MustExec("drop table t")

	_, err = s.tk.Exec("CREATE TABLE `t` (`a` int) DEFAULT CHARSET=abcdefg")
	c.Assert(err, NotNil)
}

func (s *testDBSuite) TestTableForeignKey(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("create table t1 (a int, b int);")
	// test create table with foreign key.
	failSQL := "create table t2 (c int, foreign key (a) references t1(a));"
	s.testErrorCode(c, failSQL, tmysql.ErrKeyColumnDoesNotExits)
	// test add foreign key.
	s.tk.MustExec("create table t3 (a int, b int);")
	failSQL = "alter table t1 add foreign key (c) REFERENCES t3(a);"
	s.testErrorCode(c, failSQL, tmysql.ErrKeyColumnDoesNotExits)
	s.tk.MustExec("drop table if exists t1,t2,t3;")
}

func (s *testDBSuite) TestCreateTableWithPartition(c *C) {
	s.tk.MustExec("use test")
	s.tk.MustExec(`CREATE TABLE tp (a int) PARTITION BY RANGE(a) (
	PARTITION p0 VALUES LESS THAN (10),
	PARTITION p1 VALUES LESS THAN (20),
	PARTITION p2 VALUES LESS THAN (MAXVALUE)
);`)
	ctx := s.tk.Se.(sessionctx.Context)
	is := domain.GetDomain(ctx).InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("tp"))
	c.Assert(err, IsNil)
	c.Assert(tbl.Meta().Partition, NotNil)
	part := tbl.Meta().Partition
	c.Assert(part.Type, Equals, model.PartitionTypeRange)
	c.Assert(part.Expr, Equals, "`a`")
	for _, pdef := range part.Definitions {
		c.Assert(pdef.ID, Greater, int64(0))
	}
	c.Assert(part.Definitions, HasLen, 3)
	c.Assert(part.Definitions[0].LessThan[0], Equals, "10")
	c.Assert(part.Definitions[0].Name, Equals, "p0")
	c.Assert(part.Definitions[1].LessThan[0], Equals, "20")
	c.Assert(part.Definitions[1].Name, Equals, "p1")
	c.Assert(part.Definitions[2].MaxValue, IsTrue)
	c.Assert(part.Definitions[2].Name, Equals, "p2")

	// Fix issue 7362.
	s.tk.MustExec("create table test_partition(id bigint, name varchar(255), primary key(id)) ENGINE=InnoDB DEFAULT CHARSET=utf8 PARTITION BY RANGE  COLUMNS(id) (PARTITION p1 VALUES LESS THAN (10) ENGINE = InnoDB);")
}

func (s *testDBSuite) TestTruncateTable(c *C) {
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("use test")
	tk.MustExec("create table truncate_table (c1 int, c2 int)")
	tk.MustExec("insert truncate_table values (1, 1), (2, 2)")
	ctx := tk.Se.(sessionctx.Context)
	is := domain.GetDomain(ctx).InfoSchema()
	oldTblInfo, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("truncate_table"))
	c.Assert(err, IsNil)
	oldTblID := oldTblInfo.Meta().ID

	tk.MustExec("truncate table truncate_table")

	tk.MustExec("insert truncate_table values (3, 3), (4, 4)")
	tk.MustQuery("select * from truncate_table").Check(testkit.Rows("3 3", "4 4"))

	is = domain.GetDomain(ctx).InfoSchema()
	newTblInfo, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("truncate_table"))
	c.Assert(err, IsNil)
	c.Assert(newTblInfo.Meta().ID, Greater, oldTblID)

	// Verify that the old table data has been deleted by background worker.
	tablePrefix := tablecodec.EncodeTablePrefix(oldTblID)
	hasOldTableData := true
	for i := 0; i < waitForCleanDataRound; i++ {
		err = kv.RunInNewTxn(s.store, false, func(txn kv.Transaction) error {
			it, err1 := txn.Iter(tablePrefix, nil)
			if err1 != nil {
				return err1
			}
			if !it.Valid() {
				hasOldTableData = false
			} else {
				hasOldTableData = it.Key().HasPrefix(tablePrefix)
			}
			it.Close()
			return nil
		})
		c.Assert(err, IsNil)
		if !hasOldTableData {
			break
		}
		time.Sleep(waitForCleanDataInterval)
	}
	c.Assert(hasOldTableData, IsFalse)
}

func (s *testDBSuite) TestRenameTable(c *C) {
	isAlterTable := false
	s.testRenameTable(c, "rename table %s to %s", isAlterTable)
}

func (s *testDBSuite) TestAlterTableRenameTable(c *C) {
	isAlterTable := true
	s.testRenameTable(c, "alter table %s rename to %s", isAlterTable)
}

func (s *testDBSuite) testRenameTable(c *C, sql string, isAlterTable bool) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	// for different databases
	s.tk.MustExec("create table t (c1 int, c2 int)")
	s.tk.MustExec("insert t values (1, 1), (2, 2)")
	ctx := s.tk.Se.(sessionctx.Context)
	is := domain.GetDomain(ctx).InfoSchema()
	oldTblInfo, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t"))
	c.Assert(err, IsNil)
	oldTblID := oldTblInfo.Meta().ID
	s.tk.MustExec("create database test1")
	s.tk.MustExec("use test1")
	s.tk.MustExec(fmt.Sprintf(sql, "test.t", "test1.t1"))
	is = domain.GetDomain(ctx).InfoSchema()
	newTblInfo, err := is.TableByName(model.NewCIStr("test1"), model.NewCIStr("t1"))
	c.Assert(err, IsNil)
	c.Assert(newTblInfo.Meta().ID, Equals, oldTblID)
	s.tk.MustQuery("select * from t1").Check(testkit.Rows("1 1", "2 2"))
	s.tk.MustExec("use test")
	// Make sure t doesn't exist.
	s.tk.MustExec("create table t (c1 int, c2 int)")
	s.tk.MustExec("drop table t")

	// for the same database
	s.tk.MustExec("use test1")
	s.tk.MustExec(fmt.Sprintf(sql, "t1", "t2"))
	is = domain.GetDomain(ctx).InfoSchema()
	newTblInfo, err = is.TableByName(model.NewCIStr("test1"), model.NewCIStr("t2"))
	c.Assert(err, IsNil)
	c.Assert(newTblInfo.Meta().ID, Equals, oldTblID)
	s.tk.MustQuery("select * from t2").Check(testkit.Rows("1 1", "2 2"))
	isExist := is.TableExists(model.NewCIStr("test1"), model.NewCIStr("t1"))
	c.Assert(isExist, IsFalse)
	s.tk.MustQuery("show tables").Check(testkit.Rows("t2"))

	// for failure case
	failSQL := fmt.Sprintf(sql, "test_not_exist.t", "test_not_exist.t")
	s.testErrorCode(c, failSQL, tmysql.ErrFileNotFound)
	failSQL = fmt.Sprintf(sql, "test.t_not_exist", "test_not_exist.t")
	s.testErrorCode(c, failSQL, tmysql.ErrFileNotFound)
	failSQL = fmt.Sprintf(sql, "test1.t2", "test_not_exist.t")
	s.testErrorCode(c, failSQL, tmysql.ErrErrorOnRename)

	// for the same table name
	s.tk.MustExec("use test1")
	s.tk.MustExec("create table if not exists t (c1 int, c2 int)")
	s.tk.MustExec("create table if not exists t1 (c1 int, c2 int)")
	if isAlterTable {
		s.tk.MustExec(fmt.Sprintf(sql, "test1.t", "t"))
		s.tk.MustExec(fmt.Sprintf(sql, "test1.t1", "test1.T1"))
	} else {
		s.testErrorCode(c, fmt.Sprintf(sql, "test1.t", "t"), tmysql.ErrTableExists)
		s.testErrorCode(c, fmt.Sprintf(sql, "test1.t1", "test1.T1"), tmysql.ErrTableExists)
	}

	s.tk.MustExec("drop database test1")
}

func (s *testDBSuite) TestRenameMultiTables(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")
	s.tk.MustExec("create table t1(id int)")
	s.tk.MustExec("create table t2(id int)")
	// Currently it will fail only.
	sql := fmt.Sprintf("rename table t1 to t3, t2 to t4")
	_, err := s.tk.Exec(sql)
	c.Assert(err, NotNil)
	originErr := errors.Cause(err)
	c.Assert(originErr.Error(), Equals, "can't run multi schema change")

	s.tk.MustExec("drop table t1, t2")
}

func (s *testDBSuite) TestAddNotNullColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test_db")
	// for different databases
	s.tk.MustExec("create table tnn (c1 int primary key auto_increment, c2 int)")
	s.tk.MustExec("insert tnn (c2) values (0)" + strings.Repeat(",(0)", 99))
	done := make(chan error, 1)
	sessionExecInGoroutine(c, s.store, "alter table tnn add column c3 int not null default 3", done)
	updateCnt := 0
out:
	for {
		select {
		case err := <-done:
			c.Assert(err, IsNil)
			break out
		default:
			s.tk.MustExec("update tnn set c2 = c2 + 1 where c1 = 99")
			updateCnt++
		}
	}
	expected := fmt.Sprintf("%d %d", updateCnt, 3)
	s.tk.MustQuery("select c2, c3 from tnn where c1 = 99").Check(testkit.Rows(expected))

	s.tk.MustExec("drop table tnn")
}

func (s *testDBSuite) TestIssue2858And2717(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.tk.MustExec("create table t_issue_2858_bit (a bit(64) default b'0')")
	s.tk.MustExec("insert into t_issue_2858_bit value ()")
	s.tk.MustExec(`insert into t_issue_2858_bit values (100), ('10'), ('\0')`)
	s.tk.MustQuery("select a+0 from t_issue_2858_bit").Check(testkit.Rows("0", "100", "12592", "0"))
	s.tk.MustExec(`alter table t_issue_2858_bit alter column a set default '\0'`)

	s.tk.MustExec("create table t_issue_2858_hex (a int default 0x123)")
	s.tk.MustExec("insert into t_issue_2858_hex value ()")
	s.tk.MustExec("insert into t_issue_2858_hex values (123), (0x321)")
	s.tk.MustQuery("select a from t_issue_2858_hex").Check(testkit.Rows("291", "123", "801"))
	s.tk.MustExec(`alter table t_issue_2858_hex alter column a set default 0x321`)
}

func (s *testDBSuite) TestIssue4432(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.tk.MustExec("create table tx (col bit(10) default 'a')")
	s.tk.MustExec("insert into tx value ()")
	s.tk.MustQuery("select * from tx").Check(testkit.Rows("\x00a"))
	s.tk.MustExec("drop table tx")

	s.tk.MustExec("create table tx (col bit(10) default 0x61)")
	s.tk.MustExec("insert into tx value ()")
	s.tk.MustQuery("select * from tx").Check(testkit.Rows("\x00a"))
	s.tk.MustExec("drop table tx")

	s.tk.MustExec("create table tx (col bit(10) default 97)")
	s.tk.MustExec("insert into tx value ()")
	s.tk.MustQuery("select * from tx").Check(testkit.Rows("\x00a"))
	s.tk.MustExec("drop table tx")

	s.tk.MustExec("create table tx (col bit(10) default 0b1100001)")
	s.tk.MustExec("insert into tx value ()")
	s.tk.MustQuery("select * from tx").Check(testkit.Rows("\x00a"))
	s.tk.MustExec("drop table tx")
}

func (s *testDBSuite) TestChangeColumnPosition(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.tk.MustExec("create table position (a int default 1, b int default 2)")
	s.tk.MustExec("insert into position value ()")
	s.tk.MustExec("insert into position values (3,4)")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("1 2", "3 4"))
	s.tk.MustExec("alter table position modify column b int first")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("2 1", "4 3"))
	s.tk.MustExec("insert into position value ()")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("2 1", "4 3", "<nil> 1"))

	s.tk.MustExec("drop table position")
	s.tk.MustExec("create table position (a int, b int, c double, d varchar(5))")
	s.tk.MustExec(`insert into position value (1, 2, 3.14, 'TiDB')`)
	s.tk.MustExec("alter table position modify column d varchar(5) after a")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("1 TiDB 2 3.14"))
	s.tk.MustExec("alter table position modify column a int after c")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("TiDB 2 3.14 1"))
	s.tk.MustExec("alter table position modify column c double first")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("3.14 TiDB 2 1"))
	s.testErrorCode(c, "alter table position modify column b int after b", tmysql.ErrBadField)

	s.tk.MustExec("drop table position")
	s.tk.MustExec("create table position (a int, b int)")
	s.tk.MustExec("alter table position add index t(a, b)")
	s.tk.MustExec("alter table position modify column b int first")
	s.tk.MustExec("insert into position value (3, 5)")
	s.tk.MustQuery("select a from position where a = 3").Check(testkit.Rows())

	s.tk.MustExec("alter table position change column b c int first")
	s.tk.MustQuery("select * from position where c = 3").Check(testkit.Rows("3 5"))
	s.testErrorCode(c, "alter table position change column c b int after c", tmysql.ErrBadField)

	s.tk.MustExec("drop table position")
	s.tk.MustExec("create table position (a int default 2)")
	s.tk.MustExec("alter table position modify column a int default 5 first")
	s.tk.MustExec("insert into position value ()")
	s.tk.MustQuery("select * from position").Check(testkit.Rows("5"))

	s.tk.MustExec("drop table position")
	s.tk.MustExec("create table position (a int, b int)")
	s.tk.MustExec("alter table position add index t(b)")
	s.tk.MustExec("alter table position change column b c int first")
	createSQL := s.tk.MustQuery("show create table position").Rows()[0][1]
	exceptedSQL := []string{
		"CREATE TABLE `position` (",
		"  `c` int(11) DEFAULT NULL,",
		"  `a` int(11) DEFAULT NULL,",
		"  KEY `t` (`c`)",
		") ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin",
	}
	c.Assert(createSQL, Equals, strings.Join(exceptedSQL, "\n"))
}

func (s *testDBSuite) TestGeneratedColumnDDL(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test")

	// Check create table with virtual generated column.
	s.tk.MustExec(`CREATE TABLE test_gv_ddl(a int, b int as (a+8) virtual)`)

	// Check desc table with virtual generated column.
	result := s.tk.MustQuery(`DESC test_gv_ddl`)
	result.Check(testkit.Rows(`a int(11) YES  <nil> `, `b int(11) YES  <nil> VIRTUAL GENERATED`))

	// Check show create table with virtual generated column.
	result = s.tk.MustQuery(`show create table test_gv_ddl`)
	result.Check(testkit.Rows(
		"test_gv_ddl CREATE TABLE `test_gv_ddl` (\n  `a` int(11) DEFAULT NULL,\n  `b` int(11) GENERATED ALWAYS AS (`a` + 8) VIRTUAL DEFAULT NULL\n) ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin",
	))

	// Check alter table add a stored generated column.
	s.tk.MustExec(`alter table test_gv_ddl add column c int as (b+2) stored`)
	result = s.tk.MustQuery(`DESC test_gv_ddl`)
	result.Check(testkit.Rows(`a int(11) YES  <nil> `, `b int(11) YES  <nil> VIRTUAL GENERATED`, `c int(11) YES  <nil> STORED GENERATED`))

	// Check generated expression with blanks.
	s.tk.MustExec("create table table_with_gen_col_blanks (a int, b char(20) as (cast( \r\n\t a \r\n\tas  char)))")
	result = s.tk.MustQuery(`show create table table_with_gen_col_blanks`)
	result.Check(testkit.Rows("table_with_gen_col_blanks CREATE TABLE `table_with_gen_col_blanks` (\n" +
		"  `a` int(11) DEFAULT NULL,\n" +
		"  `b` char(20) GENERATED ALWAYS AS (CAST(`a` AS CHAR)) VIRTUAL DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin"))

	genExprTests := []struct {
		stmt string
		err  int
	}{
		// drop/rename columns dependent by other column.
		{`alter table test_gv_ddl drop column a`, mysql.ErrDependentByGeneratedColumn},
		{`alter table test_gv_ddl change column a anew int`, mysql.ErrBadField},

		// modify/change stored status of generated columns.
		{`alter table test_gv_ddl modify column b bigint`, mysql.ErrUnsupportedOnGeneratedColumn},
		{`alter table test_gv_ddl change column c cnew bigint as (a+100)`, mysql.ErrUnsupportedOnGeneratedColumn},

		// modify/change generated columns breaking prior.
		{`alter table test_gv_ddl modify column b int as (c+100)`, mysql.ErrGeneratedColumnNonPrior},
		{`alter table test_gv_ddl change column b bnew int as (c+100)`, mysql.ErrGeneratedColumnNonPrior},

		// refer not exist columns in generation expression.
		{`create table test_gv_ddl_bad (a int, b int as (c+8))`, mysql.ErrBadField},

		// refer generated columns non prior.
		{`create table test_gv_ddl_bad (a int, b int as (c+1), c int as (a+1))`, mysql.ErrGeneratedColumnNonPrior},

		// virtual generated columns cannot be primary key.
		{`create table test_gv_ddl_bad (a int, b int, c int as (a+b) primary key)`, mysql.ErrUnsupportedOnGeneratedColumn},
		{`create table test_gv_ddl_bad (a int, b int, c int as (a+b), primary key(c))`, mysql.ErrUnsupportedOnGeneratedColumn},
		{`create table test_gv_ddl_bad (a int, b int, c int as (a+b), primary key(a, c))`, mysql.ErrUnsupportedOnGeneratedColumn},
	}
	for _, tt := range genExprTests {
		s.testErrorCode(c, tt.stmt, tt.err)
	}

	// Check alter table modify/change generated column.
	s.tk.MustExec(`alter table test_gv_ddl modify column c bigint as (b+200) stored`)
	result = s.tk.MustQuery(`DESC test_gv_ddl`)
	result.Check(testkit.Rows(`a int(11) YES  <nil> `, `b int(11) YES  <nil> VIRTUAL GENERATED`, `c bigint(20) YES  <nil> STORED GENERATED`))

	s.tk.MustExec(`alter table test_gv_ddl change column b b bigint as (a+100) virtual`)
	result = s.tk.MustQuery(`DESC test_gv_ddl`)
	result.Check(testkit.Rows(`a int(11) YES  <nil> `, `b bigint(20) YES  <nil> VIRTUAL GENERATED`, `c bigint(20) YES  <nil> STORED GENERATED`))

	s.tk.MustExec(`alter table test_gv_ddl change column c cnew bigint`)
	result = s.tk.MustQuery(`DESC test_gv_ddl`)
	result.Check(testkit.Rows(`a int(11) YES  <nil> `, `b bigint(20) YES  <nil> VIRTUAL GENERATED`, `cnew bigint(20) YES  <nil> `))
}

func (s *testDBSuite) TestComment(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.tk.MustExec("drop table if exists ct, ct1")

	validComment := strings.Repeat("a", 1024)
	invalidComment := strings.Repeat("b", 1025)

	s.tk.MustExec("create table ct (c int, d int, e int, key (c) comment '" + validComment + "')")
	s.tk.MustExec("create index i on ct (d) comment '" + validComment + "'")
	s.tk.MustExec("alter table ct add key (e) comment '" + validComment + "'")

	s.testErrorCode(c, "create table ct1 (c int, key (c) comment '"+invalidComment+"')", tmysql.ErrTooLongIndexComment)
	s.testErrorCode(c, "create index i1 on ct (d) comment '"+invalidComment+"b"+"'", tmysql.ErrTooLongIndexComment)
	s.testErrorCode(c, "alter table ct add key (e) comment '"+invalidComment+"'", tmysql.ErrTooLongIndexComment)

	s.tk.MustExec("set @@sql_mode=''")
	s.tk.MustExec("create table ct1 (c int, d int, e int, key (c) comment '" + invalidComment + "')")
	c.Assert(s.tk.Se.GetSessionVars().StmtCtx.WarningCount(), Equals, uint16(1))
	s.tk.MustQuery("show warnings").Check(testutil.RowsWithSep("|", "Warning|1688|Comment for index 'c' is too long (max = 1024)"))
	s.tk.MustExec("create index i1 on ct1 (d) comment '" + invalidComment + "b" + "'")
	c.Assert(s.tk.Se.GetSessionVars().StmtCtx.WarningCount(), Equals, uint16(1))
	s.tk.MustQuery("show warnings").Check(testutil.RowsWithSep("|", "Warning|1688|Comment for index 'i1' is too long (max = 1024)"))
	s.tk.MustExec("alter table ct1 add key (e) comment '" + invalidComment + "'")
	c.Assert(s.tk.Se.GetSessionVars().StmtCtx.WarningCount(), Equals, uint16(1))
	s.tk.MustQuery("show warnings").Check(testutil.RowsWithSep("|", "Warning|1688|Comment for index 'e' is too long (max = 1024)"))

	s.tk.MustExec("drop table if exists ct, ct1")
}

func (s *testDBSuite) TestRebaseAutoID(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)

	s.tk.MustExec("drop database if exists tidb;")
	s.tk.MustExec("create database tidb;")
	s.tk.MustExec("use tidb;")
	s.tk.MustExec("create table tidb.test (a int auto_increment primary key, b int);")
	s.tk.MustExec("insert tidb.test values (null, 1);")
	s.tk.MustQuery("select * from tidb.test").Check(testkit.Rows("1 1"))
	s.tk.MustExec("alter table tidb.test auto_increment = 6000;")
	s.tk.MustExec("insert tidb.test values (null, 1);")
	s.tk.MustQuery("select * from tidb.test").Check(testkit.Rows("1 1", "6000 1"))
	s.tk.MustExec("alter table tidb.test auto_increment = 5;")
	s.tk.MustExec("insert tidb.test values (null, 1);")
	s.tk.MustQuery("select * from tidb.test").Check(testkit.Rows("1 1", "6000 1", "11000 1"))

	// Current range for table test is [11000, 15999].
	// Though it does not have a tuple "a = 15999", its global next auto increment id should be 16000.
	// Anyway it is not compatible with MySQL.
	s.tk.MustExec("alter table tidb.test auto_increment = 12000;")
	s.tk.MustExec("insert tidb.test values (null, 1);")
	s.tk.MustQuery("select * from tidb.test").Check(testkit.Rows("1 1", "6000 1", "11000 1", "16000 1"))

	s.tk.MustExec("create table tidb.test2 (a int);")
	s.testErrorCode(c, "alter table tidb.test2 add column b int auto_increment key, auto_increment=10;", tmysql.ErrUnknown)
}

func (s *testDBSuite) TestCharacterSetInColumns(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("drop database if exists varchar_test;")
	s.tk.MustExec("create database varchar_test;")
	s.tk.MustExec("use varchar_test")
	s.tk.MustExec("drop table if exists t")
	s.tk.MustExec("create table t (c1 int, s1 varchar(10), s2 text)")
	s.tk.MustQuery("select count(*) from information_schema.columns where table_schema = 'varchar_test' and character_set_name != 'utf8'").Check(testkit.Rows("0"))
	s.tk.MustQuery("select count(*) from information_schema.columns where table_schema = 'varchar_test' and character_set_name = 'utf8'").Check(testkit.Rows("2"))
}

func (s *testDBSuite) TestAddNotNullColumnWhileInsertOnDupUpdate(c *C) {
	tk1 := testkit.NewTestKit(c, s.store)
	tk1.MustExec("use " + s.schemaName)
	tk2 := testkit.NewTestKit(c, s.store)
	tk2.MustExec("use " + s.schemaName)
	closeCh := make(chan bool)
	wg := new(sync.WaitGroup)
	wg.Add(1)
	tk1.MustExec("create table nn (a int primary key, b int)")
	tk1.MustExec("insert nn values (1, 1)")
	var tk2Err error
	go func() {
		defer wg.Done()
		for {
			select {
			case <-closeCh:
				return
			default:
			}
			_, tk2Err = tk2.Exec("insert nn (a, b) values (1, 1) on duplicate key update a = 1, b = b + 1")
			if tk2Err != nil {
				return
			}
		}
	}()
	tk1.MustExec("alter table nn add column c int not null default 0")
	close(closeCh)
	wg.Wait()
	c.Assert(tk2Err, IsNil)
}

func (s *testDBSuite) TestUpdateHandleFailed(c *C) {
	gofail.Enable("github.com/pingcap/tidb/ddl/errorUpdateReorgHandle", `return(true)`)
	defer gofail.Disable("github.com/pingcap/tidb/ddl/errorUpdateReorgHandle")
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("create database if not exists test_handle_failed")
	defer tk.MustExec("drop database test_handle_failed")
	tk.MustExec("use test_handle_failed")
	tk.MustExec("create table t(a int primary key, b int)")
	tk.MustExec("insert into t values(-1, 1)")
	tk.MustExec("alter table t add index idx_b(b)")
	result := tk.MustQuery("select count(*) from t use index(idx_b)")
	result.Check(testkit.Rows("1"))
	tk.MustExec("admin check index t idx_b")
}

// TestCancelDropColumn tests cancel ddl job which type is drop column.
func (s *testDBSuite) TestCancelDropColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use " + s.schemaName)
	s.mustExec(c, "drop table if exists test_drop_column")
	s.mustExec(c, "create table test_drop_column(c1 int, c2 int)")
	defer s.mustExec(c, "drop table test_drop_column;")
	testCases := []struct {
		needAddColumn  bool
		jobState       model.JobState
		JobSchemaState model.SchemaState
		cancelSucc     bool
	}{
		{true, model.JobStateNone, model.StateNone, true},
		{false, model.JobStateRunning, model.StateWriteOnly, false},
		{true, model.JobStateRunning, model.StateDeleteOnly, false},
		{true, model.JobStateRunning, model.StateDeleteReorganization, false},
	}
	var checkErr error
	oldReorgWaitTimeout := ddl.ReorgWaitTimeout
	ddl.ReorgWaitTimeout = 50 * time.Millisecond
	defer func() { ddl.ReorgWaitTimeout = oldReorgWaitTimeout }()
	hook := &ddl.TestDDLCallback{}
	var jobID int64
	testCase := &testCases[0]
	hook.OnJobRunBeforeExported = func(job *model.Job) {
		if job.Type == model.ActionDropColumn && job.State == testCase.jobState && job.SchemaState == testCase.JobSchemaState {
			jobIDs := []int64{job.ID}
			jobID = job.ID
			hookCtx := mock.NewContext()
			hookCtx.Store = s.store
			err := hookCtx.NewTxn()
			if err != nil {
				checkErr = errors.Trace(err)
				return
			}
			txn, err := hookCtx.Txn(true)
			if err != nil {
				checkErr = errors.Trace(err)
				return
			}
			errs, err := admin.CancelJobs(txn, jobIDs)
			if err != nil {
				checkErr = errors.Trace(err)
				return
			}
			if errs[0] != nil {
				checkErr = errors.Trace(errs[0])
				return
			}
			checkErr = txn.Commit(context.Background())
		}
	}
	s.dom.DDL().SetHook(hook)
	var err1 error
	for i := range testCases {
		testCase = &testCases[i]
		if testCase.needAddColumn {
			s.mustExec(c, "alter table test_drop_column add column c3 int")
		}
		_, err1 = s.tk.Exec("alter table test_drop_column drop column c3")
		var col1 *table.Column
		t := s.testGetTable(c, "test_drop_column")
		for _, col := range t.Cols() {
			if strings.EqualFold(col.Name.L, "c3") {
				col1 = col
				break
			}
		}
		if testCase.cancelSucc {
			c.Assert(checkErr, IsNil)
			c.Assert(col1, NotNil)
			c.Assert(col1.Name.L, Equals, "c3")
			c.Assert(err1.Error(), Equals, "[ddl:12]cancelled DDL job")
		} else {
			c.Assert(col1, IsNil)
			c.Assert(err1, IsNil)
			c.Assert(checkErr, NotNil)
			c.Assert(checkErr.Error(), Equals, admin.ErrCannotCancelDDLJob.GenByArgs(jobID).Error())
		}
	}
	s.dom.DDL().SetHook(&ddl.TestDDLCallback{})
	s.mustExec(c, "alter table test_drop_column add column c3 int")
	s.mustExec(c, "alter table test_drop_column drop column c3")
}

func (s *testDBSuite) TestAddIndexFailed(c *C) {
	gofail.Enable("github.com/pingcap/tidb/ddl/mockAddIndexErr", `return(true)`)
	defer gofail.Disable("github.com/pingcap/tidb/ddl/mockAddIndexErr")
	tk := testkit.NewTestKit(c, s.store)
	tk.MustExec("create database if not exists test_add_index_failed")
	defer tk.MustExec("drop database test_add_index_failed")
	tk.MustExec("use test_add_index_failed")

	tk.MustExec("create table t(a bigint PRIMARY KEY, b int)")
	for i := 0; i < 1000; i++ {
		tk.MustExec(fmt.Sprintf("insert into t values(%v, %v)", i, i))
	}

	// Get table ID for split.
	dom := domain.GetDomain(tk.Se)
	is := dom.InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test_add_index_failed"), model.NewCIStr("t"))
	c.Assert(err, IsNil)
	tblID := tbl.Meta().ID

	// Split the table.
	s.cluster.SplitTable(s.mvccStore, tblID, 100)

	tk.MustExec("alter table t add index idx_b(b)")
	tk.MustExec("admin check index t idx_b")
	tk.MustExec("admin check table t")
}

func (s *testDBSuite) TestTransactionOnAddDropColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.mustExec(c, "use test_db")
	s.mustExec(c, "drop table if exists t1")
	s.mustExec(c, "create table t1 (a int, b int);")
	s.mustExec(c, "create table t2 (a int, b int);")
	s.mustExec(c, "insert into t2 values (2,0)")

	transactions := [][]string{
		{
			"begin",
			"insert into t1 set a=1",
			"update t1 set b=1 where a=1",
			"commit",
		},
		{
			"begin",
			"insert into t1 select a,b from t2",
			"update t1 set b=2 where a=2",
			"commit",
		},
	}

	originHook := s.dom.DDL().GetHook()
	defer s.dom.DDL().SetHook(originHook)
	hook := &ddl.TestDDLCallback{}
	hook.OnJobRunBeforeExported = func(job *model.Job) {
		switch job.SchemaState {
		case model.StateWriteOnly, model.StateWriteReorganization, model.StateDeleteOnly, model.StateDeleteReorganization:
		default:
			return
		}
		// do transaction.
		for _, transaction := range transactions {
			for _, sql := range transaction {
				s.mustExec(c, sql)
			}
		}
	}
	s.dom.DDL().SetHook(hook)
	done := make(chan error, 1)
	// test transaction on add column.
	go backgroundExec(s.store, "alter table t1 add column c int not null after a", done)
	err := <-done
	c.Assert(err, IsNil)
	s.tk.MustQuery("select a,b from t1 order by a").Check(testkit.Rows("1 1", "1 1", "1 1", "2 2", "2 2", "2 2"))
	s.mustExec(c, "delete from t1")

	// test transaction on drop column.
	go backgroundExec(s.store, "alter table t1 drop column c", done)
	err = <-done
	c.Assert(err, IsNil)
	s.tk.MustQuery("select a,b from t1 order by a").Check(testkit.Rows("1 1", "1 1", "1 1", "2 2", "2 2", "2 2"))
}

func (s *testDBSuite) TestTransactionWithWriteOnlyColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.mustExec(c, "use test_db")
	s.mustExec(c, "drop table if exists t1")
	s.mustExec(c, "create table t1 (a int key);")

	transactions := [][]string{
		{
			"begin",
			"insert into t1 set a=1",
			"update t1 set a=2 where a=1",
			"commit",
		},
	}

	originHook := s.dom.DDL().GetHook()
	defer s.dom.DDL().SetHook(originHook)
	hook := &ddl.TestDDLCallback{}
	hook.OnJobRunBeforeExported = func(job *model.Job) {
		switch job.SchemaState {
		case model.StateWriteOnly:
		default:
			return
		}
		// do transaction.
		for _, transaction := range transactions {
			for _, sql := range transaction {
				s.mustExec(c, sql)
			}
		}
	}
	s.dom.DDL().SetHook(hook)
	done := make(chan error, 1)
	// test transaction on add column.
	go backgroundExec(s.store, "alter table t1 add column c int not null", done)
	err := <-done
	c.Assert(err, IsNil)
	s.tk.MustQuery("select a from t1").Check(testkit.Rows("2"))
	s.mustExec(c, "delete from t1")

	// test transaction on drop column.
	go backgroundExec(s.store, "alter table t1 drop column c", done)
	err = <-done
	c.Assert(err, IsNil)
	s.tk.MustQuery("select a from t1").Check(testkit.Rows("2"))
}

func (s *testDBSuite) TestAddColumn2(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.mustExec(c, "use test_db")
	s.mustExec(c, "drop table if exists t1")
	s.mustExec(c, "create table t1 (a int key, b int);")
	defer s.mustExec(c, "drop table if exists t1, t2")

	originHook := s.dom.DDL().GetHook()
	defer s.dom.DDL().SetHook(originHook)
	hook := &ddl.TestDDLCallback{}
	var writeOnlyTable table.Table
	hook.OnJobRunBeforeExported = func(job *model.Job) {
		if job.SchemaState == model.StateWriteOnly {
			writeOnlyTable, _ = s.dom.InfoSchema().TableByID(job.TableID)
		}
	}
	s.dom.DDL().SetHook(hook)
	done := make(chan error, 1)
	// test transaction on add column.
	go backgroundExec(s.store, "alter table t1 add column c int not null", done)
	err := <-done
	c.Assert(err, IsNil)

	s.mustExec(c, "insert into t1 values (1,1,1)")
	s.tk.MustQuery("select a,b,c from t1").Check(testkit.Rows("1 1 1"))

	// mock for outdated tidb update record.
	c.Assert(writeOnlyTable, NotNil)
	ctx := context.Background()
	err = s.tk.Se.NewTxn()
	c.Assert(err, IsNil)
	oldRow, err := writeOnlyTable.RowWithCols(s.tk.Se, 1, writeOnlyTable.WritableCols())
	c.Assert(err, IsNil)
	c.Assert(len(oldRow), Equals, 3)
	err = writeOnlyTable.RemoveRecord(s.tk.Se, 1, oldRow)
	c.Assert(err, IsNil)
	_, err = writeOnlyTable.AddRecord(s.tk.Se, types.MakeDatums(oldRow[0].GetInt64(), 2, oldRow[2].GetInt64()), false, true)
	c.Assert(err, IsNil)
	err = s.tk.Se.StmtCommit()
	c.Assert(err, IsNil)
	err = s.tk.Se.CommitTxn(ctx)

	s.tk.MustQuery("select a,b,c from t1").Check(testkit.Rows("1 2 1"))

	// Test for _tidb_rowid
	var re *testkit.Result
	s.mustExec(c, "create table t2 (a int);")
	hook.OnJobRunBeforeExported = func(job *model.Job) {
		if job.SchemaState != model.StateWriteOnly {
			return
		}
		// allow write _tidb_rowid first
		s.mustExec(c, "set @@tidb_opt_write_row_id=1")
		s.mustExec(c, "begin")
		s.mustExec(c, "insert into t2 (a,_tidb_rowid) values (1,2);")
		re = s.tk.MustQuery(" select a,_tidb_rowid from t2;")
		s.mustExec(c, "commit")

	}
	s.dom.DDL().SetHook(hook)

	go backgroundExec(s.store, "alter table t2 add column b int not null default 3", done)
	err = <-done
	c.Assert(err, IsNil)
	re.Check(testkit.Rows("1 2"))
	s.tk.MustQuery("select a,b,_tidb_rowid from t2").Check(testkit.Rows("1 3 2"))
}

func (s *testDBSuite) TestAddIndexForGeneratedColumn(c *C) {
	s.tk = testkit.NewTestKit(c, s.store)
	s.tk.MustExec("use test_db")
	s.tk.MustExec("create table t(y year NOT NULL DEFAULT '2155')")
	defer s.mustExec(c, "drop table t;")
	for i := 0; i < 50; i++ {
		s.mustExec(c, "insert into t values (?)", i)
	}
	s.tk.MustExec("insert into t values()")
	s.tk.MustExec("ALTER TABLE t ADD COLUMN y1 year as (y + 2)")
	_, err := s.tk.Exec("ALTER TABLE t ADD INDEX idx_y(y1)")
	c.Assert(err.Error(), Equals, "[ddl:15]cannot decode index value, because cannot convert datum from unsigned bigint to type year.")

	t := s.testGetTable(c, "t")
	for _, idx := range t.Indices() {
		c.Assert(strings.EqualFold(idx.Meta().Name.L, "idx_c2"), IsFalse)
	}
	s.mustExec(c, "delete from t where y = 2155")
	s.mustExec(c, "alter table t add index idx_y(y1)")
	s.mustExec(c, "alter table t drop index idx_y")
}
