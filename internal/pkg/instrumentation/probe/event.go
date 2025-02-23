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

package probe

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Event is a telemetry event that happens within an instrumented package.
type Event struct {
	Library           string
	Name              string
	Attributes        []attribute.KeyValue
	Kind              trace.SpanKind
	StartTime         int64
	EndTime           int64
	SpanContext       *trace.SpanContext
	ParentSpanContext *trace.SpanContext
}
