package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// The agent's session_metrics payload crosses three places that must agree:
// the manifest (what the key means), agentws.SessionMetricsMsg (what the
// control plane will even parse), and buildAgentMetrics (what reaches
// storage). encode_ms_max was published by the agent, listed in the manifest,
// and silently dropped here because it was missing from the latter two —
// every test was green. This test closes that hole in both directions.
func TestAgentMetricsStructMatchesManifest(t *testing.T) {
	tags := map[string]bool{}
	rt := reflect.TypeOf(agentws.SessionMetricsMsg{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			tags[tag] = true
		}
	}
	// Envelope fields, not metrics.
	envelope := map[string]bool{"type": true, "session_id": true, "ts_unix_ms": true, "ts": true, "bytes_used": true, "window_ms": true}

	for _, e := range telemetry.Manifest().Metrics {
		if e.Source != "agent" || !e.IsStored() {
			continue
		}
		if !tags[e.Key] {
			t.Errorf("manifest agent key %q has no field on agentws.SessionMetricsMsg — the control plane drops it at parse", e.Key)
		}
	}

	// Every parsed metric field must survive the projection. Fill every
	// pointer with a sentinel and read back the stored object.
	v := reflect.New(rt).Elem()
	for i := 0; i < rt.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() == reflect.String && f.CanSet() {
			f.SetString("x")
			continue
		}
		if f.Kind() == reflect.Ptr && f.CanSet() {
			p := reflect.New(f.Type().Elem())
			switch p.Elem().Kind() {
			case reflect.Float64:
				p.Elem().SetFloat(1)
			case reflect.Int, reflect.Int64:
				p.Elem().SetInt(1)
			case reflect.String:
				p.Elem().SetString("x")
			case reflect.Bool:
				p.Elem().SetBool(true)
			}
			f.Set(p)
		}
	}
	var stored map[string]any
	if err := json.Unmarshal(buildAgentMetrics(v.Interface().(agentws.SessionMetricsMsg)), &stored); err != nil {
		t.Fatal(err)
	}
	for tag := range tags {
		if envelope[tag] {
			continue
		}
		if _, ok := stored[tag]; !ok {
			t.Errorf("agentws.SessionMetricsMsg field %q is parsed but buildAgentMetrics never stores it", tag)
		}
	}
}
