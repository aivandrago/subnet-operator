/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azurefake

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Azure Queue Storage, as much of it as the change event source uses (#55), served by the same
// server as Resource Manager under /QueueAccount, so the provider reads it with the real SDK
// client (azqueue), its XML and its bearer token policy:
//
//	GET    /{account}/{queue}/messages?numofmessages=&visibilitytimeout=   Get Messages
//	DELETE /{account}/{queue}/messages/{id}?popreceipt=                    Delete Message
//
// A received message is hidden for the visibility timeout and comes back with a higher dequeue
// count and a new pop receipt; only the latest pop receipt deletes it. Like the service there
// is no long polling: an empty queue answers at once. It fails the way the service does: 404
// QueueNotFound, 403 AuthorizationPermissionMismatch for a principal without a data role on the
// queue (RestrictQueue) or without the delete permission (DenyQueueDeletes), 401 without a
// token the fake issued. Event Grid is not faked: Publish puts a resource event in the queue
// the way Event Grid delivers it, Base64 of the event's JSON, and Enqueue any text at all.

// QueueAccount is the name of the fake's storage account.
const QueueAccount = "subnetoperatorevents"

type queue struct {
	messages []*queueMessage
	// receivers are the only principals it answers, nil for everyone.
	receivers []string
	// denyDeletes refuses Delete Message, as for an identity that may only read.
	denyDeletes bool
	nextID      int
}

type queueMessage struct {
	id, text, popReceipt string
	inserted, visibleAt  time.Time
	dequeueCount         int
}

// AddQueue creates an empty queue.
func (c *Cloud) AddQueue(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queues[name] = &queue{}
}

// RemoveQueue deletes a queue and its messages.
func (c *Cloud) RemoveQueue(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.queues, name)
}

// QueueURL is the URL of a queue, whether it exists or not.
func (c *Cloud) QueueURL(name string) string {
	return c.server.URL + "/" + QueueAccount + "/" + name
}

// RestrictQueue makes the queue answer only these principals (client IDs, or Operator).
func (c *Cloud) RestrictQueue(name string, principals ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queues[name].receivers = lower(principals)
}

// DenyQueueDeletes makes the queue refuse Delete Message, or accept it again.
func (c *Cloud) DenyQueueDeletes(name string, deny bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queues[name].denyDeletes = deny
}

// Enqueue puts a message with this text in the queue and returns its ID. It takes any text of
// any size, which the service would not: a test decides what the operator is faced with.
func (c *Cloud) Enqueue(name, text string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.queues[name]
	q.nextID++
	m := &queueMessage{id: fmt.Sprintf("%s-%04d", name, q.nextID), text: text, inserted: time.Now()}
	q.messages = append(q.messages, m)
	return m.id
}

// Publish puts an event in the queue as Event Grid does, Base64-encoded, and returns the
// message ID.
func (c *Cloud) Publish(name string, event []byte) string {
	return c.Enqueue(name, base64.StdEncoding.EncodeToString(event))
}

// ResourceEvent is a resource event of a subscription's system topic in the Event Grid schema,
// as Event Grid delivers it: eventType is Microsoft.Resources.ResourceWriteSuccess and the
// like, resourceID the target of the operation, which is the subject, in the spelling given.
func ResourceEvent(eventType, resourceID string) []byte {
	return resourceEvent(resourceID, map[string]any{"eventType": eventType, "eventTime": "2026-10-05T09:12:44.4Z",
		"dataVersion": "2", "metadataVersion": "1", "topic": topicOf(resourceID)})
}

// CloudEvent is the same event in the CloudEvents 1.0 schema, which an event subscription can
// ask for instead.
func CloudEvent(eventType, resourceID string) []byte {
	return resourceEvent(resourceID, map[string]any{"type": eventType, "time": "2026-10-05T09:12:44.4Z",
		"specversion": "1.0", "source": topicOf(resourceID)})
}

func resourceEvent(resourceID string, event map[string]any) []byte {
	subscription := strings.TrimPrefix(topicOf(resourceID), "/subscriptions/")
	event["id"] = "4db48cba-50a2-455a-93b4-de41a3b5b7f6"
	event["subject"] = resourceID
	event["data"] = map[string]any{
		"authorization": map[string]any{"scope": resourceID, "action": "Microsoft.Network/virtualNetworks/write"},
		"claims":        map[string]any{"name": "Maria K", "appid": "c44b4083-3bb0-49c1-b47d-974e53cbdf3c"},
		"correlationId": "9f1d3d0c-0000-4000-8000-0000000000c1", "resourceProvider": "Microsoft.Network",
		"resourceUri": resourceID, "operationName": "Microsoft.Network/virtualNetworks/write",
		"status": "Succeeded", "subscriptionId": subscription, "tenantId": DefaultTenant,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	return raw
}

// topicOf is the system topic's source: the subscription of the resource.
func topicOf(resourceID string) string {
	parts := strings.Split(resourceID, "/")
	if len(parts) < 3 {
		return ""
	}
	return "/subscriptions/" + parts[2]
}

// QueueLength is how many messages the queue holds, hidden ones included.
func (c *Cloud) QueueLength(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q := c.queues[name]; q != nil {
		return len(q.messages)
	}
	return 0
}

// Queued reports whether the message is still in the queue, and how often it was received.
func (c *Cloud) Queued(name, id string) (queued bool, dequeueCount int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.queues[name]
	if q == nil {
		return false, 0
	}
	for _, m := range q.messages {
		if m.id == id {
			return true, m.dequeueCount
		}
	}
	return false, 0
}

// beginQueue records the request and answers it when it cannot go ahead. It returns the queue,
// or nil when it answered.
func (c *Cloud) beginQueue(w http.ResponseWriter, r *http.Request) *queue {
	c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	who, issued := c.tokens[bearer]
	if !ok || !issued {
		writeStorageError(w, http.StatusUnauthorized, "InvalidAuthenticationInfo", "Server failed to authenticate "+
			"the request. Please refer to the information in the www-authenticate header.")
		return nil
	}
	c.requestsBy[who.client] = append(c.requestsBy[who.client], r.Method+" "+r.URL.RequestURI())
	q := c.queues[r.PathValue("queue")]
	if q == nil {
		writeStorageError(w, http.StatusNotFound, "QueueNotFound", "The specified queue does not exist.")
		return nil
	}
	if (q.receivers != nil && !slices.Contains(q.receivers, who.client)) ||
		(q.denyDeletes && r.Method == http.MethodDelete) {
		writeStorageError(w, http.StatusForbidden, "AuthorizationPermissionMismatch", "This request is not "+
			"authorized to perform this operation using this permission.")
		return nil
	}
	return q
}

// writeStorageError answers the way Azure Storage does: an XML body and the code in a header.
func writeStorageError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-ms-error-code", code)
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`,
		code, message)
}

type queueMessagesList struct {
	XMLName  xml.Name          `xml:"QueueMessagesList"`
	Messages []queueMessageXML `xml:"QueueMessage"`
}

type queueMessageXML struct {
	MessageID       string `xml:"MessageId"`
	InsertionTime   string
	ExpirationTime  string
	PopReceipt      string
	TimeNextVisible string
	DequeueCount    int
	MessageText     string
}

func (c *Cloud) getMessages(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.beginQueue(w, r)
	if q == nil {
		return
	}
	query := r.URL.Query()
	count, visibility := 1, 30
	if v, err := strconv.Atoi(query.Get("numofmessages")); err == nil {
		count = v
	}
	if v, err := strconv.Atoi(query.Get("visibilitytimeout")); err == nil {
		visibility = v
	}
	if count < 1 || count > 32 || visibility < 1 {
		writeStorageError(w, http.StatusBadRequest, "OutOfRangeQueryParameterValue", "One of the query parameters "+
			"specified in the request URI is outside the permissible range.")
		return
	}
	now := time.Now()
	list := queueMessagesList{}
	for _, m := range q.messages {
		if len(list.Messages) == count {
			break
		}
		if m.visibleAt.After(now) {
			continue
		}
		c.nextReceipt++
		m.dequeueCount++
		m.popReceipt = "receipt-" + strconv.Itoa(c.nextReceipt)
		m.visibleAt = now.Add(time.Duration(visibility) * time.Second)
		list.Messages = append(list.Messages, queueMessageXML{MessageID: m.id, PopReceipt: m.popReceipt,
			InsertionTime:   m.inserted.UTC().Format(http.TimeFormat),
			ExpirationTime:  m.inserted.Add(7 * 24 * time.Hour).UTC().Format(http.TimeFormat),
			TimeNextVisible: m.visibleAt.UTC().Format(http.TimeFormat),
			DequeueCount:    m.dequeueCount, MessageText: m.text})
	}
	raw, err := xml.Marshal(list)
	if err != nil {
		writeStorageError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write(append([]byte(xml.Header), raw...))
}

func (c *Cloud) deleteMessage(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.beginQueue(w, r)
	if q == nil {
		return
	}
	i := slices.IndexFunc(q.messages, func(m *queueMessage) bool { return m.id == r.PathValue("id") })
	switch {
	case i < 0:
		writeStorageError(w, http.StatusNotFound, "MessageNotFound", "The specified message does not exist.")
	case q.messages[i].popReceipt == "" || q.messages[i].popReceipt != r.URL.Query().Get("popreceipt"):
		writeStorageError(w, http.StatusBadRequest, "PopReceiptMismatch", "The specified pop receipt did not match "+
			"the pop receipt for a dequeued message.")
	default:
		q.messages = slices.Delete(q.messages, i, i+1)
		w.WriteHeader(http.StatusNoContent)
	}
}
