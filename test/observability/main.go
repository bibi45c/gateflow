// The observability probe measures a small instrumentation prototype on pkg/server.
// It is not a model API, a load-capacity test, or a financial correctness test.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bibi45c/gateflow/pkg/observability"
	"github.com/prometheus/client_golang/prometheus"
)

type countingSink struct {
	mu    sync.Mutex
	bytes int64
}

func (s *countingSink) Write(data []byte) (int, error) {
	s.mu.Lock()
	s.bytes += int64(len(data))
	s.mu.Unlock()
	return len(data), nil
}

func (s *countingSink) Bytes() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.bytes }

type sample struct {
	Number        int      `json:"number"`
	Result        string   `json:"result"`
	DispatchLagMS float64  `json:"dispatch_lag_ms"`
	FirstOutputMS *float64 `json:"first_output_ms"`
	TotalMS       float64  `json:"total_ms"`
}

type condition struct {
	Mode                    string         `json:"mode"`
	Profile                 string         `json:"profile"`
	Repeat                  int            `json:"repeat"`
	Scheduled               int            `json:"scheduled"`
	Completed               int            `json:"completed"`
	Failures                map[string]int `json:"failures"`
	FirstOutputSamples      int            `json:"first_output_samples"`
	P50FirstMS              float64        `json:"p50_first_output_ms"`
	P95FirstMS              float64        `json:"p95_first_output_ms"`
	P95TotalMS              float64        `json:"p95_total_ms"`
	P95DispatchLagMS        float64        `json:"p95_dispatch_lag_ms"`
	ActiveAfterDrain        *float64       `json:"active_after_drain"`
	EligibleIncludingWarmup *float64       `json:"eligible_including_warmup"`
	LogBytesIncludingWarmup int64          `json:"log_bytes_including_warmup"`
	Scrapes                 int            `json:"scrapes"`
	ScrapeFailures          int            `json:"scrape_failures"`
	ScrapeBytes             int64          `json:"scrape_bytes"`
	Series                  int            `json:"scrape_series"`
	HeapAllocEndBytes       uint64         `json:"heap_alloc_end_bytes"`
	Samples                 []sample       `json:"samples"`
}

type settings struct {
	duration, warmup, streamDelay, scrapeInterval time.Duration
	rps, repeats                                  int
}

func perform(client *http.Client, url, body string, stream bool, number int, planned time.Time) sample {
	request, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return sample{Number: number, Result: "request_error"}
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Request-Id", fmt.Sprintf("req_%08x-0000-4000-8000-%012x", number, number))
	started := time.Now()
	result := sample{Number: number, Result: "transport_error", DispatchLagMS: float64(started.Sub(planned)) / float64(time.Millisecond)}
	response, err := client.Do(request)
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, response.Body)
			result.Result = "http_error"
		} else if !stream {
			data, readErr := io.ReadAll(response.Body)
			if readErr == nil && string(data) == plainResponse {
				first := float64(time.Since(started)) / float64(time.Millisecond)
				result.FirstOutputMS = &first
				result.Result = "success"
			} else {
				result.Result = "read_error"
			}
		} else {
			reader := bufio.NewReader(response.Body)
			var frame strings.Builder
			result.Result = "missing_terminal"
			for {
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					if readErr != io.EOF {
						result.Result = "read_error"
					}
					break
				}
				frame.WriteString(line)
				if line != "\n" {
					continue
				}
				data := frame.String()
				frame.Reset()
				if data == outputFrame && result.FirstOutputMS == nil {
					first := float64(time.Since(started)) / float64(time.Millisecond)
					result.FirstOutputMS = &first
				}
				if data == terminalFrame {
					result.Result = "success"
					break
				}
			}
		}
	}
	result.TotalMS = float64(time.Since(started)) / float64(time.Millisecond)
	return result
}

// Scheduled arrivals continue independently of response time. A bounded generator
// rejects excess in-flight work visibly, rather than hiding overload in a queue.
func sendWindow(client *http.Client, url, body string, stream bool, duration time.Duration, rps int) []sample {
	count := int(duration.Seconds() * float64(rps))
	data := make([]sample, count)
	start := time.Now()
	var group sync.WaitGroup
	capacity := make(chan struct{}, 128)
	for i := range data {
		planned := start.Add(time.Duration(i) * time.Second / time.Duration(rps))
		if delay := time.Until(planned); delay > 0 {
			time.Sleep(delay)
		}
		select {
		case capacity <- struct{}{}:
			group.Add(1)
			go func(number int) {
				defer group.Done()
				defer func() { <-capacity }()
				data[number] = perform(client, url, body, stream, number, planned)
			}(i)
		default:
			data[i] = sample{Number: i, Result: "generator_capacity_rejected", DispatchLagMS: float64(time.Since(planned)) / float64(time.Millisecond)}
		}
	}
	group.Wait()
	return data
}

func quantile(values []float64, proportion float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copy := slices.Clone(values)
	slices.Sort(copy)
	index := int(math.Ceil(proportion*float64(len(copy)))) - 1
	return copy[max(0, min(index, len(copy)-1))]
}

func gaugeValue(registry *prometheus.Registry, name string) (float64, error) {
	families, err := registry.Gather()
	if err != nil {
		return 0, err
	}
	for _, family := range families {
		if family.GetName() == name && len(family.Metric) == 1 {
			if family.Metric[0].Gauge != nil {
				return family.Metric[0].Gauge.GetValue(), nil
			}
			if family.Metric[0].Counter != nil {
				return family.Metric[0].Counter.GetValue(), nil
			}
		}
	}
	return 0, fmt.Errorf("required prototype metric absent: %s", name)
}

// newObserver builds the observer of a mode: none for the baseline, logs for json,
// and logs with a registry of its own for metrics and scrape.
func newObserver(mode string, sink io.Writer) (*observability.Observer, *prometheus.Registry, error) {
	if mode == "baseline" {
		return nil, nil, nil
	}
	opts := observability.Options{Service: "gateway", Routes: []string{"/probe"}, ModelAliases: []string{"probe"},
		Logger: slog.New(slog.NewJSONHandler(sink, nil)), GatewaySignals: true}
	var registry *prometheus.Registry
	if mode == "metrics" || mode == "scrape" {
		registry = prometheus.NewRegistry()
		opts.Registry = registry
	}
	observer, err := observability.New(opts)
	return observer, registry, err
}

// scrapeOnce reads the metrics page once and records its size and series count.
func scrapeOnce(ctx context.Context, client *http.Client, url string, result *condition) {
	request, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() == nil {
			result.ScrapeFailures++
		}
		return
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		result.ScrapeFailures++
		return
	}
	result.Scrapes++
	result.ScrapeBytes += int64(len(data))
	series := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			series++
		}
	}
	result.Series = series
}

// startScraper scrapes url at once and then at every interval, when enabled. The
// returned function stops it and waits, after which result is safe to read.
func startScraper(enabled bool, client *http.Client, url string, interval time.Duration, result *condition) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	if !enabled {
		close(done)
	} else {
		go func() {
			defer close(done)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				scrapeOnce(ctx, client, url, result)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	return func() {
		cancel()
		<-done
	}
}

// summarize fills the counts and percentiles of result from its samples.
func summarize(result *condition) {
	var first, total, lag []float64
	for _, sample := range result.Samples {
		if sample.Result == "success" {
			result.Completed++
		} else {
			result.Failures[sample.Result]++
		}
		if sample.FirstOutputMS != nil {
			first = append(first, *sample.FirstOutputMS)
		}
		total = append(total, sample.TotalMS)
		lag = append(lag, sample.DispatchLagMS)
	}
	result.Scheduled = len(result.Samples)
	result.FirstOutputSamples = len(first)
	result.P50FirstMS = quantile(first, .5)
	result.P95FirstMS = quantile(first, .95)
	result.P95TotalMS = quantile(total, .95)
	result.P95DispatchLagMS = quantile(lag, .95)
}

// readDemand records the two demand signals after the drain. Active must be zero.
func readDemand(registry *prometheus.Registry, result *condition) error {
	active, err := gaugeValue(registry, "gateflow_gateway_active_admitted_requests")
	if err != nil {
		return err
	}
	eligible, err := gaugeValue(registry, "gateflow_gateway_eligible_arrivals_total")
	if err != nil {
		return err
	}
	result.ActiveAfterDrain, result.EligibleIncludingWarmup = &active, &eligible
	if active != 0 {
		return fmt.Errorf("admission did not return to zero")
	}
	return nil
}

func runCondition(mode, profile string, repeat int, cfg settings) (condition, error) {
	result := condition{Mode: mode, Profile: profile, Repeat: repeat, Failures: make(map[string]int)}
	stream := profile == "stream"
	sink := new(countingSink)
	observer, registry, err := newObserver(mode, sink)
	if err != nil {
		return result, err
	}
	mux := http.NewServeMux()
	mux.Handle("POST /probe", probeHandler(observer, "/probe", probeOptions{stream: stream, delay: cfg.streamDelay}))
	if observer != nil && observer.MetricsHandler() != nil {
		mux.Handle("GET /metrics", observer.MetricsHandler())
	}
	base, stop, err := startProbe(mux)
	if err != nil {
		return result, err
	}
	transport := &http.Transport{MaxIdleConns: 128, MaxIdleConnsPerHost: 128}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	stopScraper := startScraper(mode == "scrape", client, base+"/metrics", cfg.scrapeInterval, &result)
	body := strings.Repeat("x", 1024)
	warm := sendWindow(client, base+"/probe", body, stream, cfg.warmup, cfg.rps)
	for _, sample := range warm {
		if sample.Result != "success" {
			stopScraper()
			_ = stop()
			return result, fmt.Errorf("warmup did not complete")
		}
	}
	result.Samples = sendWindow(client, base+"/probe", body, stream, cfg.duration, cfg.rps)
	stopScraper()
	if err := stop(); err != nil {
		return result, err
	}
	summarize(&result)
	result.LogBytesIncludingWarmup = sink.Bytes()
	if registry != nil {
		if err := readDemand(registry, &result); err != nil {
			return result, err
		}
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	result.HeapAllocEndBytes = memory.HeapAlloc
	return result, nil
}

// parseSettings reads the flags and stops the program when they are out of bounds.
func parseSettings() (settings, string) {
	var cfg settings
	var output string
	flag.DurationVar(&cfg.duration, "duration", 10*time.Second, "measured window per condition, not the full proposal hold")
	flag.DurationVar(&cfg.warmup, "warmup", time.Second, "warmup per condition")
	flag.DurationVar(&cfg.streamDelay, "stream-delay", time.Millisecond, "test-only stream delay after first output")
	flag.DurationVar(&cfg.scrapeInterval, "scrape-interval", time.Second, "prototype scrape interval")
	flag.IntVar(&cfg.rps, "rps", 100, "scheduled probe requests per second")
	flag.IntVar(&cfg.repeats, "repeats", 3, "repeats; alternate condition order")
	flag.StringVar(&output, "output", "", "optional JSON evidence file; otherwise stdout")
	flag.Parse()
	if cfg.duration <= 0 || cfg.duration > 5*time.Minute || cfg.warmup < 0 || cfg.warmup > 5*time.Minute || cfg.rps < 1 || cfg.rps > 10000 || cfg.repeats < 1 || cfg.repeats > 10 || cfg.streamDelay < 0 || cfg.streamDelay > time.Second || cfg.scrapeInterval < 10*time.Millisecond || cfg.scrapeInterval > time.Minute {
		fmt.Fprintln(os.Stderr, "invalid bounded probe settings")
		os.Exit(2)
	}
	measured := int(cfg.duration.Seconds() * float64(cfg.rps))
	warm := int(cfg.warmup.Seconds() * float64(cfg.rps))
	if measured < 1 || measured*8*cfg.repeats > 250000 || warm > 10000 {
		fmt.Fprintln(os.Stderr, "probe sample count outside bounded limits")
		os.Exit(2)
	}
	return cfg, output
}

// runAll runs every mode on both profiles, once per repeat, reversing the order of
// the modes on even repeats so that no mode always runs first.
func runAll(cfg settings) ([]condition, error) {
	var conditions []condition
	for repeat := 1; repeat <= cfg.repeats; repeat++ {
		modes := []string{"baseline", "json", "metrics", "scrape"}
		if repeat%2 == 0 {
			slices.Reverse(modes)
		}
		for _, profile := range []string{"plain", "stream"} {
			for _, mode := range modes {
				result, err := runCondition(mode, profile, repeat, cfg)
				if err != nil {
					return nil, err
				}
				conditions = append(conditions, result)
				fmt.Fprintf(os.Stderr, "%s/%s repeat=%d completed=%d/%d p95_first_ms=%.3f\n", profile, mode, repeat, result.Completed, result.Scheduled, result.P95FirstMS)
			}
		}
	}
	return conditions, nil
}

// report is the evidence file: the settings of the run and every condition.
type report struct {
	Scope                 string      `json:"scope"`
	Go                    string      `json:"go"`
	OS                    string      `json:"os"`
	Arch                  string      `json:"arch"`
	GOMAXPROCS            int         `json:"gomaxprocs"`
	DurationSeconds       float64     `json:"duration_seconds"`
	WarmupSeconds         float64     `json:"warmup_seconds"`
	RPS                   int         `json:"rps"`
	StreamDelaySeconds    float64     `json:"stream_delay_seconds"`
	ScrapeIntervalSeconds float64     `json:"scrape_interval_seconds"`
	LogSink               string      `json:"log_sink"`
	Conditions            []condition `json:"conditions"`
}

// writeReport saves the report to output, or prints it when output is empty.
func writeReport(cfg settings, conditions []condition, output string) error {
	data, err := json.MarshalIndent(report{
		"skeleton instrumentation probe, not formal capacity or paired-gateway latency acceptance",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0),
		cfg.duration.Seconds(), cfg.warmup.Seconds(), cfg.rps, cfg.streamDelay.Seconds(), cfg.scrapeInterval.Seconds(),
		"JSON serialized to a counting/discard sink; no disk or log shipping measured", conditions}, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode probe report")
	}
	data = append(data, '\n')
	if output == "" {
		_, err = os.Stdout.Write(data)
	} else if err = os.MkdirAll(filepath.Dir(output), 0755); err == nil {
		err = os.WriteFile(output, data, 0644)
	}
	if err != nil {
		return fmt.Errorf("cannot save probe report")
	}
	return nil
}

func main() {
	cfg, output := parseSettings()
	conditions, err := runAll(cfg)
	if err == nil {
		err = writeReport(cfg, conditions, output)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
