package rest

import (
	"encoding/json"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/hyperledger-labs/firefly-fabconnect/internal/errors"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/messages"
	restasync "github.com/hyperledger-labs/firefly-fabconnect/internal/rest/async"
	restsync "github.com/hyperledger-labs/firefly-fabconnect/internal/rest/sync"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/utils"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/ws"
	"github.com/julienschmidt/httprouter"

	log "github.com/sirupsen/logrus"
)

type restErrMsg struct {
	Message string `json:"error"`
}

type router struct {
	syncDispatcher  restsync.SyncDispatcher
	asyncDispatcher restasync.AsyncDispatcher
	ws              ws.WebSocketServer
}

func newRouter(syncDispatcher restsync.SyncDispatcher, asyncDispatcher restasync.AsyncDispatcher, ws ws.WebSocketServer) *router {
	return &router{
		syncDispatcher:  syncDispatcher,
		asyncDispatcher: asyncDispatcher,
		ws:              ws,
	}
}

func (r *router) addRoutes(router *httprouter.Router) {
	// router.POST("/identities", r.restHandler)
	// router.GET("/identities", r.restHandler)
	// router.GET("/identities/:username", r.restHandler)

	router.POST("/transactions", r.sendTransaction)
	router.GET("/receipts", r.handleReceipts)
	router.GET("/receipts/:receiptId", r.handleReceipts)

	// router.POST("/eventstreams", r.createStream)
	// router.PATCH("/eventstreams/:streamId", r.updateStream)
	// router.GET("/eventstreams", r.listStreams)
	// router.GET("/eventstreams/:streamId", r.getStream)
	// router.DELETE("/eventstreams/:streamId", r.deleteStream)
	// router.POST("/eventstreams/:streamId/suspend", r.suspendStream)
	// router.POST("/eventstreams/:streamId/resume", r.resumeStream)
	// router.POST("/eventstreams/:streamId/subscriptions", r.createSubscription)
	// router.GET("/eventstreams/:streamId/subscriptions", r.listSubscription)
	// router.GET("/eventstreams/:streamId/subscriptions/:subscriptionId", r.getSubscription)
	// router.DELETE("/eventstreams/:streamId/subscriptions/:subscriptionId", r.deleteSubscription)
	// router.POST("/eventstreams/:streamId/subscriptions/:subscriptionId/reset", r.resetSubscription)

	router.GET("/ws", r.wsHandler)
	router.GET("/status", r.statusHandler)
}

func (r *router) wsHandler(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	r.ws.NewConnection(res, req, params)
	return
}

func (r *router) statusHandler(res http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	reply, _ := json.Marshal(&statusMsg{OK: true})
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(200)
	res.Write(reply)
	return
}

func (r *router) sendTransaction(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	c, err := r.resolveParams(res, req, params)
	if err != nil {
		return
	}

	msg := &messages.SendTransaction{}
	msg.Headers.MsgType = messages.MsgTypeSendTransaction
	msg.Headers.ChannelID = c["channel"].(string)
	msg.Headers.Signer = c["signer"].(string)
	msg.ChaincodeName = c["chaincode"].(string)
	msg.Function = c["function"].(string)
	msg.Args = c["args"].([]string)

	if strings.ToLower(getFlyParam("sync", req, true)) == "true" {
		r.syncDispatcher.DispatchMsgSync(req.Context(), res, req, msg)
	} else {
		ack := (getFlyParam("noack", req, true) != "true") // turn on ack's by default

		// Async messages are dispatched as generic map payloads.
		// We are confident in the re-serialization here as we've deserialized from JSON then built our own structure
		msgBytes, _ := json.Marshal(msg)
		var mapMsg map[string]interface{}
		json.Unmarshal(msgBytes, &mapMsg)
		if asyncResponse, err := r.asyncDispatcher.DispatchMsgAsync(req.Context(), mapMsg, ack); err != nil {
			errors.RestErrReply(res, req, err, 500)
		} else {
			restAsyncReply(res, req, asyncResponse)
		}
	}
}

func (r *router) handleReceipts(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	r.asyncDispatcher.HandleReceipts(res, req, params)
}

func (r *router) resolveParams(res http.ResponseWriter, req *http.Request, params httprouter.Params) (c map[string]interface{}, err error) {
	channelParam := params.ByName("channel")
	methodParam := params.ByName("function")
	signer := getFlyParam("signer", req, false)
	blocknumber := getFlyParam("blocknumber", req, false)

	body, err := utils.YAMLorJSONPayload(req)
	if err != nil {
		errors.RestErrReply(res, req, err, 400)
		return nil, err
	}

	// consolidate inidividual parameters with the body parameters
	if channelParam != "" {
		body["channel"] = channelParam
	}
	if methodParam != "" {
		body["function"] = methodParam
	}
	if signer != "" {
		body["signer"] = signer
	}
	if blocknumber != "" {
		body["blocknumber"] = blocknumber
	}

	return body, nil
}

func getQueryParamNoCase(name string, req *http.Request) []string {
	name = strings.ToLower(name)
	req.ParseForm()
	for k, vs := range req.Form {
		if strings.ToLower(k) == name {
			return vs
		}
	}
	return nil
}

// getFlyParam standardizes how special 'fly' params are specified, in query params, or headers
func getFlyParam(name string, req *http.Request, isBool bool) string {
	valStr := ""
	vs := getQueryParamNoCase(utils.GetenvOrDefaultLowerCase("PREFIX_SHORT", "fly")+"-"+name, req)
	if len(vs) > 0 {
		valStr = vs[0]
	}
	if isBool && valStr == "" && len(vs) > 0 {
		valStr = "true"
	}
	if valStr == "" {
		valStr = req.Header.Get("x-" + utils.GetenvOrDefaultLowerCase("PREFIX_LONG", "firefly") + "-" + name)
	}
	return valStr
}

// getFlyParamMulti returns an array parameter, or nil if none specified.
// allows multiple query params / headers, or a single comma-separated query param / header
func getFlyParamMulti(name string, req *http.Request) (val []string) {
	req.ParseForm()
	val = getQueryParamNoCase(utils.GetenvOrDefaultLowerCase("PREFIX_SHORT", "fly")+"-"+name, req)
	if len(val) == 0 {
		val = textproto.MIMEHeader(req.Header)[textproto.CanonicalMIMEHeaderKey("x-"+utils.GetenvOrDefaultLowerCase("PREFIX_LONG", "firefly")+"-"+name)]
	}
	if val != nil && len(val) == 1 {
		val = strings.Split(val[0], ",")
	}
	return
}

func restAsyncReply(res http.ResponseWriter, req *http.Request, asyncResponse *messages.AsyncSentMsg) {
	resBytes, _ := json.Marshal(asyncResponse)
	status := 202 // accepted
	log.Infof("<-- %s %s [%d]:\n%s", req.Method, req.URL, status, string(resBytes))
	log.Debugf("<-- %s", resBytes)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
	res.Write(resBytes)
}
