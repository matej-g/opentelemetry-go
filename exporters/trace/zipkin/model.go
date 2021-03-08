// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package zipkin // import "go.opentelemetry.io/otel/exporters/trace/zipkin"

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	zkmodel "github.com/openzipkin/zipkin-go/model"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	export "go.opentelemetry.io/otel/sdk/export/trace"
	"go.opentelemetry.io/otel/semconv"
	"go.opentelemetry.io/otel/trace"
)

const (
	keyInstrumentationLibraryName    = "otel.library.name"
	keyInstrumentationLibraryVersion = "otel.library.version"

	keyPeerHostname attribute.Key = "peer.hostname"
	keyPeerAddress  attribute.Key = "peer.address"
)

func toZipkinSpanModels(batch []*export.SpanSnapshot, serviceName string) []zkmodel.SpanModel {
	models := make([]zkmodel.SpanModel, 0, len(batch))
	for _, data := range batch {
		models = append(models, toZipkinSpanModel(data, serviceName))
	}
	return models
}

func toZipkinSpanModel(data *export.SpanSnapshot, serviceName string) zkmodel.SpanModel {
	return zkmodel.SpanModel{
		SpanContext: toZipkinSpanContext(data),
		Name:        data.Name,
		Kind:        toZipkinKind(data.SpanKind),
		Timestamp:   data.StartTime,
		Duration:    data.EndTime.Sub(data.StartTime),
		Shared:      false,
		LocalEndpoint: &zkmodel.Endpoint{
			ServiceName: serviceName,
		},
		RemoteEndpoint: toZipkinRemoteEndpoint(data),
		Annotations:    toZipkinAnnotations(data.MessageEvents),
		Tags:           toZipkinTags(data),
	}
}

func toZipkinSpanContext(data *export.SpanSnapshot) zkmodel.SpanContext {
	return zkmodel.SpanContext{
		TraceID:  toZipkinTraceID(data.SpanContext.TraceID()),
		ID:       toZipkinID(data.SpanContext.SpanID()),
		ParentID: toZipkinParentID(data.ParentSpanID),
		Debug:    false,
		Sampled:  nil,
		Err:      nil,
	}
}

func toZipkinTraceID(traceID trace.TraceID) zkmodel.TraceID {
	return zkmodel.TraceID{
		High: binary.BigEndian.Uint64(traceID[:8]),
		Low:  binary.BigEndian.Uint64(traceID[8:]),
	}
}

func toZipkinID(spanID trace.SpanID) zkmodel.ID {
	return zkmodel.ID(binary.BigEndian.Uint64(spanID[:]))
}

func toZipkinParentID(spanID trace.SpanID) *zkmodel.ID {
	if spanID.IsValid() {
		id := toZipkinID(spanID)
		return &id
	}
	return nil
}

func toZipkinKind(kind trace.SpanKind) zkmodel.Kind {
	switch kind {
	case trace.SpanKindUnspecified:
		return zkmodel.Undetermined
	case trace.SpanKindInternal:
		// The spec says we should set the kind to nil, but
		// the model does not allow that.
		return zkmodel.Undetermined
	case trace.SpanKindServer:
		return zkmodel.Server
	case trace.SpanKindClient:
		return zkmodel.Client
	case trace.SpanKindProducer:
		return zkmodel.Producer
	case trace.SpanKindConsumer:
		return zkmodel.Consumer
	}
	return zkmodel.Undetermined
}

func toZipkinAnnotations(events []trace.Event) []zkmodel.Annotation {
	if len(events) == 0 {
		return nil
	}
	annotations := make([]zkmodel.Annotation, 0, len(events))
	for _, event := range events {
		value := event.Name
		if len(event.Attributes) > 0 {
			jsonString := attributesToJSONMapString(event.Attributes)
			if jsonString != "" {
				value = fmt.Sprintf("%s: %s", event.Name, jsonString)
			}
		}
		annotations = append(annotations, zkmodel.Annotation{
			Timestamp: event.Time,
			Value:     value,
		})
	}
	return annotations
}

func attributesToJSONMapString(attributes []attribute.KeyValue) string {
	m := make(map[string]interface{}, len(attributes))
	for _, attribute := range attributes {
		m[(string)(attribute.Key)] = attribute.Value.AsInterface()
	}
	// if an error happens, the result will be an empty string
	jsonBytes, _ := json.Marshal(m)
	return (string)(jsonBytes)
}

// extraZipkinTags are those that may be added to every outgoing span
var extraZipkinTags = []string{
	"otel.status_code",
	keyInstrumentationLibraryName,
	keyInstrumentationLibraryVersion,
}

func toZipkinTags(data *export.SpanSnapshot) map[string]string {
	m := make(map[string]string, len(data.Attributes)+len(extraZipkinTags))
	for _, kv := range data.Attributes {
		switch kv.Value.Type() {
		// For array attributes, serialize as JSON list string.
		case attribute.ARRAY:
			json, _ := json.Marshal(kv.Value.AsArray())
			m[(string)(kv.Key)] = (string)(json)
		default:
			m[(string)(kv.Key)] = kv.Value.Emit()
		}
	}

	if data.StatusCode != codes.Unset {
		m["otel.status_code"] = data.StatusCode.String()
	}

	if data.StatusCode == codes.Error {
		m["error"] = data.StatusMessage
	}

	// If boolean with 'false' is present, should be removed.
	if v, ok := m["error"]; ok && v == "false" {
		delete(m, "error")
	}

	if il := data.InstrumentationLibrary; il.Name != "" {
		m[keyInstrumentationLibraryName] = il.Name
		if il.Version != "" {
			m[keyInstrumentationLibraryVersion] = il.Version
		}
	}

	if len(m) == 0 {
		return nil
	}

	return m
}

// Rank determines selection order for remote endpoint. See the specification
// https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/trace/sdk_exporters/zipkin.md#remote-endpoint
var remoteEndpointKeyRank = map[attribute.Key]int{
	semconv.PeerServiceKey: 0,
	semconv.NetPeerNameKey: 1,
	semconv.NetPeerIPKey:   2,
	keyPeerHostname:        3,
	keyPeerAddress:         4,
	semconv.HTTPHostKey:    5,
	semconv.DBNameKey:      6,
}

func toZipkinRemoteEndpoint(data *export.SpanSnapshot) *zkmodel.Endpoint {
	// Should be set only for consumer or producer kind
	if data.SpanKind != trace.SpanKindConsumer &&
		data.SpanKind != trace.SpanKindProducer {
		return nil
	}

	var endpointAttr attribute.KeyValue
	for _, kv := range data.Attributes {
		rank, ok := remoteEndpointKeyRank[kv.Key]
		if !ok {
			continue
		}

		currentKeyRank, ok := remoteEndpointKeyRank[endpointAttr.Key]
		if !ok {
			endpointAttr = kv
		} else {
			if rank < currentKeyRank {
				endpointAttr = kv
			}
		}
	}

	if endpointAttr.Key == "" {
		return nil
	}

	if endpointAttr.Key != semconv.NetPeerIPKey &&
		endpointAttr.Value.Type() == attribute.STRING {
		return &zkmodel.Endpoint{
			ServiceName: endpointAttr.Value.AsString(),
		}
	}

	return remoteEndpointPeerIPWithPort(endpointAttr.Value.AsString(), data.Attributes)
}

// Handles `net.peer.ip` remote endpoint separately (should include `net.peer.ip`
// as well, if available).
func remoteEndpointPeerIPWithPort(peerIP string, attrs []attribute.KeyValue) *zkmodel.Endpoint {
	ip := net.ParseIP(peerIP)
	if ip == nil {
		return nil
	}

	endpoint := &zkmodel.Endpoint{}
	// Determine if IPv4 or IPv6
	if ip.To4() != nil {
		endpoint.IPv4 = ip
	} else {
		endpoint.IPv6 = ip
	}

	for _, kv := range attrs {
		if kv.Key == semconv.NetPeerPortKey {
			port, _ := strconv.ParseUint(kv.Value.Emit(), 10, 16)
			endpoint.Port = uint16(port)
			return endpoint
		}
	}

	return endpoint
}
