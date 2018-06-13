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

package ddl

import (
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/binloginfo"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

// RunWorker indicates if this TiDB server starts DDL worker and can run DDL job.
var RunWorker = true

// onDDLWorker is for async online schema changing, it will try to become the owner firstly,
// then wait or pull the job queue to handle a schema change job.
func (d *ddl) onDDLWorker() {
	defer d.wait.Done()

	// We use 4 * lease time to check owner's timeout, so here, we will update owner's status
	// every 2 * lease time. If lease is 0, we will use default 1s.
	// But we use etcd to speed up, normally it takes less than 1s now, so we use 1s as the max value.
	checkTime := chooseLeaseTime(2*d.lease, 1*time.Second)

	ticker := time.NewTicker(checkTime)
	defer ticker.Stop()
	defer func() {
		r := recover()
		if r != nil {
			buf := util.GetStack()
			log.Errorf("ddlWorker %v %s", r, buf)
			metrics.PanicCounter.WithLabelValues(metrics.LabelDDL).Inc()
		}
	}()

	// shouldCleanJobs is used to determine whether to clean up the job in adding index queue.
	shouldCleanJobs := true
	for {
		select {
		case <-ticker.C:
			log.Debugf("[ddl] wait %s to check DDL status again", checkTime)
		case <-d.ddlJobCh:
		case <-d.quitCh:
			return
		}

		err := d.handleDDLJobQueue(shouldCleanJobs)
		if err != nil {
			log.Errorf("[ddl] handle ddl job err %v", errors.ErrorStack(err))
		} else if shouldCleanJobs {
			log.Info("[ddl] cleaning jobs in the adding index queue finished.")
			shouldCleanJobs = false
		}
	}
}

func asyncNotify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (d *ddl) isOwner() bool {
	isOwner := d.ownerManager.IsOwner()
	log.Debugf("[ddl] it's the DDL owner %v, self ID %s", isOwner, d.uuid)
	if isOwner {
		metrics.DDLCounter.WithLabelValues(metrics.IsDDLOwner).Inc()
	}
	return isOwner
}

// buildJobDependence sets the curjob's dependency-ID.
// The dependency-job's ID must less than the current job's ID, and we need the largest one in the list.
func buildJobDependence(t *meta.Meta, curJob *model.Job) error {
	jobs, err := t.GetAllDDLJobs()
	if err != nil {
		return errors.Trace(err)
	}
	for _, job := range jobs {
		if curJob.ID < job.ID {
			continue
		}
		isDependent, err := curJob.IsDependentOn(job)
		if err != nil {
			return errors.Trace(err)
		}
		if isDependent {
			curJob.DependencyID = job.ID
			break
		}
	}
	return nil
}

// addDDLJob gets a global job ID and puts the DDL job in the DDL queue.
func (d *ddl) addDDLJob(ctx sessionctx.Context, job *model.Job) error {
	startTime := time.Now()
	job.Version = currentVersion
	job.Query, _ = ctx.Value(sessionctx.QueryString).(string)
	err := kv.RunInNewTxn(d.store, true, func(txn kv.Transaction) error {
		t := meta.NewMeta(txn)
		var err error
		job.ID, err = t.GenGlobalID()
		if err != nil {
			return errors.Trace(err)
		}
		job.StartTS = txn.StartTS()
		err = t.EnQueueDDLJob(job)
		return errors.Trace(err)
	})
	metrics.DDLWorkerHistogram.WithLabelValues(metrics.WorkerAddDDLJob, metrics.RetLabel(err)).Observe(time.Since(startTime).Seconds())
	return errors.Trace(err)
}

// getFirstDDLJob gets the first DDL job form DDL queue.
func (d *ddl) getFirstDDLJob(t *meta.Meta) (*model.Job, error) {
	job, err := t.GetDDLJob(0)
	return job, errors.Trace(err)
}

// handleUpdateJobError handles the too large DDL job.
func (d *ddl) handleUpdateJobError(t *meta.Meta, job *model.Job, err error) error {
	if err == nil {
		return nil
	}
	if kv.ErrEntryTooLarge.Equal(err) {
		log.Warnf("[ddl] update DDL job %v failed %v", job, errors.ErrorStack(err))
		// Reduce this txn entry size.
		job.BinlogInfo.Clean()
		job.Error = toTError(err)
		job.SchemaState = model.StateNone
		job.State = model.JobStateCancelled
		err = d.finishDDLJob(t, job)
	}
	return errors.Trace(err)
}

// updateDDLJob updates the DDL job information.
// Every time we enter another state except final state, we must call this function.
func (d *ddl) updateDDLJob(t *meta.Meta, job *model.Job, meetErr bool) error {
	updateRawArgs := true
	// If there is an error when running job and the RawArgs hasn't been decoded by DecodeArgs,
	// so we shouldn't replace RawArgs with the marshaling Args.
	if meetErr && (job.RawArgs != nil && job.Args == nil) {
		log.Infof("[ddl] update DDL Job %s shouldn't update raw args", job)
		updateRawArgs = false
	}
	return errors.Trace(t.UpdateDDLJob(0, job, updateRawArgs))
}

func (d *ddl) deleteRange(job *model.Job) error {
	var err error
	if job.Version <= currentVersion {
		err = d.delRangeManager.addDelRangeJob(job)
	} else {
		err = errInvalidJobVersion.GenByArgs(job.Version, currentVersion)
	}
	return errors.Trace(err)
}

// finishDDLJob deletes the finished DDL job in the ddl queue and puts it to history queue.
// If the DDL job need to handle in background, it will prepare a background job.
func (d *ddl) finishDDLJob(t *meta.Meta, job *model.Job) (err error) {
	startTime := time.Now()
	defer func() {
		metrics.DDLWorkerHistogram.WithLabelValues(metrics.WorkerFinishDDLJob, metrics.RetLabel(err)).Observe(time.Since(startTime).Seconds())
	}()

	switch job.Type {
	case model.ActionAddIndex:
		if job.State != model.JobStateRollbackDone {
			break
		}
		// After rolling back an AddIndex operation, we need to use delete-range to delete the half-done index data.
		err = d.deleteRange(job)
	case model.ActionDropSchema, model.ActionDropTable, model.ActionTruncateTable, model.ActionDropIndex:
		err = d.deleteRange(job)
	}
	if err != nil {
		return errors.Trace(err)
	}

	_, err = t.DeQueueDDLJob()
	if err != nil {
		return errors.Trace(err)
	}

	job.BinlogInfo.FinishedTS = t.StartTS
	log.Infof("[ddl] finish DDL job %v", job)
	err = t.AddHistoryDDLJob(job)
	return errors.Trace(err)
}

// getHistoryDDLJob gets a DDL job with job's ID form history queue.
func (d *ddl) getHistoryDDLJob(id int64) (*model.Job, error) {
	var job *model.Job

	err := kv.RunInNewTxn(d.store, false, func(txn kv.Transaction) error {
		t := meta.NewMeta(txn)
		var err1 error
		job, err1 = t.GetHistoryDDLJob(id)
		return errors.Trace(err1)
	})

	return job, errors.Trace(err)
}

// handleDDLJobQueue handles DDL jobs in DDL Job queue.
// shouldCleanJobs is used to determine whether to clean up the job in adding index queue.
func (d *ddl) handleDDLJobQueue(shouldCleanJobs bool) error {
	once := true
	for {
		if d.isClosed() {
			return nil
		}

		waitTime := 2 * d.lease

		var (
			job       *model.Job
			schemaVer int64
			runJobErr error
		)
		err := kv.RunInNewTxn(d.store, false, func(txn kv.Transaction) error {
			// We are not owner, return and retry checking later.
			if !d.isOwner() {
				return nil
			}

			// It's used for clean up the job in adding index queue before we support adding index queue.
			// TODO: Remove this logic after we support the adding index queue.
			if shouldCleanJobs {
				return errors.Trace(d.cleanAddIndexQueueJobs(txn))
			}

			var err error
			t := meta.NewMeta(txn)
			// We become the owner. Get the first job and run it.
			job, err = d.getFirstDDLJob(t)
			if job == nil || err != nil {
				return errors.Trace(err)
			}

			if once {
				d.waitSchemaSynced(job, waitTime)
				once = false
				return nil
			}

			if job.IsDone() || job.IsRollbackDone() {
				binloginfo.SetDDLBinlog(d.workerVars.BinlogClient, txn, job.ID, job.Query)
				if !job.IsRollbackDone() {
					job.State = model.JobStateSynced
				}
				err = d.finishDDLJob(t, job)
				return errors.Trace(err)
			}

			d.hookMu.RLock()
			d.hook.OnJobRunBefore(job)
			d.hookMu.RUnlock()

			// If running job meets error, we will save this error in job Error
			// and retry later if the job is not cancelled.
			schemaVer, runJobErr = d.runDDLJob(t, job)
			if job.IsCancelled() {
				err = d.finishDDLJob(t, job)
				return errors.Trace(err)
			}
			err = d.updateDDLJob(t, job, runJobErr != nil)
			return errors.Trace(d.handleUpdateJobError(t, job, err))
		})

		if runJobErr != nil {
			// wait a while to retry again. If we don't wait here, DDL will retry this job immediately,
			// which may act like a deadlock.
			log.Infof("[ddl] run DDL job error, sleep a while:%v then retry it.", WaitTimeWhenErrorOccured)
			metrics.DDLJobErrCounter.Inc()
			time.Sleep(WaitTimeWhenErrorOccured)
		}

		if err != nil {
			return errors.Trace(err)
		} else if job == nil {
			// No job now, return and retry getting later.
			return nil
		}

		d.hookMu.RLock()
		d.hook.OnJobUpdated(job)
		d.hookMu.RUnlock()

		// Here means the job enters another state (delete only, write only, public, etc...) or is cancelled.
		// If the job is done or still running or rolling back, we will wait 2 * lease time to guarantee other servers to update
		// the newest schema.
		if job.IsRunning() || job.IsRollingback() || job.IsDone() || job.IsRollbackDone() {
			d.waitSchemaChanged(nil, waitTime, schemaVer)
		}
		if job.IsSynced() {
			asyncNotify(d.ddlJobDoneCh)
		}
	}
}

func chooseLeaseTime(t, max time.Duration) time.Duration {
	if t == 0 || t > max {
		return max
	}
	return t
}

// runDDLJob runs a DDL job. It returns the current schema version in this transaction and the error.
func (d *ddl) runDDLJob(t *meta.Meta, job *model.Job) (ver int64, err error) {
	log.Infof("[ddl] run DDL job %s", job)
	if job.IsFinished() {
		return
	}
	// The cause of this job state is that the job is cancelled by client.
	if job.IsCancelling() {
		// If the value of SnapshotVer isn't zero, it means the work is backfilling the indexes.
		if job.Type == model.ActionAddIndex && job.SchemaState == model.StateWriteReorganization && job.SnapshotVer != 0 {
			log.Infof("[ddl] run the cancelling DDL job %s", job)
			d.reorgCtx.notifyReorgCancel()
		} else {
			job.State = model.JobStateCancelled
			job.Error = errCancelledDDLJob
			job.ErrorCount++
			return
		}
	}

	if !job.IsRollingback() && !job.IsCancelling() {
		job.State = model.JobStateRunning
	}

	switch job.Type {
	case model.ActionCreateSchema:
		ver, err = d.onCreateSchema(t, job)
	case model.ActionDropSchema:
		ver, err = d.onDropSchema(t, job)
	case model.ActionCreateTable:
		ver, err = d.onCreateTable(t, job)
	case model.ActionDropTable:
		ver, err = d.onDropTable(t, job)
	case model.ActionAddColumn:
		ver, err = d.onAddColumn(t, job)
	case model.ActionDropColumn:
		ver, err = d.onDropColumn(t, job)
	case model.ActionModifyColumn:
		ver, err = d.onModifyColumn(t, job)
	case model.ActionAddIndex:
		ver, err = d.onCreateIndex(t, job)
	case model.ActionDropIndex:
		ver, err = d.onDropIndex(t, job)
	case model.ActionAddForeignKey:
		ver, err = d.onCreateForeignKey(t, job)
	case model.ActionDropForeignKey:
		ver, err = d.onDropForeignKey(t, job)
	case model.ActionTruncateTable:
		ver, err = d.onTruncateTable(t, job)
	case model.ActionRebaseAutoID:
		ver, err = d.onRebaseAutoID(t, job)
	case model.ActionRenameTable:
		ver, err = d.onRenameTable(t, job)
	case model.ActionSetDefaultValue:
		ver, err = d.onSetDefaultValue(t, job)
	case model.ActionShardRowID:
		ver, err = d.onShardRowID(t, job)
	case model.ActionModifyTableComment:
		ver, err = d.onModifyTableComment(t, job)
	case model.ActionRenameIndex:
		ver, err = d.onRenameIndex(t, job)
	default:
		// Invalid job, cancel it.
		job.State = model.JobStateCancelled
		err = errInvalidDDLJob.Gen("invalid ddl job %v", job)
	}

	// Save errors in job, so that others can know errors happened.
	if err != nil {
		// If job is not cancelled, we should log this error.
		if job.State != model.JobStateCancelled {
			log.Errorf("[ddl] run DDL job err %v", errors.ErrorStack(err))
		} else {
			log.Infof("[ddl] the DDL job is normal to cancel because %v", errors.ErrorStack(err))
		}

		job.Error = toTError(err)
		job.ErrorCount++
	}
	return
}

func toTError(err error) *terror.Error {
	originErr := errors.Cause(err)
	tErr, ok := originErr.(*terror.Error)
	if ok {
		return tErr
	}

	// TODO: Add the error code.
	return terror.ClassDDL.New(terror.CodeUnknown, err.Error())
}

// waitSchemaChanged waits for the completion of updating all servers' schema. In order to make sure that happens,
// we wait 2 * lease time.
func (d *ddl) waitSchemaChanged(ctx context.Context, waitTime time.Duration, latestSchemaVersion int64) {
	if waitTime == 0 {
		return
	}

	timeStart := time.Now()
	var err error
	defer func() {
		metrics.DDLWorkerHistogram.WithLabelValues(metrics.WorkerWaitSchemaChanged, metrics.RetLabel(err)).Observe(time.Since(timeStart).Seconds())
	}()

	if latestSchemaVersion == 0 {
		log.Infof("[ddl] schema version doesn't change")
		return
	}

	if ctx == nil {
		var cancelFunc context.CancelFunc
		ctx, cancelFunc = context.WithTimeout(context.Background(), waitTime)
		defer cancelFunc()
	}
	err = d.schemaSyncer.OwnerUpdateGlobalVersion(ctx, latestSchemaVersion)
	if err != nil {
		log.Infof("[ddl] update latest schema version %d failed %v", latestSchemaVersion, err)
		if terror.ErrorEqual(err, context.DeadlineExceeded) {
			// If err is context.DeadlineExceeded, it means waitTime(2 * lease) is elapsed. So all the schemas are synced by ticker.
			// There is no need to use etcd to sync. The function returns directly.
			return
		}
	}

	// OwnerCheckAllVersions returns only when context is timeout(2 * lease) or all TiDB schemas are synced.
	err = d.schemaSyncer.OwnerCheckAllVersions(ctx, latestSchemaVersion)
	if err != nil {
		log.Infof("[ddl] wait latest schema version %d to deadline %v", latestSchemaVersion, err)
		if terror.ErrorEqual(err, context.DeadlineExceeded) {
			return
		}
		select {
		case <-ctx.Done():
			return
		}
	}
	log.Infof("[ddl] wait latest schema version %v changed, take time %v", latestSchemaVersion, time.Since(timeStart))
	return
}

// waitSchemaSynced handles the following situation:
// If the job enters a new state, and the worker crashs when it's in the process of waiting for 2 * lease time,
// Then the worker restarts quickly, we may run the job immediately again,
// but in this case we don't wait enough 2 * lease time to let other servers update the schema.
// So here we get the latest schema version to make sure all servers' schema version update to the latest schema version
// in a cluster, or to wait for 2 * lease time.
func (d *ddl) waitSchemaSynced(job *model.Job, waitTime time.Duration) {
	if !job.IsRunning() && !job.IsRollingback() && !job.IsDone() && !job.IsRollbackDone() {
		return
	}
	// TODO: Make ctx exits when the d is close.
	ctx, cancelFunc := context.WithTimeout(context.Background(), waitTime)
	defer cancelFunc()

	startTime := time.Now()
	latestSchemaVersion, err := d.schemaSyncer.MustGetGlobalVersion(ctx)
	if err != nil {
		log.Warnf("[ddl] handle exception take time %v", time.Since(startTime))
		return
	}
	d.waitSchemaChanged(ctx, waitTime, latestSchemaVersion)
	log.Infof("[ddl] the handle exception take time %v", time.Since(startTime))
}

// updateSchemaVersion increments the schema version by 1 and sets SchemaDiff.
func updateSchemaVersion(t *meta.Meta, job *model.Job) (int64, error) {
	schemaVersion, err := t.GenSchemaVersion()
	if err != nil {
		return 0, errors.Trace(err)
	}
	diff := &model.SchemaDiff{
		Version:  schemaVersion,
		Type:     job.Type,
		SchemaID: job.SchemaID,
	}
	if job.Type == model.ActionTruncateTable {
		// Truncate table has two table ID, should be handled differently.
		err = job.DecodeArgs(&diff.TableID)
		if err != nil {
			return 0, errors.Trace(err)
		}
		diff.OldTableID = job.TableID
	} else if job.Type == model.ActionRenameTable {
		err = job.DecodeArgs(&diff.OldSchemaID)
		if err != nil {
			return 0, errors.Trace(err)
		}
		diff.TableID = job.TableID
	} else {
		diff.TableID = job.TableID
	}
	err = t.SetSchemaDiff(diff)
	return schemaVersion, errors.Trace(err)
}

// cleanAddIndexQueueJobs cleans jobs in adding index queue.
// It's only done once after the worker become the owner.
// TODO: Remove this logic after we support the adding index queue.
func (d *ddl) cleanAddIndexQueueJobs(txn kv.Transaction) error {
	startTime := time.Now()
	m := meta.NewMeta(txn)
	m.SetJobListKey(meta.AddIndexJobListKey)
	for {
		job, err := d.getFirstDDLJob(m)
		if err != nil {
			return errors.Trace(err)
		}
		if job == nil {
			log.Infof("[ddl] cleaning jobs in the adding index queue takes time %v.", time.Since(startTime))
			return nil
		}
		log.Infof("[ddl] cleaning job %v in the adding index queue.", job)

		// The types of these jobs must be ActionAddIndex.
		if job.SchemaState == model.StatePublic || job.SchemaState == model.StateNone {
			if job.SchemaState == model.StateNone {
				job.State = model.JobStateCancelled
			} else {
				binloginfo.SetDDLBinlog(d.workerVars.BinlogClient, txn, job.ID, job.Query)
				job.State = model.JobStateSynced
			}
			err = d.finishDDLJob(m, job)
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}

		// When the job not in "none" and "public" state, we need to rollback it.
		schemaID := job.SchemaID
		tblInfo, err := getTableInfo(m, job, schemaID)
		if err != nil {
			return errors.Trace(err)
		}
		var indexName model.CIStr
		var unique bool
		err = job.DecodeArgs(&unique, &indexName)
		if err != nil {
			return errors.Trace(err)
		}
		indexInfo := findIndexByName(indexName.L, tblInfo.Indices)
		_, err = d.convert2RollbackJob(m, job, tblInfo, indexInfo, nil)
		if err == nil {
			_, err = m.DeQueueDDLJob()
		}
		if err != nil {
			return errors.Trace(err)
		}
		// Put the job to the default job list.
		m.SetJobListKey(meta.DefaultJobListKey)
		err = m.EnQueueDDLJob(job)
		m.SetJobListKey(meta.AddIndexJobListKey)
		if err != nil {
			return errors.Trace(err)
		}
	}
}
