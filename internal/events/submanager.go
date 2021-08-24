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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hyperledger-labs/firefly-fabconnect/internal/conf"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/errors"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/fabric"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/kvstore"
	restutil "github.com/hyperledger-labs/firefly-fabconnect/internal/rest/utils"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/utils"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/ws"
	"github.com/julienschmidt/httprouter"
	log "github.com/sirupsen/logrus"
	"github.com/syndtr/goleveldb/leveldb"
)

const (
	// SubPathPrefix is the path prefix for subscriptions
	SubPathPrefix = "/subscriptions"
	// StreamPathPrefix is the path prefix for event streams
	StreamPathPrefix   = "/eventstreams"
	subIDPrefix        = "sb-"
	streamIDPrefix     = "es-"
	checkpointIDPrefix = "cp-"
)

type ResetRequest struct {
	InitialBlock string `json:"initialBlock"`
}

// SubscriptionManager provides REST APIs for managing events
type SubscriptionManager interface {
	Init() error
	AddStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*StreamInfo, *restutil.RestError)
	Streams(res http.ResponseWriter, req *http.Request, params httprouter.Params) []*StreamInfo
	StreamByID(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*StreamInfo, *restutil.RestError)
	UpdateStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*StreamInfo, *restutil.RestError)
	SuspendStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError)
	ResumeStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError)
	DeleteStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError)
	AddSubscription(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*SubscriptionInfo, *restutil.RestError)
	Subscriptions(res http.ResponseWriter, req *http.Request, params httprouter.Params) []*SubscriptionInfo
	SubscriptionByID(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*SubscriptionInfo, *restutil.RestError)
	ResetSubscription(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError)
	DeleteSubscription(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError)
	Close()
}

type subscriptionManager interface {
	getConfig() *conf.EventstreamConf
	streamByID(string) (*eventStream, error)
	subscriptionByID(string) (*subscription, error)
	subscriptionsForStream(string) []*subscription
	loadCheckpoint(string) (map[string]uint64, error)
	storeCheckpoint(string, map[string]uint64) error
}

type subscriptionMGR struct {
	config        *conf.EventstreamConf
	db            kvstore.KVStore
	rpc           fabric.RPCClient
	subscriptions map[string]*subscription
	streams       map[string]*eventStream
	closed        bool
	wsChannels    ws.WebSocketChannels
}

// NewSubscriptionManager constructor
func NewSubscriptionManager(config *conf.EventstreamConf, rpc fabric.RPCClient, wsChannels ws.WebSocketChannels) SubscriptionManager {
	sm := &subscriptionMGR{
		config:        config,
		rpc:           rpc,
		subscriptions: make(map[string]*subscription),
		streams:       make(map[string]*eventStream),
		wsChannels:    wsChannels,
	}
	if config.PollingIntervalSec <= 0 {
		config.PollingIntervalSec = 1
	}
	return sm
}

// SubscriptionByID used externally to get serializable details
func (s *subscriptionMGR) SubscriptionByID(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*SubscriptionInfo, *restutil.RestError) {
	id := params.ByName("subscriptionId")
	sub, err := s.subscriptionByID(id)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 404)
	}
	return sub.info, nil
}

// Subscriptions used externally to get list subscriptions
func (s *subscriptionMGR) Subscriptions(res http.ResponseWriter, req *http.Request, params httprouter.Params) []*SubscriptionInfo {
	l := make([]*SubscriptionInfo, 0, len(s.subscriptions))
	for _, sub := range s.subscriptions {
		l = append(l, sub.info)
	}
	return l
}

// AddSubscription adds a new subscription
func (s *subscriptionMGR) AddSubscription(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*SubscriptionInfo, *restutil.RestError) {
	var spec SubscriptionInfo
	if err := json.NewDecoder(req.Body).Decode(&spec); err != nil {
		return nil, restutil.NewRestError(fmt.Sprintf(errors.RESTGatewaySubscriptionInvalid, err), 400)
	}
	if spec.ChannelId == "" {
		return nil, restutil.NewRestError(`Missing required parameter "channel"`, 400)
	}
	if spec.Stream == "" {
		return nil, restutil.NewRestError(`Missing required parameter "stream"`, 400)
	}
	spec.TimeSorted = TimeSorted{
		CreatedISO8601: time.Now().UTC().Format(time.RFC3339),
	}
	spec.ID = subIDPrefix + utils.UUIDv4()
	spec.Path = SubPathPrefix + "/" + spec.ID
	// Check initial block number to subscribe from
	if spec.FromBlock == "" {
		// user did not set an initial block, default to newest
		spec.FromBlock = FromBlockNewest
	}
	// Create it
	sub, err := newSubscription(s, s.rpc, &spec)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 404)
	}
	s.subscriptions[sub.info.ID] = sub
	subInfo, err := s.storeSubscription(sub.info)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}
	return subInfo, nil
}

func (s *subscriptionMGR) getConfig() *conf.EventstreamConf {
	return s.config
}

// ResetSubscription restarts the steam from the specified block
func (s *subscriptionMGR) ResetSubscription(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError) {
	id := params.ByName("subscriptionId")
	sub, err := s.subscriptionByID(id)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 404)
	}
	var request ResetRequest
	if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
		return nil, restutil.NewRestError(fmt.Sprintf("Failed to parse request body. %s", err), 400)
	}
	err = s.resetSubscription(sub, request.InitialBlock)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}
	result := map[string]string{}
	result["id"] = sub.info.ID
	result["reset"] = "true"
	return &result, nil
}

func (s *subscriptionMGR) resetSubscription(sub *subscription, initialBlock string) error {
	// Re-set the inital block on the subscription and save it
	if initialBlock == "" || initialBlock == FromBlockNewest {
		sub.info.FromBlock = FromBlockNewest
	} else {
		sub.info.FromBlock = initialBlock
	}
	if _, err := s.storeSubscription(sub.info); err != nil {
		return err
	}
	// Request a reset on the next poling cycle
	sub.requestReset()
	return nil
}

// DeleteSubscription deletes a subscription
func (s *subscriptionMGR) DeleteSubscription(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError) {
	id := params.ByName("subscriptionId")
	sub, err := s.subscriptionByID(id)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 404)
	}
	err = s.deleteSubscription(sub)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}
	result := map[string]string{}
	result["id"] = sub.info.ID
	result["deleted"] = "true"
	return &result, nil
}

func (s *subscriptionMGR) deleteSubscription(sub *subscription) error {
	delete(s.subscriptions, sub.info.ID)
	sub.unsubscribe(true)
	if err := s.db.Delete(sub.info.ID); err != nil {
		return err
	}
	return nil
}

func (s *subscriptionMGR) storeSubscription(info *SubscriptionInfo) (*SubscriptionInfo, error) {
	infoBytes, _ := json.MarshalIndent(info, "", "  ")
	if err := s.db.Put(info.ID, infoBytes); err != nil {
		return nil, errors.Errorf(errors.EventStreamsSubscribeStoreFailed, err)
	}
	return info, nil
}

// StreamByID used externally to get serializable details
func (s *subscriptionMGR) StreamByID(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*StreamInfo, *restutil.RestError) {
	streamID := params.ByName("streamId")
	stream, err := s.streamByID(streamID)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 404)
	}
	return stream.spec, nil
}

// Streams used externally to get list streams
func (s *subscriptionMGR) Streams(res http.ResponseWriter, req *http.Request, params httprouter.Params) []*StreamInfo {
	l := make([]*StreamInfo, 0, len(s.subscriptions))
	for _, stream := range s.streams {
		l = append(l, stream.spec)
	}
	return l
}

// AddStream adds a new stream
func (s *subscriptionMGR) AddStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*StreamInfo, *restutil.RestError) {
	var spec StreamInfo
	if err := json.NewDecoder(req.Body).Decode(&spec); err != nil {
		return nil, restutil.NewRestError(fmt.Sprintf(errors.RESTGatewayEventStreamInvalid, err), 400)
	}

	spec.ID = streamIDPrefix + utils.UUIDv4()
	spec.CreatedISO8601 = time.Now().UTC().Format(time.RFC3339)
	spec.Path = StreamPathPrefix + "/" + spec.ID
	stream, err := newEventStream(s, &spec, s.wsChannels)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}
	s.streams[stream.spec.ID] = stream
	streamInfo, err := s.storeStream(stream.spec)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}
	return streamInfo, nil
}

// UpdateStream updates an existing stream
func (s *subscriptionMGR) UpdateStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*StreamInfo, *restutil.RestError) {
	streamID := params.ByName("streamId")
	stream, err := s.streamByID(streamID)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 404)
	}
	var spec StreamInfo
	if err := json.NewDecoder(req.Body).Decode(&spec); err != nil {
		return nil, restutil.NewRestError(fmt.Sprintf(errors.RESTGatewayEventStreamInvalid, err), 400)
	}
	updatedSpec, err := stream.update(&spec)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}
	streamInfo, err := s.storeStream(updatedSpec)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}
	return streamInfo, nil
}

func (s *subscriptionMGR) storeStream(spec *StreamInfo) (*StreamInfo, error) {
	infoBytes, _ := json.MarshalIndent(spec, "", "  ")
	if err := s.db.Put(spec.ID, infoBytes); err != nil {
		return nil, errors.Errorf(errors.EventStreamsCreateStreamStoreFailed, err)
	}
	return spec, nil
}

// DeleteStream deletes a streamm
func (s *subscriptionMGR) DeleteStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError) {
	streamID := params.ByName("streamId")
	stream, err := s.streamByID(streamID)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 404)
	}
	// We have to clean up all the associated subs
	for _, sub := range s.subscriptions {
		if sub.info.Stream == stream.spec.ID {
			err := s.deleteSubscription(sub)
			if err != nil {
				log.Errorf("Failed to delete subscription from database. %s", err)
			}
		}
	}
	delete(s.streams, stream.spec.ID)
	stream.stop()
	if err = s.db.Delete(stream.spec.ID); err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}
	s.deleteCheckpoint(stream.spec.ID)

	result := map[string]string{}
	result["id"] = streamID
	result["deleted"] = "true"
	return &result, nil
}

func (s *subscriptionMGR) subscriptionsForStream(id string) []*subscription {
	subIDs := make([]*subscription, 0)
	for _, sub := range s.subscriptions {
		if sub.info.Stream == id {
			subIDs = append(subIDs, sub)
		}
	}
	return subIDs
}

// SuspendStream suspends a stream from firing
func (s *subscriptionMGR) SuspendStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError) {
	streamID := params.ByName("streamId")
	stream, err := s.streamByID(streamID)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 404)
	}
	stream.suspend()
	// Persist the state change
	_, err = s.storeStream(stream.spec)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}

	result := map[string]string{}
	result["id"] = streamID
	result["suspended"] = "true"
	return &result, nil
}

// ResumeStream restarts a suspended stream
func (s *subscriptionMGR) ResumeStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*map[string]string, *restutil.RestError) {
	streamID := params.ByName("streamId")
	stream, err := s.streamByID(streamID)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 404)
	}
	if err = stream.resume(); err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}
	// Persist the state change
	_, err = s.storeStream(stream.spec)
	if err != nil {
		return nil, restutil.NewRestError(err.Error(), 500)
	}

	result := map[string]string{}
	result["id"] = streamID
	result["resumed"] = "true"
	return &result, nil
}

// subscriptionByID used internally to lookup full objects
func (s *subscriptionMGR) subscriptionByID(id string) (*subscription, error) {
	sub, exists := s.subscriptions[id]
	if !exists {
		return nil, errors.Errorf(errors.EventStreamsSubscriptionNotFound, id)
	}
	return sub, nil
}

// streamByID used internally to lookup full objects
func (s *subscriptionMGR) streamByID(id string) (*eventStream, error) {
	stream, exists := s.streams[id]
	if !exists {
		return nil, errors.Errorf(errors.EventStreamsStreamNotFound, id)
	}
	return stream, nil
}

func (s *subscriptionMGR) loadCheckpoint(streamID string) (map[string]uint64, error) {
	cpID := checkpointIDPrefix + streamID
	b, err := s.db.Get(cpID)
	if err == leveldb.ErrNotFound {
		return make(map[string]uint64), nil
	} else if err != nil {
		return nil, err
	}
	log.Debugf("Loaded checkpoint %s: %s", cpID, string(b))
	var checkpoint map[string]uint64
	err = json.Unmarshal(b, &checkpoint)
	if err != nil {
		return nil, err
	}
	return checkpoint, nil
}

func (s *subscriptionMGR) storeCheckpoint(streamID string, checkpoint map[string]uint64) error {
	cpID := checkpointIDPrefix + streamID
	b, _ := json.MarshalIndent(&checkpoint, "", "  ")
	log.Debugf("Storing checkpoint %s: %s", cpID, string(b))
	return s.db.Put(cpID, b)
}

func (s *subscriptionMGR) deleteCheckpoint(streamID string) {
	cpID := checkpointIDPrefix + streamID
	err := s.db.Delete(cpID)
	if err != nil {
		log.Errorf("Failed to delete checkpoint from database. %s", err)
	}
}

func (s *subscriptionMGR) Init() error {
	s.db = kvstore.NewLDBKeyValueStore(s.config.LevelDB.Path)
	err := s.db.Init()
	if err != nil {
		return errors.Errorf(errors.EventStreamsDBLoad, s.config.LevelDB.Path, err)
	}
	s.recoverStreams()
	s.recoverSubscriptions()
	return nil
}

func (s *subscriptionMGR) recoverStreams() {
	// Recover all the streams
	iStream := s.db.NewIterator()
	defer iStream.Release()
	for iStream.Next() {
		k := iStream.Key()
		if strings.HasPrefix(k, streamIDPrefix) {
			var streamInfo StreamInfo
			err := json.Unmarshal(iStream.Value(), &streamInfo)
			if err != nil {
				log.Errorf("Failed to recover stream '%s': %s", string(iStream.Value()), err)
				continue
			}
			stream, err := newEventStream(s, &streamInfo, s.wsChannels)
			if err != nil {
				log.Errorf("Failed to recover stream '%s': %s", streamInfo.ID, err)
			} else {
				s.streams[streamInfo.ID] = stream
			}
		}
	}
}

func (s *subscriptionMGR) recoverSubscriptions() {
	// Recover all the subscriptions
	iSub := s.db.NewIterator()
	defer iSub.Release()
	for iSub.Next() {
		k := iSub.Key()
		if strings.HasPrefix(k, subIDPrefix) {
			var subInfo SubscriptionInfo
			err := json.Unmarshal(iSub.Value(), &subInfo)
			if err != nil {
				log.Errorf("Failed to recover subscription '%s': %s", string(iSub.Value()), err)
				continue
			}
			sub, err := restoreSubscription(s, s.rpc, &subInfo)
			if err != nil {
				log.Errorf("Failed to recover subscription '%s': %s", subInfo.ID, err)
			} else {
				s.subscriptions[subInfo.ID] = sub
			}
		}
	}
}

func (s *subscriptionMGR) Close() {
	log.Infof("Event stream subscription manager shutting down")
	for _, stream := range s.streams {
		stream.stop()
	}
	for _, sub := range s.subscriptions {
		sub.close()
	}
	if !s.closed && s.db != nil {
		s.db.Close()
	}
	s.closed = true
}
