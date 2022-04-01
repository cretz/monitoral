package hostworker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"crawshaw.io/sqlite"
	"crawshaw.io/sqlite/sqlitex"
	"github.com/cretz/monitoral/monitoralpb"
	"github.com/pierrec/lz4/v4"
	"go.temporal.io/sdk/activity"
)

const dbPoolSize = 10

type host struct {
	*hostOptions
	dbPool                 *sqlitex.Pool
	metricManager          *metricManager
	running                bool
	unexpectedFailureErrCh chan error
	// Only needed to store reference
	prevSer *sqlite.Serialized
}

type hostOptions struct {
	client          monitoralpb.Client
	hostname        string
	systemTaskQueue string
	tags            []string
}

func newHost(hostOptions *hostOptions) (*host, error) {
	dbPool, err := sqlitex.Open("file::memory:?mode=memory", 0, dbPoolSize)
	if err != nil {
		return nil, err
	}
	ret := &host{
		hostOptions:            hostOptions,
		dbPool:                 dbPool,
		unexpectedFailureErrCh: make(chan error, 1),
	}
	ret.metricManager = newMetricManager(ret)
	return ret, nil
}

func (h *host) run(ctx context.Context, req *monitoralpb.HostRunRequest) (resp *monitoralpb.HostRunRequest, actErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// If we're already running, fail (we accept that this isn't thread safe)
	if h.running {
		return nil, fmt.Errorf("already running")
	}
	h.running = true
	defer func() { h.running = false }()

	// If we fail unexpectedly, populate channel
	defer func() {
		if actErr != nil {
			// Non-blocking just in case no room (should never happen)
			select {
			case h.unexpectedFailureErrCh <- actErr:
			default:
			}
		}
	}()

	// Set configs if newer
	if err := h.metricManager.updateConfigs(req.ConfigMutationSeq, req.Configs); err != nil {
		return nil, fmt.Errorf("failed updating config: %w", err)
	}

	// Deserialize if there is a previously serialized version
	if len(req.PrevRunSnapshot) > 0 {
		if err := h.deserializeDB(ctx, req.PrevRunSnapshot); err != nil {
			return nil, fmt.Errorf("failed deserializing previous snapshot: %w", err)
		}
	}

	// Start the metrics
	metricErrCh := make(chan error, 1)
	go func() { metricErrCh <- h.metricManager.run(ctx) }()

	// Run until context closed or worker stopped
	var metricErr error
	metricManagerDone := false
	select {
	case <-ctx.Done():
	case <-activity.GetWorkerStopChannel(ctx):
	case metricErr = <-metricErrCh:
		metricManagerDone = true
		// If the context isn't done, return this as an error
		if ctx.Err() == nil {
			return nil, metricErr
		}
	}

	// If metric manager not done, wait just a bit
	cancel()
	if !metricManagerDone {
		select {
		case <-time.After(10 * time.Second):
			return nil, fmt.Errorf("metric manager did not complete after 10 seconds")
		case <-metricErrCh:
			// We don't care about the error
		}
	}

	// Serialize the DB and return
	panic("TODO")
}

func (h *host) close() {
	if h.dbPool != nil {
		// Ignore error
		h.dbPool.Close()
	}
	h.prevSer = nil
}

func (h *host) deserializeDB(ctx context.Context, b []byte) error {
	// Uncompress
	r := lz4.NewReader(bytes.NewReader(b))
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	// Need to store a reference to this (we accept this isn't thread safe)
	h.prevSer = sqlite.NewSerialized("", b, true)
	db := h.dbPool.Get(ctx)
	defer h.dbPool.Put(db)
	return db.Deserialize(h.prevSer, sqlite.SQLITE_DESERIALIZE_FREEONCLOSE|sqlite.SQLITE_DESERIALIZE_RESIZEABLE)
}

func (h *host) updateConfig(ctx context.Context, req *monitoralpb.HostUpdateConfigRequest) error {
	panic("TODO")
}

func (h *host) collect(ctx context.Context, req *monitoralpb.HostCollectRequest) (*monitoralpb.HostCollectResponse, error) {
	panic("TODO")
}
