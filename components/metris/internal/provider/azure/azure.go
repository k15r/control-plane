package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/tracing/opencensus"
	"github.com/kyma-project/control-plane/components/metris/internal/edp"
	"github.com/kyma-project/control-plane/components/metris/internal/log"
	"github.com/kyma-project/control-plane/components/metris/internal/provider"
	"github.com/kyma-project/control-plane/components/metris/internal/storage"
	"github.com/kyma-project/control-plane/components/metris/internal/tracing"
	"go.opencensus.io/trace"
	"k8s.io/client-go/util/workqueue"
)

var (
	// register the azure provider
	_ = func() struct{} {
		err := provider.RegisterProvider("az", NewAzureProvider)
		if err != nil {
			panic(err)
		}
		return struct{}{}
	}()
)

// NewAzureProvider returns a new Azure provider.
func NewAzureProvider(config *provider.Config) provider.Provider {
	// enable azure go-autorest tracing
	if tracing.IsEnabled() {
		if err := opencensus.Enable(); err != nil {
			config.Logger.With("error", err).Error("could not enable azure tracing")
		}
	}

	return &Azure{
		config:           config,
		instanceStorage:  storage.NewMemoryStorage("clusters"),
		vmCapsStorage:    storage.NewMemoryStorage("vm_capabilities"),
		queue:            workqueue.NewNamedDelayingQueue("clients"),
		ClientAuthConfig: &DefaultAuthConfig{},
	}
}

// Run starts azure metrics gathering for all clusters returned by gardener.
func (a *Azure) Run(ctx context.Context) {
	a.config.Logger.Info("provider started")

	// remove throttling request (429) from the status codes for which the client will retry
	// this will help with rate limit issues
	autorest.StatusCodesForRetry = []int{
		http.StatusRequestTimeout, // 408
		// http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
	}

	go a.clusterHandler(ctx)

	var wg sync.WaitGroup

	wg.Add(a.config.Workers)

	for i := 0; i < a.config.Workers; i++ {
		go func(i int) {
			defer wg.Done()

			for {
				// lock till an item is available from the queue.
				clusterid, quit := a.queue.Get()
				workerlogger := a.config.Logger.With("worker", i).With("technicalid", clusterid)

				if quit {
					workerlogger.Debug("worker stopped")
					return
				}

				obj, ok := a.instanceStorage.Get(clusterid.(string))
				if !ok {
					workerlogger.Warn("cluster not found in storage, must have been deleted")
					a.queue.Done(clusterid)

					continue
				}

				instance, ok := obj.(*Instance)
				if !ok {
					workerlogger.Error("cluster object is corrupted, removing it from storage")
					a.instanceStorage.Delete(clusterid.(string))
					a.queue.Done(clusterid)

					continue
				}

				workerlogger = workerlogger.With("account", instance.cluster.AccountID).With("subaccount", instance.cluster.SubAccountID)

				vmcaps := make(vmCapabilities)

				if obj, exists := a.vmCapsStorage.Get(instance.cluster.Region); exists {
					if caps, ok := obj.(*vmCapabilities); ok {
						vmcaps = *caps
					}
				} else {
					workerlogger.Warnf("vm capabilities for region %s not found, some metrics won't be available", instance.cluster.Region)
				}

				var (
					eventData *EventData
					err       error
				)

				// if last api call was rate limited, we skip this call to release some pressure on azure and return last events
				if instance.retryBackoff {
					instance.retryBackoff = false
					err = errors.New("client-side self-throttling, skip fetching metrics")
				} else {
					eventData, err = a.getMetrics(ctx, workerlogger, instance, &vmcaps)
				}

				if err != nil {
					if errdetail, ok := err.(autorest.DetailedError); ok {
						err = errdetail

						switch errdetail.StatusCode {
						// Check if the error is a resource group not found, then it would mean
						// that the cluster may have been deleted, and gardener did not trigger
						// the delete event or metris did not yet remove it from its cache.
						// Start retry attempt, then remove from storage if it reach max attempt.
						case http.StatusNotFound:
							if strings.Contains(errdetail.Original.Error(), responseErrCodeResourceGroupNotFound) {
								instance.retryAttempts++

								if instance.retryAttempts < maxRetryAttempts {
									a.instanceStorage.Put(instance.cluster.TechnicalID, instance)
									workerlogger.Warnf("can't find resource group in azure, attempts: %d/%d", instance.retryAttempts, maxRetryAttempts)
								} else {
									a.instanceStorage.Delete(instance.cluster.TechnicalID)
									workerlogger.Warnf("removing cluster after %d attempts", maxRetryAttempts)
								}
							}

						case http.StatusTooManyRequests:
							// request is being throttled, skip next call to release pressure on API
							instance.retryBackoff = true

							workerlogger.Debug("=============> THROTTLING - setting retryBackoff")
						}
					}

					if instance.lastEvent == nil {
						workerlogger.With("error", err).Error("could not get metrics, dropping events because no cached information")
					} else {
						workerlogger.With("error", err).Error("could not get metrics, using information from cache")

						eventData = instance.lastEvent
					}
				}

				if eventData != nil {
					if err := a.sendMetrics(workerlogger, instance, eventData); err != nil {
						workerlogger.With("error", err).Error("error parsing metric information, could not send event to EDP")
					}
				}

				// save changes to storage
				a.instanceStorage.Put(instance.cluster.TechnicalID, instance)

				a.queue.Done(clusterid)

				// requeue item after X duration if client still in storage
				if !a.queue.ShuttingDown() {
					workerlogger.Debugf("requeuing cluster in %s", a.config.PollInterval)
					a.queue.AddAfter(clusterid, a.config.PollInterval)
				} else {
					workerlogger.Debug("queue is shutting down, can't requeue cluster")
				}
			}
		}(i)
	}

	wg.Wait()
	a.config.Logger.Info("provider stopped")
}

// clusterHandler listen on the cluster channel then update the storage and the queue.
func (a *Azure) clusterHandler(parentctx context.Context) {
	a.config.Logger.Debug("starting cluster handler")

	for {
		select {
		case cluster := <-a.config.ClusterChannel:
			logger := a.config.Logger.
				With("technicalid", cluster.TechnicalID).
				With("accountid", cluster.AccountID).
				With("subaccountid", cluster.SubAccountID)

			logger.Debug("received cluster from gardener controller")

			// if cluster was flag as deleted, remove it from storage and exit.
			if cluster.Deleted {
				logger.Info("removing cluster from storage")

				a.instanceStorage.Delete(cluster.TechnicalID)

				continue
			}

			instance := &Instance{cluster: cluster}

			// recover instance from storage.
			if obj, exists := a.instanceStorage.Get(cluster.TechnicalID); exists {
				if i, ok := obj.(*Instance); ok {
					instance.lastEvent = i.lastEvent
					instance.eventHubResourceGroupName = i.eventHubResourceGroupName
				}
			}

			// creating Azure REST API base client
			if client, err := newClient(cluster, logger, a.ClientAuthConfig); err != nil {
				logger.With("error", err).Error("error while creating client configuration, cluster will be ignored")
				a.instanceStorage.Delete(cluster.TechnicalID)

				continue
			} else {
				instance.client = client
			}

			if instance.eventHubResourceGroupName == "" {
				// Resource Groups for Event Hubs are tag with the subaccountid, if none is found, it may be a trial account.
				filter := fmt.Sprintf("tagname eq '%s' and tagvalue eq '%s'", tagNameSubAccountID, cluster.SubAccountID)

				if rg, err := instance.client.GetResourceGroup(parentctx, "", filter, logger); err != nil {
					if !cluster.Trial {
						logger.Warnf("could not find a resource group for event hub, cluster may not be ready, retrying in %s: %s", a.config.PollInterval, err)
						time.AfterFunc(a.config.PollInterval, func() { a.config.ClusterChannel <- cluster })

						continue
					}
				} else {
					instance.eventHubResourceGroupName = *rg.Name
				}
			}

			a.instanceStorage.Put(cluster.TechnicalID, instance)

			// initialize vm capabilities cache for the cluster region if not already.
			if _, exists := a.vmCapsStorage.Get(cluster.Region); !exists {
				logger.Debugf("initializing vm capabilities cache for region %s", instance.cluster.Region)
				filter := fmt.Sprintf("location eq '%s'", cluster.Region)

				var vmcaps = make(vmCapabilities) // [vmtype][capname]capvalue

				if skuList, err := instance.client.GetVMResourceSkus(parentctx, filter, logger); err != nil {
					logger.Errorf("error while getting vm capabilities for region %s: %s", cluster.Region, err)
				} else {
					for _, item := range skuList {
						vmcaps[*item.Name] = make(map[string]string)
						for _, v := range *item.Capabilities {
							vmcaps[*item.Name][*v.Name] = *v.Value
						}
					}
				}

				if len(vmcaps) > 0 {
					a.vmCapsStorage.Put(instance.cluster.Region, &vmcaps)
				}
			}

			a.queue.Add(cluster.TechnicalID)
		case <-parentctx.Done():
			a.config.Logger.Debug("stopping cluster handler")
			a.queue.ShutDown()

			return
		}
	}
}

// getMetrics - collect results from different Azure API and create edp events.
func (a *Azure) getMetrics(parentctx context.Context, workerlogger log.Logger, instance *Instance, vmcaps *vmCapabilities) (*EventData, error) {
	if tracing.IsEnabled() {
		var span *trace.Span

		parentctx, span = trace.StartSpan(parentctx, "metris/provider/azure/getMetrics")
		defer span.End()

		workerlogger = workerlogger.With("traceID", span.SpanContext().TraceID).With("spanID", span.SpanContext().SpanID)
	}

	workerlogger.Debug("getting metrics")

	// Using a timeout context to prevent azure api to hang for too long,
	// sometimes client get stuck waiting even with a max poll duration is set.
	// If it reach the time limit, last successful event data will be returned.
	ctx, cancel := context.WithTimeout(parentctx, a.config.PollingDuration)
	defer cancel()

	computeData, err := instance.getComputeMetrics(ctx, workerlogger, vmcaps)
	if err != nil {
		return nil, err
	}

	networkData, err := instance.getNetworkMetrics(ctx, workerlogger)
	if err != nil {
		return nil, err
	}

	eventData := &EventData{
		ResourceGroups: []string{instance.cluster.TechnicalID},
		Compute:        computeData,
		Networking:     networkData,
		// init an empty eventhub data, because they are optional (trial account)
		EventHub: &EventHub{
			NumberNamespaces:     0,
			IncomingRequestsPT1M: 0,
			MaxIncomingBytesPT1M: 0,
			MaxOutgoingBytesPT1M: 0,
			IncomingRequestsPT5M: 0,
			MaxIncomingBytesPT5M: 0,
			MaxOutgoingBytesPT5M: 0,
		},
	}

	if len(instance.eventHubResourceGroupName) > 0 {
		eventhubData, err := instance.getEventHubMetrics(ctx, a.config.PollInterval, workerlogger)
		if err != nil {
			return nil, err
		}

		eventData.ResourceGroups = append(eventData.ResourceGroups, instance.eventHubResourceGroupName)
		eventData.EventHub = eventhubData
	}

	return eventData, nil
}

// sendMetrics - send events to EDP.
func (a *Azure) sendMetrics(workerlogger log.Logger, instance *Instance, eventData *EventData) error {
	eventDataRaw, err := json.Marshal(&eventData)
	if err != nil {
		return err
	}

	// save a copy of the event data in case of error next time
	instance.lastEvent = eventData

	eventDataJSON := json.RawMessage(eventDataRaw)

	eventBuffer := edp.Event{
		Datatenant: instance.cluster.SubAccountID,
		Data:       &eventDataJSON,
	}

	workerlogger.Debug("sending event to EDP")

	a.config.EventsChannel <- &eventBuffer

	return nil
}
