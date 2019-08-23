package replicator

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/uber/cadence/.gen/go/cadence/workflowserviceclient"
	"github.com/uber/cadence/.gen/go/replicator"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/backoff"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/metrics"
)

const (
	fetchTaskRequestTimeout                   = 60 * time.Second
	pollTimerJitterCoefficient                = 0.2
	pollIntervalSecs                          = 5
	taskProcessorErrorRetryWait               = time.Second
	taskProcessorErrorRetryBackoffCoefficient = 1
	taskProcessorErrorRetryMaxAttampts        = 5
)

func newDomainReplicationMessageProcessor(
	sourceCluster string,
	logger log.Logger,
	remotePeer workflowserviceclient.Interface,
	metricsClient metrics.Client,
	domainReplicator DomainReplicator,
) *domainReplicationMessageProcessor {
	retryPolicy := backoff.NewExponentialRetryPolicy(taskProcessorErrorRetryWait)
	retryPolicy.SetBackoffCoefficient(taskProcessorErrorRetryBackoffCoefficient)
	retryPolicy.SetMaximumAttempts(taskProcessorErrorRetryMaxAttampts)

	return &domainReplicationMessageProcessor{
		status:                 common.DaemonStatusInitialized,
		sourceCluster:          sourceCluster,
		logger:                 logger,
		remotePeer:             remotePeer,
		domainReplicator:       domainReplicator,
		metricsClient:          metricsClient,
		retryPolicy:            retryPolicy,
		lastProcessedMessageID: -1,
		lastRetrievedMessageID: -1,
		done:                   make(chan struct{}),
	}
}

type (
	domainReplicationMessageProcessor struct {
		status                 int32
		sourceCluster          string
		logger                 log.Logger
		remotePeer             workflowserviceclient.Interface
		domainReplicator       DomainReplicator
		metricsClient          metrics.Client
		retryPolicy            backoff.RetryPolicy
		lastProcessedMessageID int64
		lastRetrievedMessageID int64
		done                   chan struct{}
	}
)

func (p *domainReplicationMessageProcessor) Start() {
	if !atomic.CompareAndSwapInt32(&p.status, common.DaemonStatusInitialized, common.DaemonStatusStarted) {
		return
	}

	go p.processorLoop()
}

// TODO: need to make sure only one worker is processing per source DC
// TODO: store checkpoints in DB
func (p *domainReplicationMessageProcessor) processorLoop() {
	timer := time.NewTimer(getWaitDuration())

	for {
		select {
		case <-timer.C:
			ctx, cancel := context.WithTimeout(context.Background(), fetchTaskRequestTimeout)
			request := &replicator.GetDomainReplicationMessagesRequest{
				LastRetrivedMessageId:  common.Int64Ptr(p.lastRetrievedMessageID),
				LastProcessedMessageId: common.Int64Ptr(p.lastProcessedMessageID),
			}
			response, err := p.remotePeer.GetDomainReplicationMessages(ctx, request)
			cancel()

			if err != nil {
				p.logger.Error("Failed to get replication tasks", tag.Error(err))
				timer.Reset(getWaitDuration())
				continue
			}

			p.logger.Debug("Successfully fetched domain replication tasks.", tag.Counter(len(response.Messages.ReplicationTasks)))

			for _, task := range response.Messages.ReplicationTasks {
				err := backoff.Retry(func() error {
					return p.handleDomainReplicationTask(task)
				}, p.retryPolicy, isTransientRetryableError)

				if err != nil {
					p.metricsClient.IncCounter(metrics.DomainReplicationTaskScope, metrics.ReplicatorFailures)
					// TODO: put task into DLQ
				}
			}

			p.lastProcessedMessageID = response.Messages.GetLastRetrivedMessageId()
			p.lastRetrievedMessageID = response.Messages.GetLastRetrivedMessageId()
			timer.Reset(getWaitDuration())
		case <-p.done:
			timer.Stop()
			return
		}
	}
}

func (p *domainReplicationMessageProcessor) handleDomainReplicationTask(task *replicator.ReplicationTask) error {
	p.metricsClient.IncCounter(metrics.DomainReplicationTaskScope, metrics.ReplicatorMessages)
	sw := p.metricsClient.StartTimer(metrics.DomainReplicationTaskScope, metrics.ReplicatorLatency)
	defer sw.Stop()

	return p.domainReplicator.HandleReceivingTask(task.DomainTaskAttributes)
}

func (p *domainReplicationMessageProcessor) Stop() {
	close(p.done)
}

func getWaitDuration() time.Duration {
	return backoff.JitDuration(time.Duration(pollIntervalSecs)*time.Second, pollTimerJitterCoefficient)
}