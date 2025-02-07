// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// Package upgrades contains the implementation of upgrades. It is imported
// by the server library.
//
// This package registers the upgrades with the upgrade package.

package upgrades

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	// Import for the side effect of registering the MVCC statistics update job.
	_ "github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/systemschema"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/upgrade"
)

func createMVCCStatisticsTableAndJobMigration(
	ctx context.Context, cv clusterversion.ClusterVersion, d upgrade.TenantDeps,
) error {

	// Create the table.
	err := createSystemTable(
		ctx,
		d.DB.KV(),
		d.Settings,
		d.Codec,
		systemschema.SystemMVCCStatisticsTable,
	)
	if err != nil {
		return err
	}

	// Bake the job.
	return createMVCCStatisticsJob(ctx, cv, d)
}

func createMVCCStatisticsJob(
	ctx context.Context, _ clusterversion.ClusterVersion, d upgrade.TenantDeps,
) error {
	if d.TestingKnobs != nil && d.TestingKnobs.SkipMVCCStatisticsJobBootstrap {
		return nil
	}

	record := jobs.Record{
		JobID:         jobs.MVCCStatisticsJobID,
		Description:   "mvcc statistics update job",
		Username:      username.NodeUserName(),
		Details:       jobspb.MVCCStatisticsJobDetails{},
		Progress:      jobspb.MVCCStatisticsJobProgress{},
		NonCancelable: true, // The job can't be canceled, but it can be paused.
	}
	return d.DB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		return d.JobRegistry.CreateIfNotExistAdoptableJobWithTxn(ctx, record, txn)
	})
}
