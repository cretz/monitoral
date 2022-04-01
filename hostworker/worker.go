package hostworker

import (
	"context"
	"fmt"
	"time"

	"github.com/cretz/monitoral/monitoralpb"
	"github.com/cretz/monitoral/systemworker"
	"github.com/cretz/temporal-sdk-go-advanced/temporalutil/clientutil"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

type WorkerOptions struct {
	Client          client.Client
	Hostname        string
	SystemTaskQueue string
	Tags            []string
}

type Worker interface {
	// This should be always be called to properly close and get errors
	Wait() error
	Stop()
}

func StartWorker(options WorkerOptions) (Worker, error) {
	if options.Client == nil {
		return nil, fmt.Errorf("missing client")
	} else if options.Hostname == "" {
		return nil, fmt.Errorf("missing hostname")
	} else if options.SystemTaskQueue == "" {
		return nil, fmt.Errorf("missing system task queue")
	} else if len(options.Tags) == 0 {
		return nil, fmt.Errorf("missing tags")
	}

	// Create task queue specific to this host
	taskQueue := uuid.NewString() + "-host-" + options.Hostname

	// Create a worker
	w := worker.New(options.Client, taskQueue, worker.Options{
		DisableWorkflowWorker: true,
		// We want to give a 10 second graceful stop
		WorkerStopTimeout: 10 * time.Second,
	})

	// Create the host w/ a call responder
	callRespHandler, err := clientutil.NewCallResponseHandler(clientutil.CallResponseHandlerOptions{
		TaskQueue: taskQueue,
		Worker:    w,
	})
	if err != nil {
		return nil, err
	}
	h, err := newHost(&hostOptions{
		client: monitoralpb.NewClient(monitoralpb.ClientOptions{
			Client:              options.Client,
			CallResponseHandler: callRespHandler,
		}),
		hostname:        options.Hostname,
		systemTaskQueue: options.SystemTaskQueue,
		tags:            options.Tags,
	})
	if err != nil {
		return nil, fmt.Errorf("failed creating host: %w", err)
	}

	// Register the activities on it
	monitoralpb.RegisterHostRunActivity(w, h.run)
	monitoralpb.RegisterHostUpdateConfigActivity(w, h.updateConfig)
	monitoralpb.RegisterHostCollectActivity(w, h.collect)

	// Start it, run the worker in the background, then return it
	if err := w.Start(); err != nil {
		return nil, err
	}
	ret := newHostWorker(h, w)
	go ret.run()
	return ret, nil
}

type hostWorker struct {
	host       *host
	worker     worker.Worker
	runContext context.Context
	runCancel  context.CancelFunc
	doneCh     chan error
}

func newHostWorker(host *host, worker worker.Worker) *hostWorker {
	ret := &hostWorker{
		host:   host,
		worker: worker,
		doneCh: make(chan error, 1),
	}
	ret.runContext, ret.runCancel = context.WithCancel(context.Background())
	return ret
}

func (h *hostWorker) run() {
	defer h.worker.Stop()
	defer h.runCancel()

	// Start workflow in the background
	startWorkflowDoneCh := make(chan error, 1)
	go func() { startWorkflowDoneCh <- h.startHostWorkflow() }()

	// Wait for context done, unexpected failure, or start failure
	for {
		select {
		case <-h.runContext.Done():
			return
		case err := <-h.host.unexpectedFailureErrCh:
			select {
			case h.doneCh <- err:
			default:
			}
			return
		case err := <-startWorkflowDoneCh:
			if err != nil && h.runContext.Err() == nil {
				select {
				case h.doneCh <- err:
				default:
				}
				return
			}
		}
	}
}

func (h *hostWorker) startHostWorkflow() error {
	// Grab the configs
	seq, configs, err := h.getHostConfigs()
	if err != nil {
		return err
	}

	// Start the host workflow which is ok if it already exists
	run, err := h.host.client.ExecuteHostWorkflow(
		h.runContext,
		&client.StartWorkflowOptions{
			ID:               "host-" + h.host.hostname,
			TaskQueue:        h.host.systemTaskQueue,
			SearchAttributes: (&systemworker.HostInfo{Name: h.host.hostname, Tags: h.host.tags}).ToSearchAttributes(),
		},
		&monitoralpb.HostWorkflowRequest{},
	)
	if err != nil {
		return fmt.Errorf("failed starting host workflow: %w", err)
	}

	// Send the configs
	err = run.HostUpdateConfig(h.runContext, &monitoralpb.HostUpdateConfigRequest{
		MutationSeq: seq,
		Configs:     configs,
	})
	if err != nil {
		return fmt.Errorf("failed updating host config on start: %w", err)
	}

	// To avoid a potential missed host config during startup, we wait a few
	// seconds and see if the config changed at which point we send that. The
	// sequence will be used to discard it if otherwise too old.
	select {
	case <-time.After(5 * time.Second):
	case <-h.runContext.Done():
		return nil
	}
	newSeq, newConfigs, err := h.getHostConfigs()
	if err != nil {
		return err
	}
	if newSeq > seq {
		err := h.host.updateConfig(h.runContext, &monitoralpb.HostUpdateConfigRequest{
			MutationSeq: newSeq,
			Configs:     newConfigs,
		})
		if err != nil {
			return fmt.Errorf("failed updating host config with config change shortly after start: %w", err)
		}
	}
	return nil
}

func (h *hostWorker) getHostConfigs() (seq uint64, configs []*monitoralpb.HostConfig, err error) {
	resp, err := h.host.client.SystemGetConfig(h.runContext, systemworker.SystemWorkflowID, "",
		&monitoralpb.SystemGetConfigRequest{})
	if err != nil {
		return 0, nil, fmt.Errorf("failed getting system config from system workflow: %w", err)
	}
	for _, hostConfig := range resp.GetConfig().GetHostConfigs() {
		for _, tag := range h.host.tags {
			if hostConfig.Tag == tag {
				configs = append(configs, hostConfig)
				break
			}
		}
	}
	return resp.MutationSeq, configs, nil
}

func (h *hostWorker) Wait() error {
	select {
	case <-h.runContext.Done():
		return nil
	case err := <-h.doneCh:
		return err
	}
}

func (h *hostWorker) Stop() {
	h.runCancel()
}
