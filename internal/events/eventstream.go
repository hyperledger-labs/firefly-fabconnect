// Copyright 2021 Kaleido
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package events

import (
	"container/list"
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hyperledger-labs/firefly-fabconnect/internal/auth"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/errors"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/events/api"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/ws"

	log "github.com/sirupsen/logrus"
)

type DistributionMode string

const (
	DistributionModeBroadcast DistributionMode = "broadcast"
	DistributionModeWLD       DistributionMode = "workloadDistribution"
)

const (
	// FromBlockNewest is the special string that means subscribe from the current block
	FromBlockNewest = "newest"
	// ErrorHandlingBlock blocks the event stream until the handler can accept the event
	ErrorHandlingBlock = "block"
	// ErrorHandlingSkip processes up to the retry behavior on the stream, then skips to the next event
	ErrorHandlingSkip = "skip"
	// MaxBatchSize is the maximum that a user can specific for their batch size
	MaxBatchSize = 1000
	// DefaultExponentialBackoffInitial  is the initial delay for backoff retry
	DefaultExponentialBackoffInitial = time.Duration(1) * time.Second
	// DefaultExponentialBackoffFactor is the factor we use between retries
	DefaultExponentialBackoffFactor = float64(2.0)
	// DefaultTimestampCacheSize is the number of entries we will hold in a LRU cache for block timestamps
	DefaultTimestampCacheSize = 1000
)

// StreamInfo configures the stream to perform an action for each event
type StreamInfo struct {
	TimeSorted
	ID                   string               `json:"id"`
	Name                 string               `json:"name,omitempty"`
	Path                 string               `json:"path"`
	Suspended            bool                 `json:"suspended"`
	Type                 string               `json:"type,omitempty"`
	BatchSize            uint64               `json:"batchSize,omitempty"`
	BatchTimeoutMS       uint64               `json:"batchTimeoutMS,omitempty"`
	ErrorHandling        string               `json:"errorHandling,omitempty"`
	RetryTimeoutSec      uint64               `json:"retryTimeoutSec,omitempty"`
	BlockedRetryDelaySec uint64               `json:"blockedReryDelaySec,omitempty"`
	Webhook              *webhookActionInfo   `json:"webhook,omitempty"`
	WebSocket            *webSocketActionInfo `json:"websocket,omitempty"`
	Timestamps           bool                 `json:"timestamps,omitempty"` // Include block timestamps in the events generated
	TimestampCacheSize   int                  `json:"timestampCacheSize,omitempty"`
}

type webhookActionInfo struct {
	URL               string            `json:"url,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	TLSkipHostVerify  bool              `json:"tlsSkipHostVerify,omitempty"`
	RequestTimeoutSec uint32            `json:"requestTimeoutSec,omitempty"`
}

type webSocketActionInfo struct {
	Topic            string           `json:"topic,omitempty"`
	DistributionMode DistributionMode `json:"distributionMode,omitempty"`
}

type eventStream struct {
	sm                subscriptionManager
	allowPrivateIPs   bool
	spec              *StreamInfo
	eventStream       chan *eventData
	stopped           bool
	processorDone     bool
	pollingInterval   time.Duration
	pollerDone        bool
	inFlight          uint64
	batchCond         *sync.Cond
	batchQueue        *list.List
	batchCount        uint64
	initialRetryDelay time.Duration
	backoffFactor     float64
	updateInProgress  bool
	updateInterrupt   chan struct{}   // a zero-sized struct used only for signaling (hand rolled alternative to context)
	updateWG          *sync.WaitGroup // Wait group for the go routines to reply back after they have stopped
	action            eventStreamAction
	wsChannels        ws.WebSocketChannels
}

type eventStreamAction interface {
	attemptBatch(batchNumber, attempt uint64, events []*api.EventEntry) error
}

func validateWebSocket(w *webSocketActionInfo) error {
	if w.DistributionMode != "" && w.DistributionMode != DistributionModeBroadcast && w.DistributionMode != DistributionModeWLD {
		return errors.Errorf(errors.EventStreamsInvalidDistributionMode, w.DistributionMode)
	}
	return nil
}

// newEventStream constructor verifies the action is correct, kicks
// off the event batch processor, and blockHWM will be
// initialied to that supplied (zero on initial, or the
// value from the checkpoint)
func newEventStream(sm subscriptionManager, spec *StreamInfo, wsChannels ws.WebSocketChannels) (a *eventStream, err error) {
	if spec == nil || spec.GetID() == "" {
		return nil, errors.Errorf(errors.EventStreamsNoID)
	}

	if spec.BatchSize == 0 {
		spec.BatchSize = 1
	} else if spec.BatchSize > MaxBatchSize {
		spec.BatchSize = MaxBatchSize
	}
	if spec.BatchTimeoutMS == 0 {
		spec.BatchTimeoutMS = 5000
	}
	if spec.BlockedRetryDelaySec == 0 {
		spec.BlockedRetryDelaySec = 30
	}
	if strings.ToLower(spec.ErrorHandling) == ErrorHandlingBlock {
		spec.ErrorHandling = ErrorHandlingBlock
	} else {
		spec.ErrorHandling = ErrorHandlingSkip
	}
	if spec.TimestampCacheSize == 0 {
		spec.TimestampCacheSize = DefaultTimestampCacheSize
	}

	a = &eventStream{
		sm:                sm,
		spec:              spec,
		allowPrivateIPs:   sm.getConfig().WebhooksAllowPrivateIPs,
		eventStream:       make(chan *eventData),
		batchCond:         sync.NewCond(&sync.Mutex{}),
		batchQueue:        list.New(),
		initialRetryDelay: DefaultExponentialBackoffInitial,
		backoffFactor:     DefaultExponentialBackoffFactor,
		pollingInterval:   time.Duration(sm.getConfig().PollingIntervalSec) * time.Second,
		wsChannels:        wsChannels,
	}

	if a.pollingInterval == 0 {
		// Let's us do this from UTs, without exposing it
		a.pollingInterval = 10 * time.Millisecond
	}

	spec.Type = strings.ToLower(spec.Type)
	switch spec.Type {
	case "webhook":
		if a.action, err = newWebhookAction(a, spec.Webhook); err != nil {
			return nil, err
		}
	case "websocket":

		if spec.WebSocket != nil {
			if err := validateWebSocket(spec.WebSocket); err != nil {
				return nil, err
			}
		}

		if a.action, err = newWebSocketAction(a, spec.WebSocket); err != nil {
			return nil, err
		}
	default:
		return nil, errors.Errorf(errors.EventStreamsInvalidActionType, spec.Type)
	}

	a.startEventHandlers(false)
	return a, nil
}

// helper to kick off go routines and any tracking entities
func (a *eventStream) startEventHandlers(resume bool) {
	// create a context that can be used to indicate an update to the eventstream
	a.updateInterrupt = make(chan struct{})
	a.updateWG = &sync.WaitGroup{}
	a.updateWG.Add(1) // add a channel for eventPoller to inform after it has stopped
	go a.eventPoller()
	a.updateWG.Add(1) // add a channel for batchProcessor to inform after it has stopped
	go a.batchProcessor()
	// For a pause/resume, the batch dispatcher goroutine is not terminated, hence no need to start it
	if !resume {
		a.updateWG.Add(1) // add a channel for batchDispatcher to inform after it has stopped
		go a.batchDispatcher()
	}
}

// GetID returns the ID (for sorting)
func (spec *StreamInfo) GetID() string {
	return spec.ID
}

// preUpdateStream sets a flag to indicate updateInProgress and wakes up goroutines waiting on condition variable
func (a *eventStream) preUpdateStream() {
	a.batchCond.L.Lock()
	a.updateInProgress = true
	a.batchCond.Broadcast()
	a.batchCond.L.Unlock()
}

// postUpdateStream resets flags and kicks off a fresh round of handler go routines
func (a *eventStream) postUpdateStream() {
	a.batchCond.L.Lock()
	a.pollerDone = false
	a.processorDone = false
	a.updateInProgress = false
	a.batchCond.L.Unlock()
	a.startEventHandlers(false)
}

// update modifies an existing eventStream
func (a *eventStream) update(newSpec *StreamInfo) (spec *StreamInfo, err error) {
	log.Infof("%s: Update event stream", a.spec.ID)
	// set a flag to indicate updateInProgress
	// For any go routines that are Wait() ing on the eventListener, wake them up
	a.preUpdateStream()
	// close the updateInterrupt channel so that the event handler go routines can be woken up
	close(a.updateInterrupt)
	// wait for the poked goroutines to finish up
	a.updateWG.Wait()

	if newSpec.Type != "" && newSpec.Type != a.spec.Type {
		return nil, errors.Errorf(errors.EventStreamsCannotUpdateType)
	}
	if a.spec.Type == "webhook" && newSpec.Webhook != nil {
		if newSpec.Webhook.URL == "" {
			return nil, errors.Errorf(errors.EventStreamsWebhookNoURL)
		}
		if _, err = url.Parse(newSpec.Webhook.URL); err != nil {
			return nil, errors.Errorf(errors.EventStreamsWebhookInvalidURL)
		}
		if newSpec.Webhook.RequestTimeoutSec == 0 {
			newSpec.Webhook.RequestTimeoutSec = 120
		}
		a.spec.Webhook.URL = newSpec.Webhook.URL
		a.spec.Webhook.RequestTimeoutSec = newSpec.Webhook.RequestTimeoutSec
		a.spec.Webhook.TLSkipHostVerify = newSpec.Webhook.TLSkipHostVerify
		a.spec.Webhook.Headers = newSpec.Webhook.Headers
	}
	if a.spec.Type == "websocket" && newSpec.WebSocket != nil {
		a.spec.WebSocket.Topic = newSpec.WebSocket.Topic
		if err := validateWebSocket(newSpec.WebSocket); err != nil {
			return nil, err
		}
		a.spec.WebSocket.DistributionMode = newSpec.WebSocket.DistributionMode
	}

	if a.spec.BatchSize != newSpec.BatchSize && newSpec.BatchSize != 0 && newSpec.BatchSize < MaxBatchSize {
		a.spec.BatchSize = newSpec.BatchSize
	}
	if a.spec.BatchTimeoutMS != newSpec.BatchTimeoutMS && newSpec.BatchTimeoutMS != 0 {
		a.spec.BatchTimeoutMS = newSpec.BatchTimeoutMS
	}
	if a.spec.BlockedRetryDelaySec != newSpec.BlockedRetryDelaySec && newSpec.BlockedRetryDelaySec != 0 {
		a.spec.BlockedRetryDelaySec = newSpec.BlockedRetryDelaySec
	}
	if strings.ToLower(newSpec.ErrorHandling) == ErrorHandlingBlock {
		a.spec.ErrorHandling = ErrorHandlingBlock
	} else {
		a.spec.ErrorHandling = ErrorHandlingSkip
	}
	if newSpec.Name != "" && a.spec.Name != newSpec.Name {
		a.spec.Name = newSpec.Name
	}
	if a.spec.Timestamps != newSpec.Timestamps {
		a.spec.Timestamps = newSpec.Timestamps
	}
	a.postUpdateStream()
	return a.spec, nil
}

// HandleEvent is the entry point for the stream from the event detection logic
func (a *eventStream) handleEvent(event *eventData) {
	// Does nothing more than add it to the batch, to be picked up
	// by the batchDispatcher
	a.eventStream <- event
}

// stop is a lazy stop, that marks a flag for the batch goroutine to pick up
func (a *eventStream) stop() {
	a.batchCond.L.Lock()
	a.stopped = true
	close(a.eventStream)
	a.batchCond.Broadcast()
	a.batchCond.L.Unlock()
}

// suspend only stops the dispatcher, pushing back as if we're in blocking mode
func (a *eventStream) suspend() {
	a.batchCond.L.Lock()
	a.spec.Suspended = true
	a.batchCond.Broadcast()
	a.batchCond.L.Unlock()
}

// resume resumes the dispatcher
func (a *eventStream) resume() error {
	a.batchCond.L.Lock()
	defer a.batchCond.L.Unlock()
	if !a.processorDone || !a.pollerDone {
		return errors.Errorf(errors.EventStreamsWebhookResumeActive, a.spec.Suspended)
	}
	a.spec.Suspended = false
	a.processorDone = false
	a.pollerDone = false

	a.startEventHandlers(true)
	a.batchCond.Broadcast()
	return nil
}

// isBlocked protect us from polling for more events when the stream is blocked.
// Can happen regardless of whether the error handling is
// block or skip. It's just with skip we eventually move onto new messages
// after the retries etc. are complete
func (a *eventStream) isBlocked() bool {
	a.batchCond.L.Lock()
	inFlight := a.inFlight
	v := inFlight >= a.spec.BatchSize
	a.batchCond.L.Unlock()
	if v {
		log.Warnf("%s: Is currently blocked. InFlight=%d BatchSize=%d", a.spec.ID, inFlight, a.spec.BatchSize)
	}
	return v
}

func (a *eventStream) markAllSubscriptionsStale(ctx context.Context) {
	// Mark all subscriptions stale, so they will re-start from the checkpoint if/when we re-run the poller
	subs := a.sm.subscriptionsForStream(a.spec.ID)
	for _, sub := range subs {
		sub.markFilterStale(true)
	}
}

// eventPoller checks every few seconds against the Fabric node for any
// new events on the subscriptions that are registered for this stream
func (a *eventStream) eventPoller() {
	defer a.updateWG.Done()

	ctx := auth.NewSystemAuthContext()

	defer func() { a.pollerDone = true }()
	var checkpoint map[string]uint64
	for !a.suspendOrStop() {
		var err error
		// Load the checkpoint (should only be first time round)
		if checkpoint == nil {
			if checkpoint, err = a.sm.loadCheckpoint(a.spec.ID); err != nil {
				log.Errorf("%s: Failed to load checkpoint: %s", a.spec.ID, err)
			}
		}
		// If we're not blocked, then grab some more events
		subs := a.sm.subscriptionsForStream(a.spec.ID)
		if err == nil && !a.isBlocked() {
			for _, sub := range subs {
				// We do the reset on the event processing thread, to avoid any concurrency issue.
				// It's just an unsubscribe, which clears the resetRequested flag and sets us stale.
				if sub.resetRequested {
					sub.unsubscribe(false)
					// Clear any checkpoint
					delete(checkpoint, sub.info.ID)
				}
				if sub.filterStale && !sub.deleting {
					blockHeight, exists := checkpoint[sub.info.ID]
					if !exists || blockHeight <= 0 {
						blockHeight, err = sub.setInitialBlockHeight(ctx)
					} else {
						sub.setCheckpointBlockHeight(blockHeight)
					}
					if err == nil {
						err = sub.restartFilter(ctx, blockHeight)
					}
				}
				if err != nil {
					log.Errorf("%s: subscription error: %s", a.spec.ID, err)
					err = nil
				}
			}
		}
		// Record a new checkpoint if needed
		if checkpoint != nil {
			changed := false
			for _, sub := range subs {
				i1 := checkpoint[sub.info.ID]
				i2 := sub.blockHWM()

				changed = changed || i1 == 0 || i1 != i2
				checkpoint[sub.info.ID] = i2
			}
			if changed {
				if err = a.sm.storeCheckpoint(a.spec.ID, checkpoint); err != nil {
					log.Errorf("%s: Failed to store checkpoint: %s", a.spec.ID, err)
				}
			}
		}
		// the event poller reacts to notification about a stream update, else it starts
		// another round of polling after completion of the pollingInterval
		select {
		case <-a.updateInterrupt:
			// we were notified by the caller about an ongoing update, no need to continue
			log.Infof("%s: Notified of an ongoing stream update, exiting event poller", a.spec.ID)
			a.markAllSubscriptionsStale(ctx)
			return
		case <-time.After(a.pollingInterval): //fall through and continue to the next iteration
		}
	}

	a.markAllSubscriptionsStale(ctx)

}

// batchDispatcher is the goroutine that is always available to read new
// events and form them into batches. Because we can't be sure how many
// events we'll be dispatched from blocks before the IsBlocked() feedback
// loop protects us, this logic has to build a list of batches
func (a *eventStream) batchDispatcher() {
	var currentBatch []*eventData
	var batchStart time.Time
	batchTimeout := time.Duration(a.spec.BatchTimeoutMS) * time.Millisecond
	defer a.updateWG.Done()
	for {
		// Wait for the next event - if we're in the middle of a batch, we
		// need to cope with a timeout
		log.Debugf("%s: Begin batch dispatcher loop, current batch length: %d", a.spec.ID, len(currentBatch))
		timeout := false
		if len(currentBatch) > 0 {
			// Existing batch
			timeLeft := time.Until(batchStart.Add(batchTimeout))
			ctx, cancel := context.WithTimeout(context.Background(), timeLeft)
			select {
			case <-ctx.Done():
				cancel()
				timeout = true
			case event := <-a.eventStream:
				cancel()
				if event == nil {
					log.Infof("%s: Event stream stopped while waiting for in-flight batch to fill", a.spec.ID)
					return
				}
				currentBatch = append(currentBatch, event)
			case <-a.updateInterrupt:
				// we were notified by the caller about an ongoing update, cancel the timeout ctx and return
				log.Infof("%s: Notified of an ongoing stream update, will not dispatch batch", a.spec.ID)
				cancel() // cancel the ctx which was started to track timeout
				return
			}
		} else {
			// New batch - react to an update notification or process the next set of events from the stream
			select {
			case <-a.updateInterrupt:
				// we were notified by the caller about an ongoing update, return
				log.Infof("%s: Notified of an ongoing stream update, not waiting for new events", a.spec.ID)
				return
			case event := <-a.eventStream:
				if event == nil {
					log.Infof("%s: Event stream stopped", a.spec.ID)
					return
				}
				currentBatch = []*eventData{event}
				log.Infof("%s: New batch length %d", a.spec.ID, len(currentBatch))
				batchStart = time.Now()
			}
		}
		if timeout || uint64(len(currentBatch)) == a.spec.BatchSize {
			// We are ready to dispatch the batch
			a.batchCond.L.Lock()
			if !timeout {
				a.inFlight++
			}
			a.batchQueue.PushBack(currentBatch)
			a.batchCond.Broadcast()
			a.batchCond.L.Unlock()
			currentBatch = []*eventData{}
		} else {
			// Just increment in-flight count (batch processor decrements)
			a.batchCond.L.Lock()
			a.inFlight++
			a.batchCond.L.Unlock()
		}
	}
}

func (a *eventStream) suspendOrStop() bool {
	return a.spec.Suspended || a.stopped
}

// batchProcessor picks up batches from the batchDispatcher, and performs the blocking
// actions required to perform the action itself.
// We use a sync.Cond rather than a channel to communicate with this goroutine, as
// it might be blocked for very large periods of time
func (a *eventStream) batchProcessor() {
	defer func() { a.processorDone = true }()
	for {
		// Wait for the next batch, or to be stopped
		a.batchCond.L.Lock()
		for !a.suspendOrStop() && a.batchQueue.Len() == 0 {
			if a.updateInProgress {
				<-a.updateInterrupt
				// we were notified by the caller about an ongoing update, return
				log.Infof("%s: Notified of an ongoing stream update, exiting batch processor", a.spec.ID)
				a.updateWG.Done() //Not moving this to a 'defer' since we need to unlock after calling Done()
				a.batchCond.L.Unlock()
				return
			} else {
				a.batchCond.Wait()
			}
		}
		if a.suspendOrStop() {
			log.Infof("%s: Suspended, returning exiting batch processor", a.spec.ID)
			a.batchCond.L.Unlock()
			return
		}
		batchElem := a.batchQueue.Front()
		a.batchCount++
		batchNumber := a.batchCount
		a.batchQueue.Remove(batchElem)
		a.batchCond.L.Unlock()
		// Process the batch - could block for a very long time, particularly if
		// ErrorHandlingBlock is configured.
		// Track this as an item in the update wait group
		a.updateWG.Add(1)
		a.processBatch(batchNumber, batchElem.Value.([]*eventData))
	}
}

// processBatch is the blocking function to process a batch of events
// It never returns an error, and uses the chosen block/skip ErrorHandling
// behaviour combined with the parameters on the event itself
func (a *eventStream) processBatch(batchNumber uint64, events []*eventData) {
	defer a.updateWG.Done()
	if len(events) == 0 {
		return
	}
	processed := false
	attempt := 0
	for !a.suspendOrStop() && !processed {
		if attempt > 0 {
			select {
			case <-a.updateInterrupt:
				// we were notified by the caller about an ongoing update, no need to continue
				log.Infof("%s: Notified of an ongoing stream update, terminating process batch", a.spec.ID)
				return
			case <-time.After(time.Duration(a.spec.BlockedRetryDelaySec) * time.Second): //fall through and continue
			}
		}
		attempt++
		log.Infof("%s: Batch %d initiated with %d events. FirstBlock=%d LastBlock=%d", a.spec.ID, batchNumber, len(events), events[0].event.BlockNumber, events[len(events)-1].event.BlockNumber)
		a.updateWG.Add(1)
		eventEntries := make([]*api.EventEntry, len(events))
		for i, entry := range events {
			eventEntries[i] = entry.event
		}
		err := a.performActionWithRetry(batchNumber, eventEntries)
		// If we got an error after all of the internal retries within the event
		// handler failed, then the ErrorHandling strategy kicks in
		processed = (err == nil)
		if !processed {
			log.Errorf("%s: Batch %d attempt %d failed. ErrorHandling=%s BlockedRetryDelay=%ds",
				a.spec.ID, batchNumber, attempt, a.spec.ErrorHandling, a.spec.BlockedRetryDelaySec)
			processed = (a.spec.ErrorHandling == ErrorHandlingSkip)
		}
	}

	// decrement the in-flight count if we've processed (wouldn't have occurred if we were suspended or stopped)
	a.batchCond.L.Lock()
	if processed {
		a.inFlight -= uint64(len(events))
	}
	a.batchCond.L.Unlock()

	// If we were suspended, do not ack the batch
	if a.suspendOrStop() {
		return
	}

	// Call all the callbacks on the events, so they can update their high water marks
	// If there are multiple events from one SubID, we call it only once with the
	// last message in the batch
	cbs := make(map[string]*eventData)
	for _, event := range events {
		cbs[event.event.SubID] = event
	}
	for _, event := range cbs {
		event.batchComplete(event.event)
	}
}

// performActionWithRetry performs an action, with exponential backoff retry up
// to a given threshold
func (a *eventStream) performActionWithRetry(batchNumber uint64, events []*api.EventEntry) (err error) {
	startTime := time.Now()
	endTime := startTime.Add(time.Duration(a.spec.RetryTimeoutSec) * time.Second)
	delay := a.initialRetryDelay
	var attempt uint64
	complete := false
	defer a.updateWG.Done()

	for !a.suspendOrStop() && !complete {
		if attempt > 0 {
			log.Infof("%s: Waiting %.2fs before re-attempting batch %d", a.spec.ID, delay.Seconds(), batchNumber)
			select {
			case <-a.updateInterrupt:
				// we were notified by the caller about an ongoing update, no need to continue
				log.Infof("%s: Notified of an ongoing stream update, terminating perform action for batch number: %d", a.spec.ID, batchNumber)
				return
			case <-time.After(delay): //fall through and continue
			}
			delay = time.Duration(float64(delay) * a.backoffFactor)
		}
		attempt++
		err = a.action.attemptBatch(batchNumber, attempt, events)
		complete = err == nil || time.Until(endTime) < 0
	}
	return err
}

// isAddressSafe checks for local IPs
func (a *eventStream) isAddressUnsafe(ip *net.IPAddr) bool {
	ip4 := ip.IP.To4()
	return !a.allowPrivateIPs &&
		(ip4[0] == 0 ||
			ip4[0] >= 224 ||
			ip4[0] == 127 ||
			ip4[0] == 10 ||
			(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] < 32) ||
			(ip4[0] == 192 && ip4[1] == 168))
}
