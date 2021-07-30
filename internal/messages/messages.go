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

package messages

import (
	"encoding/json"
	"reflect"
)

// Types of messages that fabconnect internally posts to the message queue (kafka)
// as well as those that it sends back to the client
const (
	// MsgTypeError - an error
	MsgTypeError = "Error"
	// MsgTypeDeployContract - deploy a contract
	MsgTypeDeployContract = "DeployContract"
	// MsgTypeSendTransaction - send a transaction
	MsgTypeSendTransaction = "SendTransaction"
	// MsgTypeTransactionSuccess - a transaction receipt where status is 1
	MsgTypeTransactionSuccess = "TransactionSuccess"
	// MsgTypeTransactionFailure - a transaction receipt where status is 0
	MsgTypeTransactionFailure = "TransactionFailure"
	// RecordHeaderAccessToken - record header name for passing JWT token over messaging
	RecordHeaderAccessToken = "fly-accesstoken"
)

// AsyncSentMsg is a standard response for async requests
type AsyncSentMsg struct {
	Sent    bool   `json:"sent"`
	Request string `json:"id"`
	Msg     string `json:"msg,omitempty"`
}

// CommonHeaders are common to all messages
type CommonHeaders struct {
	ID        string                 `json:"id,omitempty"`
	MsgType   string                 `json:"type,omitempty"`
	Signer    string                 `json:"signer,omitempty"`
	ChannelID string                 `json:"channel,omitempty"`
	Context   map[string]interface{} `json:"ctx,omitempty"`
}

// RequestHeaders are common to all requests
type RequestHeaders struct {
	CommonHeaders
}

// ReplyHeaders are common to all replies
type ReplyHeaders struct {
	CommonHeaders
	Received  string  `json:"timeReceived"`
	Elapsed   float64 `json:"timeElapsed"`
	ReqOffset string  `json:"requestOffset"`
	ReqID     string  `json:"requestId"`
}

// RequestCommon is a common interface to all requests
type RequestCommon struct {
	Headers RequestHeaders `json:"headers"`
}

// ReplyCommon is a common interface to all replies
type ReplyCommon struct {
	Headers ReplyHeaders `json:"headers"`
}

// ReplyWithHeaders gives common access the reply headers
type ReplyWithHeaders interface {
	ReplyHeaders() *ReplyHeaders
}

// ReplyHeaders returns the reply headers
func (r *ReplyCommon) ReplyHeaders() *ReplyHeaders {
	return &r.Headers
}

// TransactionCommon is the common fields
// for sending either contract call or creation transactions
type TransactionCommon struct {
	RequestCommon
	ChaincodeName string `json:"chaincode"`
}

// SendTransaction message instructs the bridge to install a contract
type SendTransaction struct {
	TransactionCommon
	Function string   `json:"func"`
	Args     []string `json:"args,omitempty"`
}

// DeployChaincode message instructs the bridge to install a contract
type DeployChaincode struct {
	TransactionCommon
	ChaincodeName string `json:"contractName,omitempty"`
	Description   string `json:"description,omitempty"`
}

// TransactionReceipt is sent when a transaction has been successfully mined
// For the big numbers, we pass a simple string as well as a full
// ethereum hex encoding version
type TransactionReceipt struct {
	ReplyCommon
}

type ErrorReply struct {
	ReplyCommon
	ErrorMessage    string `json:"errorMessage,omitempty"`
	OriginalMessage string `json:"requestPayload,omitempty"`
	TXHash          string `json:"transactionHash,omitempty"`
}

// NewErrorReply is a helper to construct an error message
func NewErrorReply(err error, origMsg interface{}) *ErrorReply {
	var errMsg ErrorReply
	errMsg.Headers.MsgType = MsgTypeError
	if err != nil {
		errMsg.ErrorMessage = err.Error()
	}
	if reflect.TypeOf(origMsg).Kind() == reflect.Slice {
		errMsg.OriginalMessage = string(origMsg.([]byte))
	} else {
		origMsgBytes, _ := json.Marshal(origMsg)
		if origMsgBytes != nil {
			errMsg.OriginalMessage = string(origMsgBytes)
		}
	}
	return &errMsg
}
