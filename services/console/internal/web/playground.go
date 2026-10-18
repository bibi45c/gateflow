package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Playground limits.
const (
	maxPromptBytes       = 8 << 10
	maxPlaygroundOut     = 4096
	defaultPlaygroundOut = 256
)

type playgroundRequest struct {
	Key       string `json:"key"`
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
}

func (s *Server) playground(w http.ResponseWriter, r *http.Request) {
	channels, err := s.backend.Channels(r.Context())
	if err != nil {
		s.fail(w, r, "list the models", err)
		return
	}
	rows := catalogModels(channels)
	s.render(w, r, http.StatusOK, "playground", "Playground", modelsData{Models: rows, Mock: servedMocks(rows)})
}

// playgroundStream sends one streamed Messages call through the real gateway with
// the user's own gateway key and relays the gateway's answer, status and SSE bytes
// as they arrive. The browser therefore needs no CORS access to the gateway. The key
// is used for this call only; it is never stored or logged.
func (s *Server) playgroundStream(w http.ResponseWriter, r *http.Request) {
	var request playgroundRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPromptBytes+4<<10))
	if err := decoder.Decode(&request); err != nil || request.Key == "" || request.Model == "" ||
		strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > maxPromptBytes ||
		request.MaxTokens < 0 || request.MaxTokens > maxPlaygroundOut {
		http.Error(w, "need a key, a model, a prompt of at most 8 KiB and max_tokens of at most 4096", http.StatusBadRequest)
		return
	}
	if request.MaxTokens == 0 {
		request.MaxTokens = defaultPlaygroundOut
	}
	body, _ := json.Marshal(map[string]any{
		"model":      request.Model,
		"max_tokens": request.MaxTokens,
		"stream":     true,
		"messages":   []map[string]string{{"role": "user", "content": request.Prompt}},
	})
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.opts.GatewayURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad gateway URL", http.StatusInternalServerError)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("x-api-key", request.Key)
	upstream.Header.Set("anthropic-version", "2023-06-01")
	response, err := s.client.Do(upstream)
	if err != nil {
		s.logger.Error("playground call failed", "user", current(r).Username, "err", "gateway unreachable")
		http.Error(w, "the gateway did not answer", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "request-id", "Retry-After"} {
		if v := response.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(response.StatusCode)
	relay(w, response.Body)
}

// relay copies src to w, flushing after every read so tokens reach the browser as
// they arrive.
func relay(w http.ResponseWriter, src io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return // io.EOF ends the stream; anything else cuts it, as the browser sees
		}
	}
}
