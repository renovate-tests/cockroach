// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/option"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/roachtestutil"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/spec"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/cockroach/pkg/roachprod/logger"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/ts/tspb"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/workload/histogram"
	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func registerRestoreNodeShutdown(r registry.Registry) {
	sp := restoreSpecs{
		hardware: makeHardwareSpecs(hardwareSpecs{}),
		backup: makeRestoringBackupSpecs(
			backupSpecs{workload: tpceRestore{customers: 1000},
				version: "v22.2.1"}),
		timeout:     1 * time.Hour,
		fingerprint: 8445446819555404274,
	}

	makeRestoreStarter := func(ctx context.Context, t test.Test, c cluster.Cluster,
		gatewayNode int, rd restoreDriver) jobStarter {
		return func(c cluster.Cluster, l *logger.Logger) (jobspb.JobID, error) {
			return rd.runDetached(ctx, "DATABASE tpce", gatewayNode)
		}
	}

	r.Add(registry.TestSpec{
		Name:             "restore/nodeShutdown/worker",
		Owner:            registry.OwnerDisasterRecovery,
		Cluster:          sp.hardware.makeClusterSpecs(r, sp.backup.cloud),
		CompatibleClouds: registry.AllExceptAWS,
		Suites:           registry.Suites(registry.Nightly),
		Leases:           registry.MetamorphicLeases,
		Timeout:          sp.timeout,
		Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
			gatewayNode := 2
			nodeToShutdown := 3

			rd := makeRestoreDriver(t, c, sp)
			rd.prepareCluster(ctx)
			jobSurvivesNodeShutdown(ctx, t, c, nodeToShutdown, makeRestoreStarter(ctx, t, c,
				gatewayNode, rd))
			rd.checkFingerprint(ctx)
		},
	})

	r.Add(registry.TestSpec{
		Name:             "restore/nodeShutdown/coordinator",
		Owner:            registry.OwnerDisasterRecovery,
		Cluster:          sp.hardware.makeClusterSpecs(r, sp.backup.cloud),
		CompatibleClouds: registry.AllExceptAWS,
		Suites:           registry.Suites(registry.Nightly),
		Leases:           registry.MetamorphicLeases,
		Timeout:          sp.timeout,
		Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {

			gatewayNode := 2
			nodeToShutdown := 2

			rd := makeRestoreDriver(t, c, sp)
			rd.prepareCluster(ctx)

			jobSurvivesNodeShutdown(ctx, t, c, nodeToShutdown, makeRestoreStarter(ctx, t, c,
				gatewayNode, rd))
			rd.checkFingerprint(ctx)
		},
	})
}

func registerRestore(r registry.Registry) {

	durationGauge := r.PromFactory().NewGaugeVec(prometheus.GaugeOpts{Namespace: registry.
		PrometheusNameSpace, Subsystem: "restore", Name: "duration"}, []string{"test_name"})

	withPauseSpecs := restoreSpecs{
		hardware: makeHardwareSpecs(hardwareSpecs{ebsThroughput: 250 /* MB/s */}),
		backup: makeRestoringBackupSpecs(
			backupSpecs{workload: tpceRestore{customers: 1000},
				version: "v22.2.1"}),
		timeout:     3 * time.Hour,
		namePrefix:  "pause",
		fingerprint: 8445446819555404274,
	}
	withPauseSpecs.initTestName()

	r.Add(registry.TestSpec{
		Name:             withPauseSpecs.testName,
		Owner:            registry.OwnerDisasterRecovery,
		Benchmark:        true,
		Cluster:          withPauseSpecs.hardware.makeClusterSpecs(r, withPauseSpecs.backup.cloud),
		Timeout:          withPauseSpecs.timeout,
		CompatibleClouds: registry.AllClouds,
		Suites:           registry.Suites(registry.Nightly),
		Tags:             registry.Tags("aws"),
		Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {

			rd := makeRestoreDriver(t, c, withPauseSpecs)
			rd.prepareCluster(ctx)

			// Run the disk usage logger in the monitor to guarantee its
			// having terminated when the test ends.
			m := c.NewMonitor(ctx)
			dul := roachtestutil.NewDiskUsageLogger(t, c)
			m.Go(dul.Runner)

			jobIDCh := make(chan jobspb.JobID)
			jobCompleteCh := make(chan struct{}, 1)

			pauseAtProgress := []float32{0.2, 0.45, 0.7}
			for i := range pauseAtProgress {
				// Add up to 10% to the pause point.
				pauseAtProgress[i] = pauseAtProgress[i] + float32(rand.Intn(10))/100
			}
			pauseIndex := 0
			// Spin up go routine which pauses and resumes the Restore job three times.
			m.Go(func(ctx context.Context) error {
				// Wait until the restore job has been created.
				conn, err := c.ConnE(ctx, t.L(), c.Node(1)[0])
				require.NoError(t, err)
				sql := sqlutils.MakeSQLRunner(conn)

				// The job should be created fairly quickly once the roachtest starts.
				done := ctx.Done()
				jobID := <-jobIDCh

				jobProgressTick := time.NewTicker(time.Minute * 1)
				defer jobProgressTick.Stop()
				for {
					if pauseIndex == len(pauseAtProgress) {
						t.L().Printf("RESTORE job was paused a maximum number of times; allowing the job to complete")
						return nil
					}
					select {
					case <-done:
						return ctx.Err()
					case <-jobCompleteCh:
						return nil
					case <-jobProgressTick.C:
						var fraction float32
						sql.QueryRow(t, `SELECT fraction_completed FROM [SHOW JOBS] WHERE job_id = $1`,
							jobID).Scan(&fraction)
						t.L().Printf("RESTORE Progress %.2f", fraction)
						if fraction < pauseAtProgress[pauseIndex] {
							continue
						}
						t.L().Printf("pausing RESTORE job since progress is greater than %.2f", pauseAtProgress[pauseIndex])
						// Pause the job and wait for it to transition to a paused state.
						_, err := conn.Query(`PAUSE JOB $1`, jobID)
						if err != nil {
							// The pause job request should not fail unless the job has already succeeded,
							// in which case, the test should gracefully succeed.
							var status string
							sql.QueryRow(t, `SELECT status FROM [SHOW JOBS] WHERE job_id = $1`, jobID).Scan(&status)
							if status == "succeeded" {
								return nil
							}
						}
						require.NoError(t, err)
						testutils.SucceedsSoon(t, func() error {
							var status string
							sql.QueryRow(t, `SELECT status FROM [SHOW JOBS] WHERE job_id = $1`, jobID).Scan(&status)
							if status != "paused" {
								return errors.Newf("expected status `paused` but found %s", status)
							}
							t.L().Printf("paused RESTORE job")
							pauseIndex++
							return nil
						})

						t.L().Printf("resuming RESTORE job")
						sql.Exec(t, `RESUME JOB $1`, jobID)
					}
				}
			})

			m.Go(func(ctx context.Context) error {
				defer dul.Done()
				defer close(jobCompleteCh)
				defer close(jobIDCh)
				t.Status(`running restore`)
				metricCollector := rd.initRestorePerfMetrics(ctx, durationGauge)
				jobID, err := rd.runDetached(ctx, "DATABASE tpce", 1)
				require.NoError(t, err)
				jobIDCh <- jobID

				// Wait for the job to succeed.
				succeededJobTick := time.NewTicker(time.Minute * 1)
				defer succeededJobTick.Stop()
				done := ctx.Done()
				conn, err := c.ConnE(ctx, t.L(), c.Node(1)[0])
				require.NoError(t, err)
				var isJobComplete bool
				for {
					if isJobComplete {
						succeededJobTick.Stop()
						jobCompleteCh <- struct{}{}
						break
					}

					select {
					case <-done:
						return ctx.Err()
					case <-jobCompleteCh:
						return nil
					case <-succeededJobTick.C:
						var status string
						err := conn.QueryRow(`SELECT status FROM [SHOW JOBS] WHERE job_type = 'RESTORE'`).Scan(&status)
						require.NoError(t, err)
						if status == string(jobs.StatusSucceeded) {
							isJobComplete = true
						} else if status == string(jobs.StatusFailed) || status == string(jobs.StatusCanceled) {
							t.Fatalf("job unexpectedly found in %s state", status)
						}
					}
				}
				metricCollector()
				rd.checkFingerprint(ctx)
				return nil
			})
			m.Wait()
			// All failures from the above go routines surface via a t.Fatal() within
			// the m.Wait( ) call above; therefore, at this point, the restore job
			// should have succeeded. This final check ensures this test is actually
			// doing its job: causing the restore job to pause at least once.
			require.NotEqual(t, 0, pauseIndex, "the job should have paused at least once")
		},

		// TODO(msbutler): to test the correctness of checkpointing, we should
		// restore the same fixture without pausing it and fingerprint both restored
		// databases.
	})

	for _, sp := range []restoreSpecs{
		{
			hardware: makeHardwareSpecs(hardwareSpecs{ebsThroughput: 250 /* MB/s */}),
			backup:   makeRestoringBackupSpecs(backupSpecs{}),
			timeout:  1 * time.Hour,
			clouds:   registry.AllClouds,
			suites:   registry.Suites(registry.Nightly),
			tags:     registry.Tags("aws"),
		},
		{
			// Note that the default specs in makeHardwareSpecs() spin up restore tests in aws,
			// by default.
			hardware: makeHardwareSpecs(hardwareSpecs{}),
			backup:   makeRestoringBackupSpecs(backupSpecs{cloud: spec.GCE}),
			timeout:  1 * time.Hour,
			clouds:   registry.AllExceptAWS,
			suites:   registry.Suites(registry.Nightly),
		},
		{
			// Benchmarks using a low memory per core ratio - we don't expect ideal
			// performance but nodes should not OOM.
			hardware: makeHardwareSpecs(hardwareSpecs{mem: spec.Low}),
			backup:   makeRestoringBackupSpecs(backupSpecs{cloud: spec.GCE}),
			timeout:  1 * time.Hour,
			clouds:   registry.AllExceptAWS,
			suites:   registry.Suites(registry.Nightly),
		},
		{
			// Benchmarks if per node throughput remains constant if the number of
			// nodes doubles relative to default.
			hardware: makeHardwareSpecs(hardwareSpecs{nodes: 8, ebsThroughput: 250 /* MB/s */}),
			backup:   makeRestoringBackupSpecs(backupSpecs{}),
			timeout:  1 * time.Hour,
			clouds:   registry.AllClouds,
			suites:   registry.Suites(registry.Nightly),
			tags:     registry.Tags("aws"),
		},
		{
			// Benchmarks if per node throughput remains constant if the cluster
			// is multi-region.
			hardware: makeHardwareSpecs(hardwareSpecs{
				nodes: 9, ebsThroughput: 250, /* MB/s */
				zones: []string{"us-east-2b", "us-west-2b", "eu-west-1b"}}), // These zones are AWS-specific.
			backup:  makeRestoringBackupSpecs(backupSpecs{cloud: spec.AWS}),
			timeout: 90 * time.Minute,
			clouds:  registry.AllClouds,
			suites:  registry.Suites(registry.Nightly),
			tags:    registry.Tags("aws"),
		},
		{
			// Benchmarks if per node throughput doubles if the vcpu count doubles
			// relative to default.
			hardware: makeHardwareSpecs(hardwareSpecs{cpus: 16, ebsThroughput: 250 /* MB/s */}),
			backup:   makeRestoringBackupSpecs(backupSpecs{}),
			timeout:  1 * time.Hour,
			clouds:   registry.AllClouds,
			suites:   registry.Suites(registry.Nightly),
			tags:     registry.Tags("aws"),
		},
		{
			// Ensures we can restore a 48 length incremental chain.
			// Also benchmarks per node throughput for a long chain.
			hardware: makeHardwareSpecs(hardwareSpecs{ebsThroughput: 250 /* MB/s */}),
			backup:   makeRestoringBackupSpecs(backupSpecs{backupsIncluded: 48}),
			timeout:  1 * time.Hour,
			clouds:   registry.AllClouds,
			suites:   registry.Suites(registry.Nightly),
			tags:     registry.Tags("aws"),
		},
		{
			// The nightly 8TB Restore test.
			// NB: bump disk throughput because this load saturates the default 125
			// MB/s. See https://github.com/cockroachdb/cockroach/issues/107609.
			hardware: makeHardwareSpecs(hardwareSpecs{nodes: 10, volumeSize: 2000,
				ebsThroughput: 250 /* MB/s */}),
			backup: makeRestoringBackupSpecs(backupSpecs{
				version:  "v22.2.1",
				workload: tpceRestore{customers: 500000}}),
			timeout: 5 * time.Hour,
			clouds:  registry.AllClouds,
			suites:  registry.Suites(registry.Nightly),
			tags:    registry.Tags("aws"),
		},
		{
			// The weekly 32TB Restore test.
			hardware: makeHardwareSpecs(hardwareSpecs{nodes: 15, cpus: 16, volumeSize: 5000,
				ebsThroughput: 250 /* MB/s */}),
			backup: makeRestoringBackupSpecs(backupSpecs{
				version:  "v22.2.1",
				workload: tpceRestore{customers: 2000000}}),
			timeout: 24 * time.Hour,
			clouds:  registry.AllClouds,
			suites:  registry.Suites(registry.Weekly),
			tags:    registry.Tags("weekly", "aws-weekly"),
		},
		{
			// The weekly 32TB, 400 incremental layer Restore test on AWS.
			//
			// NB: Prior to 23.1, restore would OOM on backups that had many
			// incremental layers and many import spans. This test disables span
			// target size so restore can process the maximum number of import
			// spans. Together with having a 400 incremental chain, this
			// regression tests against the OOMs that we've seen in previous
			// versions.
			hardware: makeHardwareSpecs(hardwareSpecs{nodes: 15, cpus: 16, volumeSize: 5000,
				ebsThroughput: 250 /* MB/s */}),
			backup: makeRestoringBackupSpecs(backupSpecs{
				version:          "v22.2.4",
				workload:         tpceRestore{customers: 2000000},
				backupProperties: "inc-count=400",
			}),
			timeout: 30 * time.Hour,
			clouds:  registry.AllClouds,
			suites:  registry.Suites(registry.Weekly),
			tags:    registry.Tags("weekly", "aws-weekly"),
			setUpStmts: []string{
				`SET CLUSTER SETTING backup.restore_span.target_size = '0'`,
			},
		},
		{
			// The weekly 32TB, 400 incremental layer Restore test on GCP.
			hardware: makeHardwareSpecs(hardwareSpecs{nodes: 15, cpus: 16, volumeSize: 5000}),
			backup: makeRestoringBackupSpecs(backupSpecs{
				version:          "v22.2.4",
				workload:         tpceRestore{customers: 2000000},
				backupProperties: "inc-count=400",
				cloud:            spec.GCE,
			}),
			timeout: 30 * time.Hour,
			clouds:  registry.AllExceptAWS,
			suites:  registry.Suites(registry.Weekly),
			tags:    registry.Tags("weekly"),
			setUpStmts: []string{
				`SET CLUSTER SETTING backup.restore_span.target_size = '0'`,
			},
		},
		{
			// A teeny weeny 15GB restore that could be used to bisect scale agnostic perf regressions.
			hardware: makeHardwareSpecs(hardwareSpecs{ebsThroughput: 250 /* MB/s */}),
			backup: makeRestoringBackupSpecs(
				backupSpecs{workload: tpceRestore{customers: 1000},
					version: "v22.2.1"}),
			timeout:     3 * time.Hour,
			clouds:      registry.AllExceptAWS,
			suites:      registry.Suites(registry.Nightly),
			fingerprint: 8445446819555404274,
		},
		// TODO(msbutler): add the following tests once roachperf/grafana is hooked up and old tests are
		// removed:
		// - restore/tpce/400GB/nodes=30
		// - restore/tpce/400GB/encryption
	} {
		sp := sp
		sp.initTestName()
		r.Add(registry.TestSpec{
			Name:      sp.testName,
			Owner:     registry.OwnerDisasterRecovery,
			Benchmark: true,
			Cluster:   sp.hardware.makeClusterSpecs(r, sp.backup.cloud),
			Timeout:   sp.timeout,
			// These tests measure performance. To ensure consistent perf,
			// disable metamorphic encryption.
			EncryptionSupport: registry.EncryptionAlwaysDisabled,
			CompatibleClouds:  sp.clouds,
			Suites:            sp.suites,
			Tags:              sp.tags,
			Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {

				rd := makeRestoreDriver(t, c, sp)
				rd.prepareCluster(ctx)

				// Run the disk usage logger in the monitor to guarantee its
				// having terminated when the test ends.
				m := c.NewMonitor(ctx)
				dul := roachtestutil.NewDiskUsageLogger(t, c)
				m.Go(dul.Runner)
				m.Go(func(ctx context.Context) error {
					defer dul.Done()
					t.Status(`running setup statements`)
					db, err := rd.c.ConnE(ctx, rd.t.L(), rd.c.Node(1)[0])
					if err != nil {
						return errors.Wrapf(err, "failure to run setup statements")
					}
					// Run set-up SQL statements. In particular, enable collecting CPU
					// profiles automatically if CPU usage is high. Historically, we
					// observed CPU going as high as 100%, e.g. see issue #111160.
					// TODO(pavelkalinnikov): enable CPU profiling in all roachtests.
					for _, stmt := range append(sp.setUpStmts,
						"SET CLUSTER SETTING server.cpu_profile.duration = '2s'",
						"SET CLUSTER SETTING server.cpu_profile.cpu_usage_combined_threshold = 80",
					) {
						_, err := db.Exec(stmt)
						if err != nil {
							return errors.Wrapf(err, "error executing setup stmt [%s]", stmt)
						}
					}

					t.Status(`running restore`)
					metricCollector := rd.initRestorePerfMetrics(ctx, durationGauge)
					if err := rd.run(ctx, ""); err != nil {
						return err
					}
					metricCollector()
					rd.checkFingerprint(ctx)
					return nil
				})
				m.Wait()
			},
		})
	}
}

var defaultHardware = hardwareSpecs{
	cpus:       8,
	nodes:      4,
	volumeSize: 1000,
}

// hardwareSpecs define the cluster setup for a restore roachtest. These values
// should not get updated as the test runs.
type hardwareSpecs struct {

	// cpus is the per node cpu count.
	cpus int

	// nodes is the number of crdb nodes in the restore.
	nodes int

	// addWorkloadNode is true if workload node should also get spun up
	workloadNode bool

	// volumeSize indicates the size of per node block storage (pd-ssd for gcs,
	// ebs for aws). If zero, local ssd's are used.
	volumeSize int
	// ebsThroughput is the min provisioned throughput of the EBS volume, in MB/s.
	// TODO(pavelkalinnikov): support provisioning throughput not only on EBS.
	ebsThroughput int

	// mem is the memory per cpu.
	mem spec.MemPerCPU

	// Availability zones to use. (Values are cloud-provider-specific.)
	// If unset, the first of the default availability zones for the provider will be used.
	zones []string
}

func (hw hardwareSpecs) makeClusterSpecs(r registry.Registry, backupCloud string) spec.ClusterSpec {
	clusterOpts := make([]spec.Option, 0)
	clusterOpts = append(clusterOpts, spec.CPU(hw.cpus))
	if hw.volumeSize != 0 {
		clusterOpts = append(clusterOpts, spec.VolumeSize(hw.volumeSize))
	}
	if hw.mem != spec.Auto {
		clusterOpts = append(clusterOpts, spec.Mem(hw.mem))
	}
	addWorkloadNode := 0
	if hw.workloadNode {
		addWorkloadNode++
	}
	if len(hw.zones) > 0 {
		clusterOpts = append(clusterOpts, spec.Zones(strings.Join(hw.zones, ",")))
		clusterOpts = append(clusterOpts, spec.Geo())
	}
	s := r.MakeClusterSpec(hw.nodes+addWorkloadNode, clusterOpts...)

	if hw.ebsThroughput != 0 {
		s.AWSVolumeThroughput = hw.ebsThroughput
	}

	if backupCloud == spec.AWS && s.Cloud == spec.AWS && s.VolumeSize != 0 {
		// Work around an issue that RAID0s local NVMe and GP3 storage together:
		// https://github.com/cockroachdb/cockroach/issues/98783.
		//
		// TODO(srosenberg): Remove this workaround when 98783 is addressed.
		s.InstanceType, _ = spec.AWSMachineType(s.CPUs, s.Mem, s.PreferLocalSSD && s.VolumeSize == 0, vm.ArchAMD64)
		s.InstanceType = strings.Replace(s.InstanceType, "d.", ".", 1)
		s.Arch = vm.ArchAMD64
	}
	return s
}

// String prints the hardware specs. If verbose==true, verbose specs are printed.
func (hw hardwareSpecs) String(verbose bool) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("/nodes=%d", hw.nodes))
	builder.WriteString(fmt.Sprintf("/cpus=%d", hw.cpus))
	if hw.mem != spec.Auto {
		builder.WriteString(fmt.Sprintf("/%smem", hw.mem))
	}
	if len(hw.zones) > 0 {
		builder.WriteString(fmt.Sprintf("/zones=%s", strings.Join(hw.zones, ",")))
	}
	if verbose {
		builder.WriteString(fmt.Sprintf("/volSize=%dGB", hw.volumeSize))
	}
	return builder.String()
}

func (hw hardwareSpecs) getWorkloadNode() int {
	if !hw.workloadNode {
		panic(`this test does not have a workload node`)
	}
	return hw.nodes + 1
}

func (hw hardwareSpecs) getCRDBNodes() option.NodeListOption {
	nodes := make(option.NodeListOption, hw.nodes)
	for i := range nodes {
		nodes[i] = i + 1
	}
	return nodes
}

// makeHardwareSpecs instantiates hardware specs for a restore roachtest.
// Unless the caller provides any explicit specs, the default specs are used.
func makeHardwareSpecs(override hardwareSpecs) hardwareSpecs {
	specs := defaultHardware
	if override.cpus != 0 {
		specs.cpus = override.cpus
	}
	if override.nodes != 0 {
		specs.nodes = override.nodes
	}
	if override.mem != spec.Auto {
		specs.mem = override.mem
	}
	if override.volumeSize != 0 {
		specs.volumeSize = override.volumeSize
	}
	if override.ebsThroughput != 0 {
		specs.ebsThroughput = override.ebsThroughput
	}
	specs.zones = override.zones
	specs.workloadNode = override.workloadNode
	return specs
}

var defaultRestoringBackupSpecs = backupSpecs{
	// TODO(msbutler): write a script that automatically finds the latest versioned fixture.
	version:          "v22.2.0",
	cloud:            spec.AWS,
	backupProperties: "inc-count=48",
	fullBackupDir:    "LATEST",
	backupsIncluded:  12,
	workload:         tpceRestore{customers: 25000},
}

// backupSpecs define the backup that will get restored. These values should not
// get updated during the test.
type backupSpecs struct {
	// version specifies the crdb version the backup was taken on.
	version string

	// cloud is the cloud storage provider the backup is stored on.
	cloud string

	// backupProperties identifies specific backup properties included in the backup fixture
	// path.
	backupProperties string

	// specifies the full backup directory in the collection to restore from.
	fullBackupDir string

	// specifies the number of backups in the chain to restore from
	backupsIncluded int

	// workload defines the backed up workload.
	workload backupWorkload
}

// String returns a stringified version of the backup specs. Note that the
// backup version, backup directory, and AOST are never included.
//
// TODO(msbutler): the semantics around specifying backupsIncluded and backupProperties is real
// confusing. Simplify this.
func (bs backupSpecs) String(verbose bool) string {
	var builder strings.Builder
	builder.WriteString("/" + bs.workload.String())

	if verbose || bs.backupProperties != defaultRestoringBackupSpecs.backupProperties {
		builder.WriteString("/" + bs.backupProperties)
	}
	builder.WriteString("/" + bs.cloud)

	if verbose || bs.backupsIncluded != defaultRestoringBackupSpecs.backupsIncluded {
		builder.WriteString("/" + fmt.Sprintf("backupsIncluded=%d", bs.backupsIncluded))
	}
	return builder.String()
}

func (bs backupSpecs) storagePrefix() string {
	if bs.cloud == spec.AWS {
		return "s3"
	}
	return "gs"
}

func (bs backupSpecs) backupCollection() string {
	// N.B. AWS buckets are _regional_ whereas GCS buckets are _multi-regional_. Thus, in order to avoid egress (cost),
	// we use us-east-2 for AWS, which is the default region for all roachprod clusters. (See roachprod/vm/aws/aws.go)
	switch bs.storagePrefix() {
	case "s3":
		return fmt.Sprintf(`'s3://cockroach-fixtures-us-east-2/backups/%s/%s/%s?AUTH=implicit'`,
			bs.workload.fixtureDir(), bs.version, bs.backupProperties)
	case "gs":
		return fmt.Sprintf(`'gs://cockroach-fixtures/backups/%s/%s/%s?AUTH=implicit'`,
			bs.workload.fixtureDir(), bs.version, bs.backupProperties)
	default:
		panic(fmt.Sprintf("unknown storage prefix: %s", bs.storagePrefix()))
	}
}

// getAOSTCmd returns a sql cmd that will return a system time that is equal to the end time of
// the bs.backupsIncluded'th backup in the target backup chain.
func (bs backupSpecs) getAostCmd() string {
	return fmt.Sprintf(
		`SELECT max(end_time) FROM [SELECT DISTINCT end_time FROM [SHOW BACKUP FROM %s IN %s] ORDER BY end_time LIMIT %d]`,
		bs.fullBackupDir,
		bs.backupCollection(),
		bs.backupsIncluded)
}

func makeBackupSpecs(override backupSpecs, specs backupSpecs) backupSpecs {
	if override.cloud != "" {
		specs.cloud = override.cloud
	}
	if override.version != "" {
		specs.version = override.version
	}

	if override.backupProperties != "" {
		specs.backupProperties = override.backupProperties
	}

	if override.fullBackupDir != "" {
		specs.fullBackupDir = override.fullBackupDir
	}

	if override.backupsIncluded != 0 {
		specs.backupsIncluded = override.backupsIncluded
	}

	if override.workload != nil {
		specs.workload = override.workload
	}
	return specs
}

// makeRestoringBackupSpecs initializes the default restoring backup specs. The caller can override
// any of the default backup specs by passing any non-nil params.
func makeRestoringBackupSpecs(override backupSpecs) backupSpecs {
	return makeBackupSpecs(override, defaultRestoringBackupSpecs)
}

type backupWorkload interface {
	fixtureDir() string
	String() string

	// DatabaseName specifies the name of the database the workload will operate on.
	DatabaseName() string

	// init loads the cluster with the workload's schema and initial data.
	init(ctx context.Context, t test.Test, c cluster.Cluster, sp hardwareSpecs)

	// run begins a workload that runs indefinitely until the passed context
	// is cancelled.
	run(ctx context.Context, t test.Test, c cluster.Cluster, sp hardwareSpecs) error
}

type tpceRestore struct {
	customers int
	spec      *tpceSpec
}

func (tpce tpceRestore) getSpec(
	ctx context.Context, t test.Test, c cluster.Cluster, sp hardwareSpecs,
) *tpceSpec {
	if tpce.spec != nil {
		return tpce.spec
	}
	tpceSpec, err := initTPCESpec(ctx, t.L(), c, sp.getWorkloadNode(), sp.getCRDBNodes())
	require.NoError(t, err)
	return tpceSpec
}

func (tpce tpceRestore) init(
	ctx context.Context, t test.Test, c cluster.Cluster, sp hardwareSpecs,
) {
	spec := tpce.getSpec(ctx, t, c, sp)
	spec.init(ctx, t, c, tpceCmdOptions{
		customers: tpce.customers,
		racks:     sp.nodes})
}

func (tpce tpceRestore) run(
	ctx context.Context, t test.Test, c cluster.Cluster, sp hardwareSpecs,
) error {
	spec := tpce.getSpec(ctx, t, c, sp)
	_, err := spec.run(ctx, t, c, tpceCmdOptions{
		// Set the duration to be a week to ensure the workload never exits early.
		duration:  time.Hour * 7 * 24,
		customers: tpce.customers,
		racks:     sp.nodes,
		threads:   sp.cpus * sp.nodes})
	return err
}

func (tpce tpceRestore) fixtureDir() string {
	return fmt.Sprintf(`tpc-e/customers=%d`, tpce.customers)
}

func (tpce tpceRestore) String() string {
	var builder strings.Builder
	builder.WriteString("tpce/")
	switch tpce.customers {
	case 1000:
		builder.WriteString("15GB")
	case 5000:
		builder.WriteString("80GB")
	case 25000:
		builder.WriteString("400GB")
	case 500000:
		builder.WriteString("8TB")
	case 2000000:
		builder.WriteString("32TB")
	default:
		panic("tpce customer count not recognized")
	}
	return builder.String()
}

func (tpce tpceRestore) DatabaseName() string {
	return "tpce"
}

// restoreSpecs define input parameters to a restore roachtest set during
// registration. They should not be modified within test_spec.run(), as they are shared
// across driver runs.
type restoreSpecs struct {
	hardware hardwareSpecs
	backup   backupSpecs
	timeout  time.Duration
	clouds   registry.CloudSet
	suites   registry.SuiteSet
	tags     map[string]struct{}

	// namePrefix appears in the name of the roachtest, i.e. `restore/{prefix}/{config}`.
	namePrefix string

	// fingerprint, if specified, defines the expected stripped fingerprint of the
	// restored user space tables.
	fingerprint int

	testName   string
	setUpStmts []string
}

func (sp *restoreSpecs) initTestName() {
	sp.testName = sp.computeName(false)
}

func (sp *restoreSpecs) computeName(verbose bool) string {
	var prefix string
	if sp.namePrefix != "" {
		prefix = "/" + sp.namePrefix
	}
	return "restore" + prefix + sp.backup.String(verbose) + sp.hardware.String(verbose)
}

type restoreDriver struct {
	sp restoreSpecs

	t test.Test
	c cluster.Cluster

	// aost defines the "As Of System Time" used within the restore. Because this
	// gets computed during test execution, it is stored in the restoreDriver
	// rather than the restoreSpecs.
	aost string
}

func makeRestoreDriver(t test.Test, c cluster.Cluster, sp restoreSpecs) restoreDriver {
	return restoreDriver{
		t:  t,
		c:  c,
		sp: sp,
	}
}

func (rd *restoreDriver) prepareCluster(ctx context.Context) {
	if rd.c.Spec().Cloud != rd.sp.backup.cloud {
		// For now, only run the test on the cloud provider that also stores the backup.
		rd.t.Skipf("test configured to run on %s", rd.sp.backup.cloud)
	}
	rd.c.Put(ctx, rd.t.Cockroach(), "./cockroach")
	rd.c.Start(ctx, rd.t.L(), option.DefaultStartOptsNoBackups(), install.MakeClusterSettings())
	rd.getAOST(ctx)

	if rand.Intn(2) == 0 {
		rd.t.L().Printf("Running non-default makeSimpleImportSpans")
		conn := rd.c.Conn(ctx, rd.t.L(), 1)
		_, err := conn.ExecContext(ctx, "SET CLUSTER SETTING bulkio.restore.simple_import_spans.enabled = true")
		require.NoError(rd.t, err)
	}
}

// getAOST gets the AOST to use in the restore cmd.
func (rd *restoreDriver) getAOST(ctx context.Context) {
	var aost string
	conn := rd.c.Conn(ctx, rd.t.L(), 1)
	err := conn.QueryRowContext(ctx, rd.sp.backup.getAostCmd()).Scan(&aost)
	require.NoError(rd.t, err)
	rd.aost = aost
}

func (rd *restoreDriver) restoreCmd(target, opts string) string {
	return fmt.Sprintf(`RESTORE %s FROM %s IN %s AS OF SYSTEM TIME '%s' %s`,
		target, rd.sp.backup.fullBackupDir, rd.sp.backup.backupCollection(), rd.aost, opts)
}

// run executes the restore, where target injects a restore target into the restore command.
// Examples:
// - "DATABASE tpce" will execute a database restore on the tpce cluster.
// - "" will execute a cluster restore.
func (rd *restoreDriver) run(ctx context.Context, target string) error {
	conn, err := rd.c.ConnE(ctx, rd.t.L(), 1)
	if err != nil {
		return errors.Wrapf(err, "failed to connect to node 1; running restore")
	}
	_, err = conn.ExecContext(ctx, rd.restoreCmd(target, ""))
	return err
}

func (rd *restoreDriver) runDetached(
	ctx context.Context, target string, node int,
) (jobspb.JobID, error) {
	db, err := rd.c.ConnE(ctx, rd.t.L(), rd.c.Node(node)[0])
	if err != nil {
		return 0, errors.Wrapf(err, "failed to connect to node %d; running restore detached", node)
	}
	if _, err = db.ExecContext(ctx, rd.restoreCmd(target,
		"WITH DETACHED")); err != nil {
		return 0, err
	}
	var jobID jobspb.JobID
	if err := db.QueryRow(`SELECT job_id FROM [SHOW JOBS] WHERE job_type = 'RESTORE'`).Scan(&jobID); err != nil {
		return 0, err
	}
	return jobID, nil
}

// initRestorePerfMetrics returns a function that will collect restore throughput at the end of
// the test.
//
// TODO(msbutler): only export metrics to test-eng prometheus server once it begins scraping
// nightly roachtest runs.
func (rd *restoreDriver) initRestorePerfMetrics(
	ctx context.Context, durationGauge *prometheus.GaugeVec,
) func() {
	dut, err := roachtestutil.NewDiskUsageTracker(rd.c, rd.t.L())
	require.NoError(rd.t, err)
	startTime := timeutil.Now()
	startDu := dut.GetDiskUsage(ctx, rd.c.All())

	return func() {
		promLabel := registry.PromSub(strings.Replace(rd.sp.testName, "restore/", "", 1)) + "_seconds"
		testDuration := timeutil.Since(startTime).Seconds()
		durationGauge.WithLabelValues(promLabel).Set(testDuration)

		// compute throughput as MB / node / second.
		du := dut.GetDiskUsage(ctx, rd.c.All())
		throughput := float64(du-startDu) / (float64(rd.sp.hardware.nodes) * testDuration)
		rd.t.L().Printf("Usage %d , Nodes %d , Duration %f\n; Throughput: %f mb / node / second",
			du,
			rd.sp.hardware.nodes,
			testDuration,
			throughput)
		exportToRoachperf(ctx, rd.t, rd.c, rd.sp.testName, int64(throughput))
	}
}

// checkFingerprint runs a stripped fingerprint on all user tables in the cluster if the restore
// spec has a nonzero fingerprint.
func (rd *restoreDriver) checkFingerprint(ctx context.Context) {
	if rd.sp.fingerprint == 0 {
		rd.t.L().Printf("Fingerprint not found in specs. Skipping fingerprint check.")
		return
	}

	conn, err := rd.c.ConnE(ctx, rd.t.L(), rd.c.Node(1)[0])
	require.NoError(rd.t, err)
	sql := sqlutils.MakeSQLRunner(conn)

	var minUserTableID, maxUserTableID uint32
	sql.QueryRow(rd.t, `SELECT min(id), max(id) FROM system.namespace WHERE "parentID" >1`).Scan(
		&minUserTableID, &maxUserTableID)

	codec := keys.MakeSQLCodec(roachpb.SystemTenantID)
	startKey := codec.TablePrefix(minUserTableID)
	endkey := codec.TablePrefix(maxUserTableID).PrefixEnd()

	startTime := timeutil.Now()
	var fingerprint int
	sql.QueryRow(rd.t, `SELECT * FROM crdb_internal.fingerprint(ARRAY[$1::BYTES, $2::BYTES],true)`,
		startKey, endkey).Scan(&fingerprint)
	rd.t.L().Printf("Fingerprint is %d. Took %.2f minutes", fingerprint,
		timeutil.Since(startTime).Minutes())
	require.Equal(rd.t, rd.sp.fingerprint, fingerprint, "user table fingerprint mismatch")
}

// exportToRoachperf exports a single perf metric for the given test to roachperf.
func exportToRoachperf(
	ctx context.Context, t test.Test, c cluster.Cluster, testName string, metric int64,
) {

	// The easiest way to record a precise metric for roachperf is to caste it as a duration,
	// in seconds in the histogram's upper bound.
	reg := histogram.NewRegistry(
		time.Duration(metric)*time.Second,
		histogram.MockWorkloadName,
	)
	bytesBuf := bytes.NewBuffer([]byte{})
	jsonEnc := json.NewEncoder(bytesBuf)

	// Ensure the histogram contains the name of the roachtest
	reg.GetHandle().Get(testName)

	// Serialize the histogram into the buffer
	reg.Tick(func(tick histogram.Tick) {
		_ = jsonEnc.Encode(tick.Snapshot())
	})
	// Upload the perf artifacts to any one of the nodes so that the test
	// runner copies it into an appropriate directory path.
	dest := filepath.Join(t.PerfArtifactsDir(), "stats.json")
	if err := c.RunE(ctx, c.Node(1), "mkdir -p "+filepath.Dir(dest)); err != nil {
		log.Errorf(ctx, "failed to create perf dir: %+v", err)
	}
	if err := c.PutString(ctx, bytesBuf.String(), dest, 0755, c.Node(1)); err != nil {
		log.Errorf(ctx, "failed to upload perf artifacts to node: %s", err.Error())
	}
}

// verifyMetrics loops, retrieving the timeseries metrics specified in m every
// 10s and verifying that the most recent value is less that the limit
// specified in m. This is particularly useful for verifying that a counter
// metric does not exceed some threshold during a test. For example, the
// restore and import tests verify that the range merge queue is inactive.
func verifyMetrics(
	ctx context.Context, t test.Test, c cluster.Cluster, m map[string]float64,
) error {
	const sample = 10 * time.Second
	// Query needed information over the timespan of the query.
	adminUIAddrs, err := c.ExternalAdminUIAddr(ctx, t.L(), c.Node(1))
	if err != nil {
		return err
	}
	url := "http://" + adminUIAddrs[0] + "/ts/query"

	request := tspb.TimeSeriesQueryRequest{
		// Ask for one minute intervals. We can't just ask for the whole hour
		// because the time series query system does not support downsampling
		// offsets.
		SampleNanos: sample.Nanoseconds(),
	}
	for name := range m {
		request.Queries = append(request.Queries, tspb.Query{
			Name:             name,
			Downsampler:      tspb.TimeSeriesQueryAggregator_AVG.Enum(),
			SourceAggregator: tspb.TimeSeriesQueryAggregator_SUM.Enum(),
		})
	}

	ticker := time.NewTicker(sample)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		now := timeutil.Now()
		request.StartNanos = now.Add(-sample * 3).UnixNano()
		request.EndNanos = now.UnixNano()

		var response tspb.TimeSeriesQueryResponse
		if err := httputil.PostJSON(http.Client{}, url, &request, &response); err != nil {
			return err
		}

		for i := range request.Queries {
			name := request.Queries[i].Name
			data := response.Results[i].Datapoints
			n := len(data)
			if n == 0 {
				continue
			}
			limit := m[name]
			value := data[n-1].Value
			if value >= limit {
				return fmt.Errorf("%s: %.1f >= %.1f @ %d", name, value, limit, data[n-1].TimestampNanos)
			}
		}
	}
}

// TODO(peter): silence unused warning.
var _ = verifyMetrics
