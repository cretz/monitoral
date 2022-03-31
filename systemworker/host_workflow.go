package systemworker

import (
	"fmt"
	"sort"
	"time"

	"github.com/cretz/monitoral/monitoralpb"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/proto"
)

const ticksUntilContinueAsNew = 500

type hostWorkflow struct {
	*monitoralpb.HostWorkflowInput

	pendingCollectRequests []workflow.Future
	lastActivityRunRequest *monitoralpb.HostRunRequest

	hostActivity       *monitoralpb.HostRunActivityFuture
	hostActivityCancel workflow.CancelFunc

	newestConfigs           []*monitoralpb.HostConfig
	newestConfigMutationSeq uint64
}

func newHostWorkflow(ctx workflow.Context, in *monitoralpb.HostWorkflowInput) (monitoralpb.HostWorkflowImpl, error) {
	return &hostWorkflow{HostWorkflowInput: in}, nil
}

func (h *hostWorkflow) Run(ctx workflow.Context) error {
	// If there is a continue request, start it
	if h.Req.ContinueRunRequest != nil {
		if err := h.startActivity(ctx, h.Req.ContinueRunRequest); err != nil {
			return err
		}
	}

	// Continually run receiving signals/calls until continue as new or error
	ticksRemainingUntilContinueAsNew := ticksUntilContinueAsNew
	var wantingToContinueAsNew bool
	var currentActivityCancelled *monitoralpb.HostRunRequest
	for {
		// Run a tick. We'll set only-if-pending if we want to continue as new.
		tickResp := h.tick(ctx, wantingToContinueAsNew)

		// If the host is marked done, just return
		if tickResp.hostDone {
			return nil
		}

		// If there's an error, the workflow fails
		if tickResp.err != nil {
			return tickResp.err
		}

		// Keep track of the cancelled activity if we're wanting continue as new,
		// otherwise it's a failure that it cancelled without us asking it to
		if tickResp.activityCancelled != nil {
			if !wantingToContinueAsNew {
				return fmt.Errorf("activity unexpectedly stopped")
			}
			currentActivityCancelled = tickResp.activityCancelled
		}

		// If we didn't execute, that means nothing pending so we can go ahead and
		// return that continue as new
		if !tickResp.executed {
			return workflow.NewContinueAsNewError(ctx, monitoralpb.HostWorkflowName, &monitoralpb.HostWorkflowRequest{
				// This is so the activity can start where it left off
				ContinueRunRequest: currentActivityCancelled,
			})
		}

		// Tick down until continue as new
		if ticksRemainingUntilContinueAsNew > 0 {
			ticksRemainingUntilContinueAsNew--
			wantingToContinueAsNew = ticksRemainingUntilContinueAsNew == 0
		}
		// If we want to continue as new, but the activity cancel is around (which
		// means we haven't called it for this activity), we need to cancel the
		// activity
		if wantingToContinueAsNew && h.hostActivityCancel != nil {
			h.hostActivityCancel()
			h.hostActivityCancel = nil
		}
	}
}

type tickResponse struct {
	executed          bool
	activityCancelled *monitoralpb.HostRunRequest
	hostDone          bool
	err               error
}

func (h *hostWorkflow) tick(
	ctx workflow.Context,
	onlyIfPending bool,
) (resp tickResponse) {
	sel := workflow.NewSelector(ctx)
	// Add signal handlers
	h.HostRun.Select(sel, func(req *monitoralpb.HostRunRequest) {
		resp.err = h.startActivity(ctx, req)
	})
	h.HostUpdateConfig.Select(sel, func(req *monitoralpb.HostUpdateConfigRequest) {
		resp.err = h.updateConfig(ctx, req)
	})
	h.HostDone.Select(sel, func(*monitoralpb.HostDoneRequest) {
		resp.hostDone = true
	})
	h.HostCollect.Select(sel, func(req *monitoralpb.HostCollectRequest) {
		h.startCollect(ctx, req)
	})

	// If only when pending, return early if there are no pending signals or
	// futures
	if onlyIfPending && !sel.HasPending() && len(h.pendingCollectRequests) == 0 && h.hostActivity == nil {
		return
	}

	// Add all pending collect requests
	for _, pendingCollectRequest := range h.pendingCollectRequests {
		sel.AddFuture(pendingCollectRequest, func(f workflow.Future) {
			// Remove the completed future (we don't care about error which doesn't
			// get reported here anyways) via inline slice filter
			n := 0
			for _, pendingCollectRequest := range h.pendingCollectRequests {
				if pendingCollectRequest != f {
					h.pendingCollectRequests[n] = pendingCollectRequest
					n++
				}
			}
			h.pendingCollectRequests = h.pendingCollectRequests[:n]
		})
	}
	// Add the host activity if present
	if h.hostActivity != nil {
		h.hostActivity.Select(sel, func(f monitoralpb.HostRunActivityFuture) {
			resp.activityCancelled, resp.err = f.Get(ctx)
			if resp.err != nil {
				resp.err = fmt.Errorf("host activity failed: %w", resp.err)
			}
			h.hostActivity, h.hostActivityCancel = nil, nil
		})
	}

	// Perform the select
	sel.Select(ctx)
	resp.executed = true
	return
}

func (h *hostWorkflow) startActivity(ctx workflow.Context, req *monitoralpb.HostRunRequest) error {
	// Cancel and wait for cancellation if one is running
	if h.hostActivity != nil {
		// Technically this _could_ be nil if we're in a continue-as-new situation
		// so confirm it is not before cancelling
		if h.hostActivityCancel != nil {
			h.hostActivityCancel()
		}
		// Wait for cancel, which is reported as a success in our use case. But we
		// don't need the information to continue-as-new it so ignore response.
		// TODO(cretz): Any concerns about this waiting forever?
		_, err := h.hostActivity.Get(ctx)
		h.hostActivity, h.hostActivityCancel = nil, nil
		if err != nil {
			return fmt.Errorf("failed cancelling existing activity: %v", err)
		}
	}

	// Call config setter so it can reconcile if we already have newer
	h.setConfigs(req.ConfigMutationSeq, req.Configs)
	req.ConfigMutationSeq = h.newestConfigMutationSeq
	req.Configs = h.newestConfigs

	// Start the activity
	h.lastActivityRunRequest = req
	opts := &workflow.ActivityOptions{
		// Put this on the host task queue
		TaskQueue: req.TaskQueue,
		// We want this to run forever, so we'll give it 100 years. We also don't
		// care how long it takes to start.
		ScheduleToCloseTimeout: 100 * 365 * 24 * time.Hour,
		// But we do expect it to heartbeat frequently when it's running. We use a
		// downed activity to fail this workflow and thereby showing a host outage.
		// TODO(cretz): Make this configurable
		HeartbeatTimeout: 10 * time.Second,
		// We want to wait for the activity to be completed on cancellation since
		// we trigger a cancellation on continue as new and we want the data to
		// restart the activity.
		WaitForCancellation: true,
		// We are disabling retries. A failed activity means we fail the workflow. A
		// user can choose to restart the host worker thereby restarting the
		// workflow.
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	ctx, h.hostActivityCancel = workflow.WithCancel(ctx)
	fut := monitoralpb.HostRunActivity(ctx, opts, req)
	h.hostActivity = &fut
	return nil
}

func (h *hostWorkflow) updateConfig(ctx workflow.Context, req *monitoralpb.HostUpdateConfigRequest) error {
	// Set the config and only apply if different (implies newer)
	_, different := h.setConfigs(req.MutationSeq, req.Configs)
	if !different {
		workflow.GetLogger(ctx).Debug("Ignoring update config because it did not change")
	}

	// If there is no activity, nothing to update
	if h.hostActivity != nil {
		return nil
	}

	// The config is applied to the activity by calling an activity on the same
	// task queue. We want to wait for response inline here to make sure the
	// activity isn't restarted in the meantime.
	opts := &workflow.ActivityOptions{
		// Put this on the host task queue
		TaskQueue: h.lastActivityRunRequest.TaskQueue,
		// We expect this to be started and accepted reasonably quickly
		ScheduleToCloseTimeout: 10 * time.Second,
		// We do not want any retries. If it fails, we need to report that failure.
		// TODO(cretz): Is it ok to fail the host activities because of bad config?
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	if err := monitoralpb.HostUpdateConfigActivity(ctx, opts, req).Get(ctx); err != nil {
		return fmt.Errorf("failed updating activity config: %w", err)
	}
	return nil
}

// Only sets different if also newer
func (h *hostWorkflow) setConfigs(mutSeq uint64, configs []*monitoralpb.HostConfig) (newer, different bool) {
	// If not newer, don't set
	newer = mutSeq > h.newestConfigMutationSeq
	if !newer {
		return
	}
	// Sort the configs by tag
	sort.Slice(configs, func(i, j int) bool { return configs[i].Tag < configs[j].Tag })
	// Check if different
	different = len(configs) != len(h.newestConfigs)
	if !different {
		for i, config := range configs {
			if different = !proto.Equal(config, configs[i]); different {
				break
			}
		}
	}
	// Set values even if not different
	h.newestConfigMutationSeq = mutSeq
	h.newestConfigs = configs
	return
}

func (h *hostWorkflow) startCollect(ctx workflow.Context, req *monitoralpb.HostCollectRequest) {
	// We want to run this in the background
	fut, set := workflow.NewFuture(ctx)
	workflow.Go(ctx, func(ctx workflow.Context) {
		// Run the activity
		reqOpts := &workflow.ActivityOptions{
			// Put this on the host task queue
			TaskQueue: h.lastActivityRunRequest.TaskQueue,
			// We expect this to be started and accepted reasonably quickly
			ScheduleToCloseTimeout: 10 * time.Second,
			// We do not want any retries. If it fails, we need to report that failure.
			RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
		}
		resp, err := monitoralpb.HostCollectActivity(ctx, reqOpts, req).Get(ctx)
		if err != nil {
			resp = &monitoralpb.HostCollectResponse{Error: err.Error()}
		}

		// Respond to the call with the result
		respOpts := &workflow.ActivityOptions{
			// We expect this to be started and accepted reasonably quickly
			ScheduleToCloseTimeout: 10 * time.Second,
			// We do not want any retries.
			RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
		}
		if err := h.HostCollect.Respond(ctx, respOpts, req, resp).Get(ctx, nil); err != nil {
			// Sometimes the responder is no longer around, so we just log
			workflow.GetLogger(ctx).Debug("Failed sending collect response", "Error", err)
		}

		// Mark the response as complete
		set.SetValue(nil)
	})

	// Add the future as pending
	h.pendingCollectRequests = append(h.pendingCollectRequests, fut)
}
