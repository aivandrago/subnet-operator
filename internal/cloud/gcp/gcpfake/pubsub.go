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

package gcpfake

import (
	"context"
	"encoding/json"
	"fmt"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// PubSub is Pub/Sub in memory for the change event tests: pstest, the fake the client library
// ships, served over gRPC so the provider's subscriber runs with the real client, its
// streaming pull and its acknowledgements. A log sink is played by publishing audit log
// entries (AuditLogEntry) to the topic.
type PubSub struct {
	server *pstest.Server
	conn   *grpc.ClientConn
}

// NewPubSub starts the fake. Close stops it.
func NewPubSub() (*PubSub, error) {
	srv := pstest.NewServer()
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("connect to pstest: %w", err)
	}
	return &PubSub{server: srv, conn: conn}, nil
}

// Close stops the fake.
func (p *PubSub) Close() {
	_ = p.conn.Close()
	_ = p.server.Close()
}

// ClientOptions point a Pub/Sub client at the fake.
func (p *PubSub) ClientOptions() []option.ClientOption {
	return []option.ClientOption{option.WithGRPCConn(p.conn)}
}

// CreateTopic creates a topic by its full name (projects/<project>/topics/<name>).
func (p *PubSub) CreateTopic(ctx context.Context, topic string) error {
	_, err := p.server.GServer.CreateTopic(ctx, &pubsubpb.Topic{Name: topic})
	return err
}

// CreateSubscription creates a pull subscription of the topic, by their full names.
func (p *PubSub) CreateSubscription(ctx context.Context, subscription, topic string) error {
	_, err := p.server.GServer.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subscription, Topic: topic,
		AckDeadlineSeconds: 10})
	return err
}

// Publish publishes a message to the topic and returns its ID.
func (p *PubSub) Publish(topic string, data []byte) string {
	return p.server.Publish(topic, data, nil)
}

// Acked reports whether the message with the ID was acknowledged.
func (p *PubSub) Acked(id string) bool {
	m := p.server.Message(id)
	return m != nil && m.Acks > 0
}

// AuditLogEntry is an Admin Activity audit log entry as a log sink publishes it to Pub/Sub:
// a call of the method on the resource by the principal, which succeeded.
func AuditLogEntry(project, service, method, resourceName, principal string) []byte {
	e := map[string]any{
		"protoPayload": map[string]any{
			"@type":              "type.googleapis.com/google.cloud.audit.AuditLog",
			"status":             map[string]any{},
			"authenticationInfo": map[string]any{"principalEmail": principal},
			"serviceName":        service,
			"methodName":         method,
			"resourceName":       resourceName,
		},
		"resource": map[string]any{"labels": map[string]string{"project_id": project}},
		"logName":  "projects/" + project + "/logs/cloudaudit.googleapis.com%2Factivity",
		"severity": "NOTICE",
	}
	b, err := json.Marshal(e)
	if err != nil {
		panic(err) // maps of strings always marshal
	}
	return b
}
