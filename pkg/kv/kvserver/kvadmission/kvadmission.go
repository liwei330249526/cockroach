// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// Package kvadmission is the integration layer between KV and admission
// control.
package kvadmission

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/util/admission"
	"github.com/cockroachdb/cockroach/pkg/util/admission/admissionpb"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble"
)

// elasticCPUDurationPerExportRequest controls how many CPU tokens are allotted
// for each export request.
var elasticCPUDurationPerExportRequest = settings.RegisterDurationSetting(
	settings.SystemOnly,
	"kvadmission.elastic_cpu.duration_per_export_request",
	"controls how many CPU tokens are allotted for each export request",
	admission.MaxElasticCPUDuration,
	func(duration time.Duration) error {
		if duration < admission.MinElasticCPUDuration {
			return fmt.Errorf("minimum CPU duration allowed per export request is %s, got %s",
				admission.MinElasticCPUDuration, duration)
		}
		if duration > admission.MaxElasticCPUDuration {
			return fmt.Errorf("maximum CPU duration allowed per export request is %s, got %s",
				admission.MaxElasticCPUDuration, duration)
		}
		return nil
	},
)

// elasticCPUDurationPerRangefeedScanUnit controls how many CPU tokens are
// allotted for each unit of work during rangefeed catchup scans.
var elasticCPUDurationPerRangefeedScanUnit = settings.RegisterDurationSetting(
	settings.SystemOnly,
	"kvadmission.elastic_cpu.duration_per_rangefeed_scan_unit",
	"controls how many CPU tokens are allotted for each unit of work during rangefeed catchup scans",
	admission.MaxElasticCPUDuration,
	func(duration time.Duration) error {
		if duration < admission.MinElasticCPUDuration {
			return fmt.Errorf("minimum CPU duration allowed is %s, got %s",
				admission.MinElasticCPUDuration, duration)
		}
		if duration > admission.MaxElasticCPUDuration {
			return fmt.Errorf("maximum CPU duration allowed is %s, got %s",
				admission.MaxElasticCPUDuration, duration)
		}
		return nil
	},
)

// Controller provides admission control for the KV layer.
type Controller interface {
	// AdmitKVWork must be called before performing KV work.
	// BatchRequest.AdmissionHeader and BatchRequest.Replica.StoreID must be
	// populated for admission to work correctly. If err is non-nil, the
	// returned handle can be ignored. If err is nil, AdmittedKVWorkDone must be
	// called after the KV work is done executing.
	AdmitKVWork(context.Context, roachpb.TenantID, *roachpb.BatchRequest) (Handle, error)
	// AdmittedKVWorkDone is called after the admitted KV work is done
	// executing.
	AdmittedKVWorkDone(Handle, *StoreWriteBytes)
	// AdmitRangefeedRequest must be called before serving rangefeed requests.
	// It returns a Pacer that's used within rangefeed catchup scans (typically
	// CPU-intensive and affects scheduling latencies negatively).
	AdmitRangefeedRequest(roachpb.TenantID, *roachpb.RangeFeedRequest) *Pacer
	// SetTenantWeightProvider is used to set the provider that will be
	// periodically polled for weights. The stopper should be used to terminate
	// the periodic polling.
	SetTenantWeightProvider(TenantWeightProvider, *stop.Stopper)
	// SnapshotIngested informs admission control about a range snapshot
	// ingestion.
	SnapshotIngested(roachpb.StoreID, pebble.IngestOperationStats)
	// FollowerStoreWriteBytes informs admission control about writes
	// replicated to a raft follower, that have not been subject to admission
	// control.
	FollowerStoreWriteBytes(roachpb.StoreID, FollowerStoreWriteBytes)
}

// TenantWeightProvider can be periodically asked to provide the tenant
// weights.
type TenantWeightProvider interface {
	GetTenantWeights() TenantWeights
}

// TenantWeights contains the various tenant weights.
type TenantWeights struct {
	// Node is the node level tenant ID => weight.
	Node map[uint64]uint32
	// Stores contains the per-store tenant weights.
	Stores []TenantWeightsForStore
}

// TenantWeightsForStore contains the tenant weights for a store.
type TenantWeightsForStore struct {
	roachpb.StoreID
	// Weights is tenant ID => weight.
	Weights map[uint64]uint32
}

// controllerImpl implements Controller interface.
type controllerImpl struct {
	// Admission control queues and coordinators. All three should be nil or
	// non-nil.
	kvAdmissionQ        *admission.WorkQueue
	storeGrantCoords    *admission.StoreGrantCoordinators
	elasticCPUWorkQueue *admission.ElasticCPUWorkQueue
	settings            *cluster.Settings
	every               log.EveryN
}

var _ Controller = &controllerImpl{}

// Handle groups data around some piece admitted work. Depending on the
// type of work, it holds (a) references to specific work queues, (b) state
// needed to inform said work queues of what work was done after the fact, and
// (c) information around how much work a request is allowed to do (used for
// cooperative scheduling with elastic CPU granters).
type Handle struct {
	tenantID             roachpb.TenantID
	storeAdmissionQ      *admission.StoreWorkQueue
	storeWorkHandle      admission.StoreWorkHandle
	ElasticCPUWorkHandle *admission.ElasticCPUWorkHandle

	callAdmittedWorkDoneOnKVAdmissionQ bool
}

// MakeController returns a Controller. All three parameters must together be
// nil or non-nil.
func MakeController(
	kvAdmissionQ *admission.WorkQueue,
	elasticCPUWorkQueue *admission.ElasticCPUWorkQueue,
	storeGrantCoords *admission.StoreGrantCoordinators,
	settings *cluster.Settings,
) Controller {
	return &controllerImpl{
		kvAdmissionQ:        kvAdmissionQ,
		storeGrantCoords:    storeGrantCoords,
		elasticCPUWorkQueue: elasticCPUWorkQueue,
		settings:            settings,
		every:               log.Every(10 * time.Second),
	}
}

// AdmitKVWork implements the Controller interface.
//
// TODO(irfansharif): There's a fair bit happening here and there's no test
// coverage. Fix that.
func (n *controllerImpl) AdmitKVWork(
	ctx context.Context, tenantID roachpb.TenantID, ba *roachpb.BatchRequest,
) (handle Handle, retErr error) {
	ah := Handle{tenantID: tenantID}
	if n.kvAdmissionQ == nil {
		return ah, nil
	}

	bypassAdmission := ba.IsAdmin()
	source := ba.AdmissionHeader.Source
	if !roachpb.IsSystemTenantID(tenantID.ToUint64()) {
		// Request is from a SQL node.
		bypassAdmission = false
		source = roachpb.AdmissionHeader_FROM_SQL
	}
	if source == roachpb.AdmissionHeader_OTHER {
		bypassAdmission = true
	}
	// TODO(abaptist): Revisit and deprecate this setting in v23.1.
	if admission.KVBulkOnlyAdmissionControlEnabled.Get(&n.settings.SV) {
		if admissionpb.WorkPriority(ba.AdmissionHeader.Priority) >= admissionpb.NormalPri {
			bypassAdmission = true
		}
	}
	createTime := ba.AdmissionHeader.CreateTime
	if !bypassAdmission && createTime == 0 {
		// TODO(sumeer): revisit this for multi-tenant. Specifically, the SQL use
		// of zero CreateTime needs to be revisited. It should use high priority.
		createTime = timeutil.Now().UnixNano()
	}
	admissionInfo := admission.WorkInfo{
		TenantID:        tenantID,
		Priority:        admissionpb.WorkPriority(ba.AdmissionHeader.Priority),
		CreateTime:      createTime,
		BypassAdmission: bypassAdmission,
	}

	admissionEnabled := true
	// Don't subject HeartbeatTxnRequest to the storeAdmissionQ. Even though
	// it would bypass admission, it would consume a slot. When writes are
	// throttled, we start generating more txn heartbeats, which then consume
	// all the slots, causing no useful work to happen. We do want useful work
	// to continue even when throttling since there are often significant
	// number of tokens available.
	if ba.IsWrite() && !ba.IsSingleHeartbeatTxnRequest() {
		storeAdmissionQ := n.storeGrantCoords.TryGetQueueForStore(int32(ba.Replica.StoreID))
		if storeAdmissionQ != nil {
			storeWorkHandle, err := storeAdmissionQ.Admit(
				ctx, admission.StoreWriteWorkInfo{WorkInfo: admissionInfo})
			if err != nil {
				return Handle{}, err
			}
			admissionEnabled = storeWorkHandle.AdmissionEnabled()
			if admissionEnabled {
				defer func() {
					if retErr != nil {
						// No bytes were written.
						_ = storeAdmissionQ.AdmittedWorkDone(ah.storeWorkHandle, admission.StoreWorkDoneInfo{})
					}
				}()
				ah.storeAdmissionQ, ah.storeWorkHandle = storeAdmissionQ, storeWorkHandle
			}
		}
	}
	if admissionEnabled {
		if ba.IsSingleExportRequest() {
			// Backups generate batches with single export requests, which we
			// admit through the elastic CPU work queue. We grant this
			// CPU-intensive work a set amount of CPU time and expect it to
			// terminate (cooperatively) once it exceeds its grant. The amount
			// disbursed is 100ms, which we've experimentally found to be long
			// enough to do enough useful work per-request while not causing too
			// much in the way of scheduling delays on individual cores. Within
			// admission control we have machinery that observes scheduling
			// latencies periodically and reduces the total amount of CPU time
			// handed out through this mechanism, as a way to provide latency
			// isolation to non-elastic ("latency sensitive") work running on
			// the same machine.
			elasticWorkHandle, err := n.elasticCPUWorkQueue.Admit(
				ctx, elasticCPUDurationPerExportRequest.Get(&n.settings.SV), admissionInfo,
			)
			if err != nil {
				return Handle{}, err
			}
			ah.ElasticCPUWorkHandle = elasticWorkHandle
			defer func() {
				if retErr != nil {
					// No elastic work was done.
					n.elasticCPUWorkQueue.AdmittedWorkDone(ah.ElasticCPUWorkHandle)
				}
			}()
		} else {
			callAdmittedWorkDoneOnKVAdmissionQ, err := n.kvAdmissionQ.Admit(ctx, admissionInfo)
			if err != nil {
				return Handle{}, err
			}
			ah.callAdmittedWorkDoneOnKVAdmissionQ = callAdmittedWorkDoneOnKVAdmissionQ
		}
	}
	return ah, nil
}

// AdmittedKVWorkDone implements the Controller interface.
func (n *controllerImpl) AdmittedKVWorkDone(ah Handle, writeBytes *StoreWriteBytes) {
	n.elasticCPUWorkQueue.AdmittedWorkDone(ah.ElasticCPUWorkHandle)
	if ah.callAdmittedWorkDoneOnKVAdmissionQ {
		n.kvAdmissionQ.AdmittedWorkDone(ah.tenantID)
	}
	if ah.storeAdmissionQ != nil {
		var doneInfo admission.StoreWorkDoneInfo
		if writeBytes != nil {
			doneInfo = admission.StoreWorkDoneInfo(*writeBytes)
		}
		err := ah.storeAdmissionQ.AdmittedWorkDone(ah.storeWorkHandle, doneInfo)
		if err != nil {
			// This shouldn't be happening.
			if buildutil.CrdbTestBuild {
				log.Fatalf(context.Background(), "%s", errors.WithAssertionFailure(err))
			}
			if n.every.ShouldLog() {
				log.Errorf(context.Background(), "%s", err)
			}
		}
	}
}

// AdmitRangefeedRequest implements the Controller interface.
func (n *controllerImpl) AdmitRangefeedRequest(
	tenantID roachpb.TenantID, request *roachpb.RangeFeedRequest,
) *Pacer {
	// TODO(irfansharif): We need to version gate/be defensive when integrating
	// rangefeeds since admission headers will not be fully set on older version
	// nodes. See EnableRangefeedElasticCPUControl in cockroach_versions.go.
	// Consider a cluster setting too.

	return &Pacer{
		unit: elasticCPUDurationPerRangefeedScanUnit.Get(&n.settings.SV),
		wi: admission.WorkInfo{
			TenantID:        tenantID,
			Priority:        admissionpb.WorkPriority(request.AdmissionHeader.Priority),
			CreateTime:      request.AdmissionHeader.CreateTime,
			BypassAdmission: false,
		},
		wq: n.elasticCPUWorkQueue,
	}
}

// SetTenantWeightProvider implements the Controller interface.
func (n *controllerImpl) SetTenantWeightProvider(
	provider TenantWeightProvider, stopper *stop.Stopper,
) {
	// TODO(irfansharif): Use a stopper here instead.
	go func() {
		const weightCalculationPeriod = 10 * time.Minute
		ticker := time.NewTicker(weightCalculationPeriod)
		// Used for short-circuiting the weights calculation if all weights are
		// disabled.
		allWeightsDisabled := false
		for {
			select {
			case <-ticker.C:
				kvDisabled := !admission.KVTenantWeightsEnabled.Get(&n.settings.SV)
				kvStoresDisabled := !admission.KVStoresTenantWeightsEnabled.Get(&n.settings.SV)
				if allWeightsDisabled && kvDisabled && kvStoresDisabled {
					// Have already transitioned to disabled, so noop.
					continue
				}
				weights := provider.GetTenantWeights()
				if kvDisabled {
					weights.Node = nil
				}
				n.kvAdmissionQ.SetTenantWeights(weights.Node)
				n.elasticCPUWorkQueue.SetTenantWeights(weights.Node)

				for _, storeWeights := range weights.Stores {
					q := n.storeGrantCoords.TryGetQueueForStore(int32(storeWeights.StoreID))
					if q != nil {
						if kvStoresDisabled {
							storeWeights.Weights = nil
						}
						q.SetTenantWeights(storeWeights.Weights)
					}
				}
				allWeightsDisabled = kvDisabled && kvStoresDisabled
			case <-stopper.ShouldQuiesce():
				ticker.Stop()
				return
			}
		}
	}()
}

// SnapshotIngested implements the Controller interface.
func (n *controllerImpl) SnapshotIngested(
	storeID roachpb.StoreID, ingestStats pebble.IngestOperationStats,
) {
	storeAdmissionQ := n.storeGrantCoords.TryGetQueueForStore(int32(storeID))
	if storeAdmissionQ == nil {
		return
	}
	storeAdmissionQ.StatsToIgnore(ingestStats)
}

// FollowerStoreWriteBytes implements the Controller interface.
func (n *controllerImpl) FollowerStoreWriteBytes(
	storeID roachpb.StoreID, followerWriteBytes FollowerStoreWriteBytes,
) {
	if followerWriteBytes.WriteBytes == 0 && followerWriteBytes.IngestedBytes == 0 {
		return
	}
	storeAdmissionQ := n.storeGrantCoords.TryGetQueueForStore(int32(storeID))
	if storeAdmissionQ == nil {
		return
	}
	storeAdmissionQ.BypassedWorkDone(
		followerWriteBytes.NumEntries, followerWriteBytes.StoreWorkDoneInfo)
}

// ProvisionedBandwidth set a value of the provisioned
// bandwidth for each store in the cluster.
var ProvisionedBandwidth = settings.RegisterByteSizeSetting(
	settings.SystemOnly, "kvadmission.store.provisioned_bandwidth",
	"if set to a non-zero value, this is used as the provisioned bandwidth (in bytes/s), "+
		"for each store. It can be over-ridden on a per-store basis using the --store flag",
	0).WithPublic()

// FollowerStoreWriteBytes captures stats about writes done to a store by a
// replica that is not the leaseholder. These are used for admission control.
type FollowerStoreWriteBytes struct {
	NumEntries int64
	admission.StoreWorkDoneInfo
}

// Merge follower store write statistics using the given data.
func (f *FollowerStoreWriteBytes) Merge(from FollowerStoreWriteBytes) {
	f.NumEntries += from.NumEntries
	f.WriteBytes += from.WriteBytes
	f.IngestedBytes += from.IngestedBytes
}

// StoreWriteBytes aliases admission.StoreWorkDoneInfo, since the notion of
// "work is done" is specific to admission control and doesn't need to leak
// everywhere.
type StoreWriteBytes admission.StoreWorkDoneInfo

var storeWriteBytesPool = sync.Pool{
	New: func() interface{} { return &StoreWriteBytes{} },
}

// NewStoreWriteBytes constructs a new StoreWriteBytes.
func NewStoreWriteBytes() *StoreWriteBytes {
	wb := storeWriteBytesPool.Get().(*StoreWriteBytes)
	*wb = StoreWriteBytes{}
	return wb
}

// Release returns the *StoreWriteBytes to the pool.
func (wb *StoreWriteBytes) Release() {
	if wb == nil {
		return
	}
	storeWriteBytesPool.Put(wb)
}

// Pacer is used in tight loops (CPU-bound) for non-premptible elastic work.
// Callers are expected to invoke Pace() every loop iteration and Close() once
// done. Internally this type integrates with elastic CPU work queue, acquiring
// tokens for the CPU work being done, and blocking if tokens are unavailable.
// This allows for a form of cooperative scheduling with elastic CPU granters.
type Pacer struct {
	unit time.Duration
	wi   admission.WorkInfo
	wq   *admission.ElasticCPUWorkQueue

	cur *admission.ElasticCPUWorkHandle
}

// Pace is part of the Pacer interface.
func (p *Pacer) Pace(ctx context.Context) error {
	if p == nil {
		return nil
	}

	if overLimit, _ := p.cur.OverLimit(); overLimit {
		p.wq.AdmittedWorkDone(p.cur)
		p.cur = nil
	}

	if p.cur == nil {
		handle, err := p.wq.Admit(ctx, p.unit, p.wi)
		if err != nil {
			return err
		}
		p.cur = handle
	}
	return nil
}

// Close is part of the Pacer interface.
func (p *Pacer) Close() {
	if p == nil || p.cur == nil {
		return
	}

	p.wq.AdmittedWorkDone(p.cur)
	p.cur = nil
}

type pacerKey struct{}

// ContextWithPacer returns a Context wrapping the supplied Pacer, if any.
func ContextWithPacer(ctx context.Context, h *Pacer) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, pacerKey{}, h)
}

// PacerFromContext returns the Pacer contained in the Context, if any.
func PacerFromContext(ctx context.Context) *Pacer {
	val := ctx.Value(pacerKey{})
	h, ok := val.(*Pacer)
	if !ok {
		return nil
	}
	return h
}
