// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package ttljob

import (
	"bytes"
	"context"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/lexbase"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/admission/admissionpb"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

type ttlProcessor struct {
	execinfra.ProcessorBase
	ttlSpec execinfrapb.TTLSpec
}

func (t *ttlProcessor) Start(ctx context.Context) {
	ctx = t.StartInternal(ctx, "ttl")
	err := t.work(ctx)
	t.MoveToDraining(err)
}

func (t *ttlProcessor) work(ctx context.Context) error {

	ttlSpec := t.ttlSpec
	flowCtx := t.FlowCtx
	serverCfg := flowCtx.Cfg
	descsCol := flowCtx.Descriptors
	codec := serverCfg.Codec
	details := ttlSpec.RowLevelTTLDetails
	tableID := details.TableID

	deleteRateLimit := ttlSpec.DeleteRateLimit
	deleteRateLimiter := quotapool.NewRateLimiter(
		"ttl-delete",
		quotapool.Limit(deleteRateLimit),
		deleteRateLimit,
	)

	processorRowCount := int64(0)

	var (
		relationName string
		pkColNames   []string
		pkColTypes   []*types.T
		pkColDirs    []catenumpb.IndexColumn_Direction
		labelMetrics bool
	)
	if err := serverCfg.DB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		desc, err := descsCol.ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, tableID)
		if err != nil {
			return err
		}

		var buf bytes.Buffer
		primaryIndexDesc := desc.GetPrimaryIndex().IndexDesc()
		pkColNames = make([]string, 0, len(primaryIndexDesc.KeyColumnNames))
		for _, name := range primaryIndexDesc.KeyColumnNames {
			lexbase.EncodeRestrictedSQLIdent(&buf, name, lexbase.EncNoFlags)
			pkColNames = append(pkColNames, buf.String())
			buf.Reset()
		}
		pkColTypes, err = GetPKColumnTypes(desc, primaryIndexDesc)
		if err != nil {
			return err
		}
		pkColDirs = primaryIndexDesc.KeyColumnDirections

		if !desc.HasRowLevelTTL() {
			return errors.Newf("unable to find TTL on table %s", desc.GetName())
		}

		rowLevelTTL := desc.GetRowLevelTTL()
		labelMetrics = rowLevelTTL.LabelMetrics

		tn, err := descs.GetObjectName(ctx, txn.KV(), descsCol, desc)
		if err != nil {
			return errors.Wrapf(err, "error fetching table relation name for TTL")
		}

		relationName = tn.FQString()
		return nil
	}); err != nil {
		return err
	}

	jobRegistry := serverCfg.JobRegistry
	metrics := jobRegistry.MetricsStruct().RowLevelTTL.(*RowLevelTTLAggMetrics).loadMetrics(
		labelMetrics,
		relationName,
	)

	group := ctxgroup.WithContext(ctx)
	processorSpanCount := int64(len(ttlSpec.Spans))
	processorConcurrency := int64(runtime.GOMAXPROCS(0))
	if processorSpanCount < processorConcurrency {
		processorConcurrency = processorSpanCount
	}
	err := func() error {
		boundsChan := make(chan QueryBounds, processorConcurrency)
		defer close(boundsChan)
		for i := int64(0); i < processorConcurrency; i++ {
			group.GoCtx(func(ctx context.Context) error {
				for bounds := range boundsChan {
					start := timeutil.Now()
					spanRowCount, err := t.runTTLOnQueryBounds(
						ctx,
						metrics,
						bounds,
						pkColNames,
						pkColDirs,
						relationName,
						deleteRateLimiter,
					)
					// add before returning err in case of partial success
					atomic.AddInt64(&processorRowCount, spanRowCount)
					metrics.SpanTotalDuration.RecordValue(int64(timeutil.Since(start)))
					if err != nil {
						// Continue until channel is fully read.
						// Otherwise, the keys input will be blocked.
						for bounds = range boundsChan {
						}
						return err
					}
				}
				return nil
			})
		}

		// Iterate over every span to feed work for the goroutine processors.
		var alloc tree.DatumAlloc
		for _, span := range ttlSpec.Spans {
			startKey := span.Key
			startPK, err := rowenc.DecodeIndexKeyToDatums(codec, pkColTypes, pkColDirs, startKey, &alloc)
			if err != nil {
				return errors.Wrapf(err, "decode startKey error key=%x", []byte(startKey))
			}
			endKey := span.EndKey
			endPK, err := rowenc.DecodeIndexKeyToDatums(codec, pkColTypes, pkColDirs, endKey, &alloc)
			if err != nil {
				return errors.Wrapf(err, "decode endKey error key=%x", []byte(endKey))
			}
			boundsChan <- QueryBounds{
				Start: startPK,
				End:   endPK,
			}
		}
		return nil
	}()
	if err != nil {
		return err
	}

	if err := group.Wait(); err != nil {
		return err
	}

	sqlInstanceID := flowCtx.NodeID.SQLInstanceID()
	jobID := ttlSpec.JobID
	return jobRegistry.UpdateJobWithTxn(
		ctx,
		jobID,
		nil,  /* txn */
		true, /* useReadLock */
		func(_ isql.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
			progress := md.Progress
			rowLevelTTL := progress.Details.(*jobspb.Progress_RowLevelTTL).RowLevelTTL
			rowLevelTTL.JobRowCount += processorRowCount
			processorID := t.ProcessorID
			rowLevelTTL.ProcessorProgresses = append(rowLevelTTL.ProcessorProgresses, jobspb.RowLevelTTLProcessorProgress{
				ProcessorID:          processorID,
				SQLInstanceID:        sqlInstanceID,
				ProcessorRowCount:    processorRowCount,
				ProcessorSpanCount:   processorSpanCount,
				ProcessorConcurrency: processorConcurrency,
			})
			ju.UpdateProgress(progress)
			log.VInfof(
				ctx,
				2, /* level */
				"TTL processorRowCount updated jobID=%d processorID=%d sqlInstanceID=%d tableID=%d jobRowCount=%d processorRowCount=%d",
				jobID, processorID, sqlInstanceID, tableID, rowLevelTTL.JobRowCount, processorRowCount,
			)
			return nil
		},
	)
}

// spanRowCount should be checked even if the function returns an error because it may have partially succeeded
func (t *ttlProcessor) runTTLOnQueryBounds(
	ctx context.Context,
	metrics rowLevelTTLMetrics,
	bounds QueryBounds,
	pkColNames []string,
	pkColDirs []catenumpb.IndexColumn_Direction,
	relationName string,
	deleteRateLimiter *quotapool.RateLimiter,
) (spanRowCount int64, err error) {
	metrics.NumActiveSpans.Inc(1)
	defer metrics.NumActiveSpans.Dec(1)

	// TODO(#82140): investigate improving row deletion performance with secondary indexes

	ttlSpec := t.ttlSpec
	details := ttlSpec.RowLevelTTLDetails
	cutoff := details.Cutoff
	ttlExpr := ttlSpec.TTLExpr
	flowCtx := t.FlowCtx
	serverCfg := flowCtx.Cfg
	ie := serverCfg.DB.Executor()

	selectBatchSize := ttlSpec.SelectBatchSize

	aostDuration := ttlSpec.AOSTDuration
	if aostDuration == 0 {
		// Read AOST in case of mixed 22.2.0/22.2.1+ cluster where the job started on a 22.2.0 node.
		//lint:ignore SA1019 execinfrapb.TTLSpec.AOST is deprecated
		aost := ttlSpec.AOST
		if !aost.IsZero() {
			aostDuration = aost.Sub(details.Cutoff)
		}
	}

	selectBuilder := MakeSelectQueryBuilder(
		cutoff,
		pkColNames,
		pkColDirs,
		relationName,
		bounds,
		aostDuration,
		selectBatchSize,
		ttlExpr,
	)
	deleteBatchSize := ttlSpec.DeleteBatchSize
	deleteBuilder := MakeDeleteQueryBuilder(
		cutoff,
		pkColNames,
		relationName,
		deleteBatchSize,
		ttlExpr,
	)

	preSelectStatement := ttlSpec.PreSelectStatement
	if preSelectStatement != "" {
		if _, err := ie.ExecEx(
			ctx,
			"pre-select-delete-statement",
			nil, /* txn */
			sessiondata.RootUserSessionDataOverride,
			preSelectStatement,
		); err != nil {
			return spanRowCount, err
		}
	}

	settingsValues := &serverCfg.Settings.SV
	for {
		// Check the job is enabled on every iteration.
		if err := checkEnabled(settingsValues); err != nil {
			return spanRowCount, err
		}

		// Step 1. Fetch some rows we want to delete using a historical
		// SELECT query.
		start := timeutil.Now()
		expiredRowsPKs, hasNext, err := selectBuilder.Run(ctx, ie)
		metrics.SelectDuration.RecordValue(int64(timeutil.Since(start)))
		if err != nil {
			return spanRowCount, errors.Wrapf(err, "error selecting rows to delete")
		}
		numExpiredRows := int64(len(expiredRowsPKs))
		metrics.RowSelections.Inc(numExpiredRows)

		// Step 2. Delete the rows which have expired.
		for startRowIdx := int64(0); startRowIdx < numExpiredRows; startRowIdx += deleteBatchSize {
			until := startRowIdx + deleteBatchSize
			if until > numExpiredRows {
				until = numExpiredRows
			}
			deleteBatch := expiredRowsPKs[startRowIdx:until]
			do := func(ctx context.Context, txn isql.Txn) error {
				// If we detected a schema change here, the DELETE will not succeed
				// (the SELECT still will because of the AOST). Early exit here.
				desc, err := flowCtx.Descriptors.ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, details.TableID)
				if err != nil {
					return err
				}
				if ttlSpec.PreDeleteChangeTableVersion || desc.GetVersion() != details.TableVersion {
					return errors.Newf(
						"table has had a schema change since the job has started at %s, aborting",
						desc.GetModificationTime().GoTime().Format(time.RFC3339),
					)
				}
				tokens, err := deleteRateLimiter.Acquire(ctx, int64(len(deleteBatch)))
				if err != nil {
					return err
				}
				defer tokens.Consume()

				start := timeutil.Now()
				batchRowCount, err := deleteBuilder.Run(ctx, txn, deleteBatch)
				if err != nil {
					return err
				}

				metrics.DeleteDuration.RecordValue(int64(timeutil.Since(start)))
				metrics.RowDeletions.Inc(batchRowCount)
				spanRowCount += batchRowCount
				return nil
			}
			if err := serverCfg.DB.Txn(
				ctx, do, isql.SteppingEnabled(), isql.WithPriority(admissionpb.TTLLowPri),
			); err != nil {
				return spanRowCount, errors.Wrapf(err, "error during row deletion")
			}
		}

		// Step 3. Early exit if necessary.

		// If we selected less than the select batch size, we have selected every
		// row and so we end it here.
		if !hasNext {
			break
		}
	}

	return spanRowCount, nil
}

func (t *ttlProcessor) Next() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	return nil, t.DrainHelper()
}

func newTTLProcessor(
	ctx context.Context, flowCtx *execinfra.FlowCtx, processorID int32, spec execinfrapb.TTLSpec,
) (execinfra.Processor, error) {
	ttlProcessor := &ttlProcessor{
		ttlSpec: spec,
	}
	if err := ttlProcessor.Init(
		ctx,
		ttlProcessor,
		&execinfrapb.PostProcessSpec{},
		[]*types.T{},
		flowCtx,
		processorID,
		nil, /* memMonitor */
		execinfra.ProcStateOpts{},
	); err != nil {
		return nil, err
	}
	return ttlProcessor, nil
}

// GetPKColumnTypes returns tableDesc's primary key column types.
func GetPKColumnTypes(
	tableDesc catalog.TableDescriptor, indexDesc *descpb.IndexDescriptor,
) ([]*types.T, error) {
	pkColTypes := make([]*types.T, 0, len(indexDesc.KeyColumnIDs))
	for i, id := range indexDesc.KeyColumnIDs {
		col, err := catalog.MustFindColumnByID(tableDesc, id)
		if err != nil {
			return nil, errors.Wrapf(err, "column index=%d", i)
		}
		pkColTypes = append(pkColTypes, col.GetType())
	}
	return pkColTypes, nil
}

func init() {
	rowexec.NewTTLProcessor = newTTLProcessor
}
