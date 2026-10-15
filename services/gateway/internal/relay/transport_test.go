package relay

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// NewTransport keeps its idle connections, in all and to each host, for 90 s; the
// timeouts and the rest of its settings are those it had before.
func TestNewTransportSettings(t *testing.T) {
	if DefaultMaxIdleConns != 256 {
		t.Errorf("DefaultMaxIdleConns = %d, want 256", DefaultMaxIdleConns)
	}
	for _, tt := range []struct{ given, want int }{{0, DefaultMaxIdleConns}, {-1, DefaultMaxIdleConns}, {32, 32}} {
		transport := NewTransport(tt.given)
		if transport.MaxIdleConns != tt.want || transport.MaxIdleConnsPerHost != tt.want {
			t.Errorf("NewTransport(%d): idle connections %d, per host %d; want %d for both",
				tt.given, transport.MaxIdleConns, transport.MaxIdleConnsPerHost, tt.want)
		}
		if transport.IdleConnTimeout != 90*time.Second {
			t.Errorf("idle timeout = %v, want 90s", transport.IdleConnTimeout)
		}
		if transport.TLSHandshakeTimeout != connectTimeout || transport.ResponseHeaderTimeout != 0 ||
			transport.MaxConnsPerHost != 0 || transport.Proxy != nil || !transport.DisableCompression ||
			!transport.ForceAttemptHTTP2 || transport.DialContext == nil {
			t.Errorf("NewTransport(%d) changed a setting other than the idle connections: %+v", tt.given, transport)
		}
	}
}

// Calls that run together to one host get their connections back from the idle
// pool the next time, instead of opening new ones, as a load test needs. With Go's
// default of two idle connections per host, the second round would open 14.
func TestNewTransportReusesConnectionsToOneHost(t *testing.T) {
	const calls = 16
	var opened atomic.Int64
	var round atomic.Pointer[barrier]
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round.Load().await() // every call of the round holds its own connection
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	transport := NewTransport(0)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	for range 2 {
		round.Store(newBarrier(calls))
		var done sync.WaitGroup
		for range calls {
			done.Go(func() {
				resp, err := client.Get(server.URL)
				if err != nil {
					t.Error(err)
					return
				}
				// Reading to the end puts the connection back before Get's caller goes on.
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			})
		}
		done.Wait()
	}
	if got := opened.Load(); got != calls {
		t.Errorf("two rounds of %d calls opened %d connections, want %d", calls, got, calls)
	}
}

// barrier holds its callers until n of them wait, or for at most 5 s.
type barrier struct {
	arrived sync.WaitGroup
	all     chan struct{}
}

func newBarrier(n int) *barrier {
	b := &barrier{all: make(chan struct{})}
	b.arrived.Add(n)
	go func() {
		b.arrived.Wait()
		close(b.all)
	}()
	return b
}

func (b *barrier) await() {
	b.arrived.Done()
	select {
	case <-b.all:
	case <-time.After(5 * time.Second):
	}
}
