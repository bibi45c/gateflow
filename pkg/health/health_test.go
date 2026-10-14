package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerReportsServiceAndStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	Handler("gateway").ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type: got %q", got)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["service"] != "gateway" || body["status"] != "ok" {
		t.Errorf("body: got %v", body)
	}
}

func TestReadyHandlerFailsOnceStoppingBegins(t *testing.T) {
	stopping, stop := context.WithCancel(context.Background())
	defer stop()
	handler := ReadyHandler(stopping, "gateway")

	for _, step := range []struct {
		code   int
		status string
	}{{http.StatusOK, "ready"}, {http.StatusServiceUnavailable, "stopping"}} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if recorder.Code != step.code {
			t.Errorf("%s: status: got %d", step.status, recorder.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["service"] != "gateway" || body["status"] != step.status {
			t.Errorf("%s: body: got %v", step.status, body)
		}
		stop()
	}
}
