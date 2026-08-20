// Package rpc implements the connect-go ProbeService that probe nodes call to
// Lease batches of checks and SubmitResults back (plan §5.1). The service runs
// on ucapi (same port as the HTTP API) and authenticates each probe via the
// shared node token.
package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	probev1 "go.upcontrol.io/back/gen/rpc/probe/v1"
	"go.upcontrol.io/back/internal/detect/availability"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/incident/triage"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// ProbeService implements probev1connect.ProbeServiceHandler.
type ProbeService struct {
	pool      *pg.Pool
	ch        *ch.Conn
	detector  *availability.Detector
	incidents *incident.Lifecycle
	nodeToken string
}

func NewProbeService(pool *pg.Pool, chConn *ch.Conn, lc *incident.Lifecycle, nodeToken string) *ProbeService {
	return &ProbeService{
		pool:      pool,
		ch:        chConn,
		detector:  availability.New(availability.DefaultThreshold),
		incidents: lc,
		nodeToken: nodeToken,
	}
}

// Lease lets a probe take a batch of work.
func (s *ProbeService) Lease(
	ctx context.Context,
	req *connect.Request[probev1.LeaseRequest],
) (*connect.Response[probev1.LeaseResponse], error) {
	if err := authReq(req, s.nodeToken); err != nil {
		return nil, err
	}
	nodeID := req.Msg.NodeId
	capacity := int32(req.Msg.Capacity)
	if capacity <= 0 {
		capacity = 50
	}

	// Heartbeat.
	_ = s.pool.Queries().UpsertProbeNode(ctx, sqlc.UpsertProbeNodeParams{
		ID: nodeID, Region: req.Msg.Region,
	})

	// Find due, unleased monitors.
	due, err := s.pool.Queries().LeaseDueMonitors(ctx, capacity)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(due) == 0 {
		return connect.NewResponse(&probev1.LeaseResponse{NextLeaseAfterMs: 5000}), nil
	}

	// Lease them atomically.
	ids := make([]int64, len(due))
	for i, d := range due {
		ids[i] = d.MonitorID
	}
	_, _ = s.pool.Queries().SetLease(ctx, sqlc.SetLeaseParams{
		LeasedBy:   &nodeID,
		LeaseUntil: pgtype.Timestamptz{Time: time.Now().Add(5 * time.Minute), Valid: true},
		Column3:    ids,
	})

	// Build CheckSpecs.
	checks := make([]*probev1.CheckSpec, 0, len(due))
	for i, d := range due {
		maxBody := uint32(65536)
		if d.Keyword != nil && *d.Keyword != "" {
			maxBody = 262144
		}
		checks = append(checks, &probev1.CheckSpec{
			CheckId:       fmt.Sprintf("%s-%d", nodeID, time.Now().UnixNano()+int64(i)),
			MonitorId:     uint64(d.MonitorID),
			Kind:          probev1.CheckKind_CHECK_KIND_WEBSITE,
			Url:           d.Target,
			Method:        "GET",
			Keyword:       ptrStr(d.Keyword),
			TimeoutMs:     10000,
			MaxRedirects:  5,
			MaxBodyBytes:  maxBody,
			CollectExpiry: true,
		})
	}

	return connect.NewResponse(&probev1.LeaseResponse{
		Checks:           checks,
		NextLeaseAfterMs: 30000,
	}), nil
}

// SubmitResults processes a batch of check results.
func (s *ProbeService) SubmitResults(
	ctx context.Context,
	req *connect.Request[probev1.SubmitResultsRequest],
) (*connect.Response[probev1.SubmitResultsResponse], error) {
	if err := authReq(req, s.nodeToken); err != nil {
		return nil, err
	}

	accepted := uint32(0)
	var checkRows []ch.CheckRow
	for _, res := range req.Msg.Results {
		monitorID := int64(res.MonitorId)

		facts, err := s.pool.Queries().GetMonitorFacts(ctx, monitorID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			continue // monitor deleted or DB error
		}
		// A missing facts row (the very first check) is normal: the zero value
		// is a clean initial state the detector accepts, and UpsertMonitorFacts
		// below creates the row.

		// Run the availability detector.
		state := availability.State{
			Status:              facts.Status,
			ConsecutiveFailures: int(facts.ConsecutiveFailures),
		}
		outcome := s.detector.Process(&state, res.Ok, time.Now())

		// Persist updated facts.
		_ = s.pool.Queries().UpsertMonitorFacts(ctx, sqlc.UpsertMonitorFactsParams{
			MonitorID:           monitorID,
			Status:              state.Status,
			ConsecutiveFailures: int32(state.ConsecutiveFailures),
		})

		// SSL expiry.
		if res.SslExpiresAt != nil {
			_ = s.pool.Queries().UpdateMonitorFactsExpiry(ctx, sqlc.UpdateMonitorFactsExpiryParams{
				MonitorID:    monitorID,
				SslExpiresAt: pgtype.Timestamptz{Time: res.SslExpiresAt.AsTime(), Valid: true},
			})
		}

		// Clear lease + schedule the next check at the monitor's REAL interval
		// (not a hardcoded 5m: a 1m monitor must be checked every minute). The
		// same lookup yields the tenant_id needed to label the raw check row.
		var tenantID int64
		intervalSec := float64(300)
		if mi, err := s.pool.Queries().GetMonitorInterval(ctx, monitorID); err == nil {
			intervalSec = float64(mi.IntervalSec)
			tenantID = mi.TenantID
		}
		_ = s.pool.Queries().ClearLeaseAndSchedule(ctx, sqlc.ClearLeaseAndScheduleParams{
			Column1:   intervalSec,
			MonitorID: monitorID,
		})

		// Record the raw check in ClickHouse (feeds the status page + history).
		// A deleted monitor (tenantID 0) is dropped, not recorded.
		if tenantID != 0 {
			checkRows = append(checkRows, ch.CheckRow{
				TenantID: uint64(tenantID), MonitorID: res.MonitorId, TS: time.Now(),
				Region: req.Msg.Region, OK: res.Ok, StatusCode: uint16(res.StatusCode),
				ErrorClass: errClassStr(res.ErrorClass),
				DNSMs:      res.DnsMs, ConnectMs: res.ConnectMs, TLSMs: res.TlsMs,
				TTFBMs: res.TtfbMs, TotalMs: res.TotalMs, BodyHash: res.BodyHash,
			})
		}

		// Incident lifecycle (§5.8): open on detector fire, close on recovery.
		if outcome.Open && s.incidents != nil {
			title := monitorTitle(ctx, s.pool, monitorID, res)
			if _, _, err := s.incidents.Open(ctx, monitorID, title); err != nil {
				// Best-effort: a failed open does not fail the batch.
				_ = err
			}
		}
		if outcome.Close && s.incidents != nil {
			_ = s.incidents.Close(ctx, monitorID, incident.ReasonRecovered)
		}

		accepted++
	}

	// Persist the raw check rows (best-effort: a CH hiccup must not fail submit).
	if s.ch != nil {
		_ = s.ch.InsertChecks(ctx, checkRows)
	}
	return connect.NewResponse(&probev1.SubmitResultsResponse{Accepted: accepted}), nil
}

// ReportBlind lets a probe declare itself blind.
func (s *ProbeService) ReportBlind(
	ctx context.Context,
	req *connect.Request[probev1.ReportBlindRequest],
) (*connect.Response[probev1.ReportBlindResponse], error) {
	if err := authReq(req, s.nodeToken); err != nil {
		return nil, err
	}
	nodeID := req.Msg.NodeId
	_ = s.pool.Queries().MarkProbeBlind(ctx, nodeID)
	_, _ = s.pool.Queries().ClearLeasesForNode(ctx, &nodeID)
	return connect.NewResponse(&probev1.ReportBlindResponse{}), nil
}

// --- helpers ---

func authReq[T any](req *connect.Request[T], token string) error {
	h := req.Header().Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") || strings.TrimPrefix(h, "Bearer ") != token {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid node token"))
	}
	return nil
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// errClassStr maps a probev1.ErrorClass back to the short executor string
// stored in checks.error_class (mirrors ucprobe's mapErrClass in reverse).
func errClassStr(c probev1.ErrorClass) string {
	switch c {
	case probev1.ErrorClass_ERROR_CLASS_DNS:
		return "dns"
	case probev1.ErrorClass_ERROR_CLASS_CONNECT:
		return "connect"
	case probev1.ErrorClass_ERROR_CLASS_TLS:
		return "tls"
	case probev1.ErrorClass_ERROR_CLASS_TIMEOUT:
		return "timeout"
	case probev1.ErrorClass_ERROR_CLASS_STATUS:
		return "status"
	case probev1.ErrorClass_ERROR_CLASS_KEYWORD_MISSING:
		return "keyword_missing"
	case probev1.ErrorClass_ERROR_CLASS_BLOCKED_TARGET:
		return "blocked_target"
	case probev1.ErrorClass_ERROR_CLASS_NONE:
		return "none"
	default:
		return ""
	}
}

// monitorTitle builds a human-readable incident title from the monitor + result.
func monitorTitle(ctx context.Context, pool *pg.Pool, monitorID int64, res *probev1.CheckResult) string {
	mon, err := pool.Queries().GetMonitorForIncident(ctx, monitorID)
	if err != nil {
		return "Service down"
	}
	name := mon.Name
	if name == "" {
		name = mon.Target
	}
	// The errorClass→title mapping lives in triage (its buildTitle produced
	// these exact strings before the switch was replaced by this call).
	return triage.Build(name, errClassStr(res.ErrorClass), int(res.StatusCode), nil).Title
}
