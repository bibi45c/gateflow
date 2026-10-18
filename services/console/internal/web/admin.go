package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bibi45c/gateflow/services/console/internal/backend"
)

type tenantRow struct {
	backend.Tenant
	Wallet       *backend.Wallet
	AdjustmentID string
}

type tenantsData struct {
	Tenants []tenantRow
	Error   string
	Done    string
}

func newAdjustmentID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "adj_" + hex.EncodeToString(b[:])
}

func (s *Server) tenants(w http.ResponseWriter, r *http.Request) {
	s.renderTenants(w, r, http.StatusOK, tenantsData{Done: r.URL.Query().Get("done")})
}

func (s *Server) renderTenants(w http.ResponseWriter, r *http.Request, status int, data tenantsData) {
	tenants, err := s.backend.Tenants(r.Context())
	if err != nil {
		s.fail(w, r, "list the tenants", err)
		return
	}
	wallets, err := s.backend.Wallets(r.Context())
	if err != nil {
		s.fail(w, r, "list the balances", err)
		return
	}
	byTenant := make(map[string]*backend.Wallet, len(wallets))
	for i := range wallets {
		byTenant[wallets[i].TenantID] = &wallets[i]
	}
	for _, t := range tenants {
		// Each form gets a fresh id, so submitting it twice tops up once.
		data.Tenants = append(data.Tenants, tenantRow{Tenant: t, Wallet: byTenant[t.ID], AdjustmentID: newAdjustmentID()})
	}
	s.render(w, r, status, "tenants", "Tenants", data)
}

func (s *Server) adjust(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	amount, err := parseUSD(r.PostFormValue("amount"))
	if err == nil && amount == 0 {
		err = errAmount
	}
	note := strings.TrimSpace(r.PostFormValue("note"))
	if err == nil && (len(note) > 200 || strings.ContainsAny(note, "\r\n")) {
		err = fmt.Errorf("a note has at most 200 characters on one line")
	}
	id := r.PostFormValue("adjustment_id")
	if err == nil && (id == "" || len(id) > 64) {
		err = fmt.Errorf("the form is stale; reload the page")
	}
	if err != nil {
		s.renderTenants(w, r, http.StatusBadRequest, tenantsData{Error: err.Error()})
		return
	}
	if !s.tenantExists(r.Context(), tenantID) {
		s.renderTenants(w, r, http.StatusNotFound, tenantsData{Error: "No tenant has that id."})
		return
	}
	if note == "" {
		note = "console: " + current(r).Username
	}
	adjustment, err := s.backend.Adjust(r.Context(), tenantID, id, amount, note)
	switch backend.Code(err) {
	case "":
	case "insufficient_balance":
		s.renderTenants(w, r, http.StatusConflict, tenantsData{Error: "That correction would take the available balance below 0."})
		return
	case "idempotency_conflict":
		s.renderTenants(w, r, http.StatusConflict, tenantsData{Error: "That form was already used with other values; try again."})
		return
	default:
		s.fail(w, r, "adjust the balance", err)
		return
	}
	s.logger.Info("console adjustment", "user", current(r).Username, "tenant_id", tenantID,
		"adjustment_id", adjustment.ID, "amount", adjustment.Amount)
	done := fmt.Sprintf("%s %s: balance now %s", tenantID, usd(adjustment.Amount), usd(adjustment.BalanceAfter))
	http.Redirect(w, r, "/admin/tenants?done="+url.QueryEscape(done), http.StatusSeeOther)
}

func (s *Server) tenantExists(ctx context.Context, id string) bool {
	tenants, err := s.backend.Tenants(ctx)
	if err != nil {
		return false
	}
	for _, t := range tenants {
		if t.ID == id {
			return true
		}
	}
	return false
}

type catalogData struct {
	Channels []backend.Channel
	Prices   []backend.PriceVersion
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	channels, err := s.backend.Channels(r.Context())
	if err != nil {
		s.fail(w, r, "list the channels", err)
		return
	}
	prices, err := s.backend.PriceVersions(r.Context())
	if err != nil {
		s.fail(w, r, "list the prices", err)
		return
	}
	s.render(w, r, http.StatusOK, "catalog", "Channels and prices", catalogData{Channels: channels, Prices: prices})
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "dashboard", "System dashboard", map[string]any{
		"Grafana": s.opts.GrafanaURL, "HasPrometheus": s.opts.PrometheusURL != ""})
}

// dashboardQueries are the PromQL of the three dashboard numbers; %s is the
// gateway's job. They match the Grafana dashboard's panels.
var dashboardQueries = map[string]string{
	"requests_per_second": `sum(rate(gateflow_http_requests_total{job="%s"}[1m]))`,
	"active_streams":      `sum(gateflow_gateway_active_admitted_requests{job="%s"})`,
	"replicas":            `count(up{job="%s"} == 1)`,
}

type dashboardNumbers struct {
	Values map[string]*float64 `json:"values"`
	Error  string              `json:"error,omitempty"`
	At     time.Time           `json:"at"`
}

func (s *Server) dashboardData(w http.ResponseWriter, r *http.Request) {
	numbers := dashboardNumbers{Values: map[string]*float64{}, At: time.Now().UTC()}
	if s.opts.PrometheusURL == "" {
		numbers.Error = "PROMETHEUS_URL is not set"
	}
	for name, query := range dashboardQueries {
		if numbers.Error != "" {
			break
		}
		value, err := s.promQuery(r.Context(), fmt.Sprintf(query, s.opts.GatewayJob))
		if err != nil {
			numbers.Error = "Prometheus did not answer"
			s.logger.Error("dashboard query failed", "metric", name, "err", err.Error())
			break
		}
		numbers.Values[name] = value
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(numbers)
}

// promQuery runs an instant query and returns the sum of its vector, or nil when
// the vector is empty.
func (s *Server) promQuery(ctx context.Context, query string) (*float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.opts.PrometheusURL+"/api/v1/query?query="+url.QueryEscape(query), nil)
	if err != nil {
		return nil, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var reply struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil || reply.Status != "success" {
		return nil, fmt.Errorf("prometheus: status %d", response.StatusCode)
	}
	if len(reply.Data.Result) == 0 {
		return nil, nil
	}
	total := 0.0
	for _, sample := range reply.Data.Result {
		text, _ := sample.Value[1].(string)
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("prometheus: bad sample")
		}
		total += v
	}
	return &total, nil
}
