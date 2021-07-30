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

package identity

import (
	"net/http"

	restutil "github.com/hyperledger-labs/firefly-fabconnect/internal/rest/utils"
	"github.com/julienschmidt/httprouter"
)

type Identity struct {
	RegisterResponse
	MaxEnrollments int    `json:"maxEnrollments"`
	Type           string `json:"type"`
	Affiliation    string `json:"affiliation"`
	CAName         string `json:"caname"`
}

type RegisterResponse struct {
	Name   string `json:"name"`
	Secret string `json:"secret,omitempty"`
}

type EnrollRequest struct {
	Name    string `json:"name"`
	Secret  string `json:"secret"`
	CAName  string `json:"caname"`
	Profile string `json:"profile"`
	CSR     string `json:"csr"`
}

type IdentityResponse struct {
	Name    string `json:"name"`
	Success bool   `json:"success"`
}

type IdentityClient interface {
	Register(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*RegisterResponse, *restutil.RestError)
	Enroll(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*IdentityResponse, *restutil.RestError)
	List(res http.ResponseWriter, req *http.Request, params httprouter.Params) ([]*Identity, *restutil.RestError)
	Get(res http.ResponseWriter, req *http.Request, params httprouter.Params) (*Identity, *restutil.RestError)
}
