package main

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/observability"
	"github.com/prometheus/client_golang/prometheus"
)

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func testProbe(t *testing.T, opts probeOptions) (string, *prometheus.Registry, *lockedBuffer) {
	t.Helper()
	registry := prometheus.NewRegistry()
	logs := new(lockedBuffer)
	observer, err := observability.New(observability.Options{Service: "gateway", Routes: []string{"/probe"},
		ModelAliases: []string{"probe"}, GatewaySignals: true, Registry: registry, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	base, stop, err := startProbe(probeHandler(observer, "/probe", opts))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	return base, registry, logs
}

func requestMetric(t *testing.T, registry *prometheus.Registry, name, result string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matches := result == ""
			for _, label := range metric.Label {
				if label.GetName() == "result" && label.GetValue() == result {
					matches = true
				}
			}
			if !matches {
				continue
			}
			if metric.Gauge != nil {
				return metric.Gauge.GetValue()
			}
			if metric.Counter != nil {
				return metric.Counter.GetValue()
			}
			if metric.Histogram != nil {
				return float64(metric.Histogram.GetSampleCount())
			}
		}
	}
	t.Fatalf("missing %s result=%s", name, result)
	return 0
}

func awaitMetric(t *testing.T, registry *prometheus.Registry, name, result string, expected float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if requestMetric(t, registry, name, result) == expected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s did not become %v", name, expected)
}

func TestFirstOutputArrivesWhileProducerIsBlockedAndBytesAreUnchanged(t *testing.T) {
	release := make(chan struct{})
	base, registry, _ := testProbe(t, probeOptions{stream: true, afterFirst: release})
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(base, "application/json", strings.NewReader("input"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var prefix strings.Builder
	for i := 0; i < 6; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		prefix.WriteString(line)
	}
	if prefix.String() != metadataFrame+outputFrame {
		t.Fatalf("frame bytes changed: %q", prefix.String())
	}
	select {
	case <-release:
		t.Fatal("producer was not blocked")
	default:
	}
	awaitMetric(t, registry, "gateflow_gateway_active_admitted_requests", "", 1)
	awaitMetric(t, registry, "gateflow_gateway_first_output_seconds", "", 1)
	close(release)
	tail, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail) != terminalFrame {
		t.Fatalf("terminal bytes changed: %q", tail)
	}
	awaitMetric(t, registry, "gateflow_gateway_active_admitted_requests", "", 0)
	awaitMetric(t, registry, "gateflow_http_requests_total", "completed", 1)
}

func TestClientDisconnectReleasesAdmission(t *testing.T) {
	base, registry, _ := testProbe(t, probeOptions{stream: true, afterFirst: make(chan struct{})})
	response, err := (&http.Client{Timeout: 5 * time.Second}).Post(base, "application/json", strings.NewReader("input"))
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	for i := 0; i < 6; i++ {
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	response.Body.Close()
	awaitMetric(t, registry, "gateflow_http_requests_total", "client_cancelled", 1)
	awaitMetric(t, registry, "gateflow_gateway_active_admitted_requests", "", 0)
}

func TestProbeBodiesHeadersQueriesAndErrorOutputAreNeverLogged(t *testing.T) {
	const canary = "body-credential-CANARY-93847"
	errorData := "event: error\ndata: {\"message\":\"" + canary + "\"}\n\n"
	base, registry, logs := testProbe(t, probeOptions{stream: true, streamError: true, errorOutput: errorData})
	request, err := http.NewRequest("POST", base+"?secret="+canary, strings.NewReader(canary))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+canary)
	request.Header.Set("X-Request-Id", canary)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || string(data) != metadataFrame+outputFrame+errorData {
		t.Fatal("stream error fixture changed")
	}
	awaitMetric(t, registry, "gateflow_http_requests_total", "upstream_error", 1)
	if requestMetric(t, registry, "gateflow_http_requests_total", "completed") != 0 {
		t.Fatal("SSE error counted as completed")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), `"msg":"request"`) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(logs.String(), `"msg":"request"`) {
		t.Fatal("terminal log missing")
	}
	if strings.Contains(logs.String(), canary) {
		t.Fatal("body, query, credential or error response leaked")
	}
}

func TestMetadataArrivesWithoutStartingFirstOutputTiming(t *testing.T) {
	release := make(chan struct{})
	base, registry, _ := testProbe(t, probeOptions{stream: true, afterMetadata: release})
	response, err := (&http.Client{Timeout: 5 * time.Second}).Post(base, "application/json", strings.NewReader("input"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var metadata strings.Builder
	for i := 0; i < 3; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		metadata.WriteString(line)
	}
	if metadata.String() != metadataFrame {
		t.Fatal("metadata bytes changed")
	}
	if requestMetric(t, registry, "gateflow_gateway_first_output_seconds", "") != 0 {
		t.Fatal("metadata counted as model output")
	}
	awaitMetric(t, registry, "gateflow_gateway_active_admitted_requests", "", 1)
	close(release)
	tail, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail) != outputFrame+terminalFrame {
		t.Fatal("model or terminal bytes changed")
	}
	awaitMetric(t, registry, "gateflow_gateway_first_output_seconds", "", 1)
}

func TestGeneratorCompletesOnTerminalEventWithoutWaitingForEOF(t *testing.T) {
	producer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, outputFrame+terminalFrame)
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		<-request.Context().Done()
	}))
	defer producer.Close()
	client := &http.Client{Timeout: time.Second}
	started := time.Now()
	result := perform(client, producer.URL, "input", true, 1, started)
	if result.Result != "success" || result.FirstOutputMS == nil || result.TotalMS >= 900 {
		t.Fatalf("terminal success waited for EOF: %+v", result)
	}
}
