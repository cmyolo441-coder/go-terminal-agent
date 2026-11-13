package telemetry

import (
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/version"
)

// SDKName is reported as telemetry.sdk.name.
const SDKName = "gta-telemetry"

// SDKVersion is reported as telemetry.sdk.version.
const SDKVersion = "1.0.0"

// DefaultServiceName is used when nothing configures service.name.
const DefaultServiceName = "gta"

// Resource describes the entity producing telemetry. It is immutable.
type Resource struct {
	set attrSet
}

// NewResource builds a resource; later duplicates of a key win.
func NewResource(attrs ...KeyValue) *Resource {
	return &Resource{set: newAttrSet(attrs)}
}

// Attributes returns the resource attributes sorted by key.
func (r *Resource) Attributes() []KeyValue {
	if r == nil {
		return nil
	}
	return append([]KeyValue(nil), r.set.kvs...)
}

// Get returns one attribute value.
func (r *Resource) Get(key string) (Value, bool) {
	if r == nil {
		return Value{}, false
	}
	for _, kv := range r.set.kvs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return Value{}, false
}

// Merge returns a resource with the attributes of both; o wins on conflict.
func (r *Resource) Merge(o *Resource) *Resource {
	all := append(r.Attributes(), o.Attributes()...)
	return NewResource(all...)
}

// defaultResource describes this process: SDK, runtime, host and service
// identity. service.instance.id is random per process, so restarts are
// distinguishable in a backend.
func defaultResource() *Resource {
	attrs := []KeyValue{
		String("service.name", DefaultServiceName),
		String("service.version", version.Version),
		String("service.instance.id", newUUID()),
		String("telemetry.sdk.name", SDKName),
		String("telemetry.sdk.language", "go"),
		String("telemetry.sdk.version", SDKVersion),
		String("process.runtime.name", "go"),
		String("process.runtime.version", runtime.Version()),
		Int("process.pid", os.Getpid()),
		String("os.type", runtime.GOOS),
		String("host.arch", runtime.GOARCH),
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		attrs = append(attrs, String("host.name", h))
	}
	return NewResource(attrs...)
}

// parseKeyValueList parses the W3C-Baggage-style lists used by
// OTEL_RESOURCE_ATTRIBUTES and OTEL_EXPORTER_OTLP_HEADERS:
// "k1=v1,k2=v2" with percent-encoded values. Any malformed member rejects
// the whole list, as the specification requires.
func parseKeyValueList(s string) ([][2]string, error) {
	var out [][2]string
	for _, member := range strings.Split(s, ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		k, v, ok := strings.Cut(member, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("telemetry: malformed key=value member %q", member)
		}
		dk, err := url.PathUnescape(k)
		if err != nil {
			return nil, fmt.Errorf("telemetry: bad percent-encoding in key %q", k)
		}
		dv, err := url.PathUnescape(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("telemetry: bad percent-encoding in value of %q", k)
		}
		out = append(out, [2]string{dk, dv})
	}
	return out, nil
}

// resourceFromEnv reads OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME;
// OTEL_SERVICE_NAME wins over a service.name in the attribute list.
func resourceFromEnv() (*Resource, error) {
	var attrs []KeyValue
	var err error
	if v := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); strings.TrimSpace(v) != "" {
		pairs, perr := parseKeyValueList(v)
		if perr != nil {
			err = fmt.Errorf("OTEL_RESOURCE_ATTRIBUTES ignored: %w", perr)
		}
		for _, p := range pairs {
			attrs = append(attrs, String(p[0], p[1]))
		}
	}
	if v := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); v != "" {
		attrs = append(attrs, String("service.name", v))
	}
	return NewResource(attrs...), err
}
