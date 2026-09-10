// Package rpc implements the connect-go ProbeService probes call to Lease
// checks and SubmitResults; it runs on ucapi, authenticated by node token.
package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	probev1 "go.upcontrol.io/back/gen/rpc/probe/v1"
	"go.upcontrol.io/back/internal/detect/availability"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/incident/triage"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// ProbeService implements probev1connect.ProbeServiceHandler.
type ProbeService struct {
	pool      *pg.Pool
	pgs       *pgstore.Store
	detector  *availability.Detector
	incidents *incident.Lifecycle
	nodeToken string
}

func NewProbeService(pool *pg.Pool, pgs *pgstore.Store, lc *incident.Lifecycle, nodeToken string) *ProbeService {
	return &ProbeService{
		pool:      pool,
		pgs:       pgs,
		detector:  availability.New(availability.DefaultThreshold),
		incidents: lc,
		nodeToken: nodeToken,
	}
}

// resultsDroppedNoTarget counts CheckResults that arrived without a
// target_id since process start: pre-migration wire shapes, dropped per the
// plan (a batch leased before the deploy must not be written as somebody
// else's measurement), but counted, never silently lost.
var resultsDroppedNoTarget atomic.Uint64

// resultsDroppedTargetRead counts CheckResults dropped because the target's
// facts could not be read (a target deleted mid-batch, a DB error): logged
// per occurrence, and counted so the loss stays visible in ops.
var resultsDroppedTargetRead atomic.Uint64

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

	// Find due, unleased targets (a target with neither an unpaused subscriber
	// nor a live host page is simply never due).
	due, err := s.pool.Queries().LeaseDueTargets(ctx, capacity)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(due) == 0 {
		return connect.NewResponse(&probev1.LeaseResponse{NextLeaseAfterMs: 5000}), nil
	}

	// Lease them atomically; the admission predicate evicts expired leases,
	// so a node that lost its batch does not freeze the target for everyone.
	ids := make([]int64, len(due))
	for i, d := range due {
		ids[i] = d.ID
		if d.PrevLeasedBy != nil && *d.PrevLeasedBy != "" {
			// A stale lease was just recovered: the old holder never submitted.
			// Logged, because a fleet that silently freezes is the defect this
			// recovery exists to remove.
			slog.Warn("stale_lease_recovered", "node", *d.PrevLeasedBy, "target", d.ID)
		}
	}
	_ = s.pool.Queries().SetLease(ctx, sqlc.SetLeaseParams{
		LeasedBy: &nodeID,
		Column2:  ids,
	})

	// Build CheckSpecs. monitor_id stays unset (deprecated on the wire); the
	// target is the work unit now.
	checks := make([]*probev1.CheckSpec, 0, len(due))
	for i, d := range due {
		maxBody := uint32(65536)
		if d.Keyword != nil && *d.Keyword != "" {
			maxBody = 262144
		}
		checks = append(checks, &probev1.CheckSpec{
			CheckId:       fmt.Sprintf("%s-%d", nodeID, time.Now().UnixNano()+int64(i)),
			TargetId:      uint64(d.ID),
			Kind:          probev1.CheckKind_CHECK_KIND_WEBSITE,
			Url:           d.Url,
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

// SubmitResults processes a batch of check results: one pass per target —
// facts, detector, first-ok stamp, reschedule at the derived cadence, the
// raw checks row, then the incident fan-out by subscription set.
func (s *ProbeService) SubmitResults(
	ctx context.Context,
	req *connect.Request[probev1.SubmitResultsRequest],
) (*connect.Response[probev1.SubmitResultsResponse], error) {
	if err := authReq(req, s.nodeToken); err != nil {
		return nil, err
	}

	accepted := uint32(0)
	droppedNoTarget := uint32(0)
	var checkRows []pgstore.CheckRow
	for _, res := range req.Msg.Results {
		// A result without target_id is a pre-migration wire shape (or a bug);
		// writing it would attribute somebody else's URL. Dropped and counted.
		if res.TargetId == 0 {
			droppedNoTarget++
			continue
		}
		targetID := int64(res.TargetId)

		facts, err := s.pool.Queries().GetTargetFacts(ctx, targetID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			// No silent loss: the result is dropped, so the drop is named.
			resultsDroppedTargetRead.Add(1)
			slog.Warn("result dropped: target facts unreadable", "target", targetID, "err", err)
			continue
		}
		// A missing facts row (the very first check) is normal: the zero value
		// is a clean initial state; UpsertTargetFacts below creates the row.

		// The effective cadence this row was taken at: the checks row, the
		// reschedule and the incident's stamp all record the same number.
		interval := int32(900)
		if eff, err := s.pool.Queries().EffectiveIntervalForTarget(ctx, targetID); err == nil {
			interval = eff
		}

		// Could-not-measure (part 3): a bot filter or auth wall answered, the
		// probe did not see the service. Not a failure for the detector, no
		// incident; the raw row is still stored (ok=false, status as measured)
		// and the uptime queries exclude it by this exact predicate.
		unmeasured := res.ErrorClass == probev1.ErrorClass_ERROR_CLASS_STATUS &&
			(res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 429)

		state := availability.State{
			Status:                facts.Status,
			ConsecutiveFailures:   int(facts.ConsecutiveFailures),
			ConsecutiveUnmeasured: int(facts.ConsecutiveUnmeasured),
		}
		kind := availability.OutcomeFail
		if res.Ok {
			kind = availability.OutcomeOK
		} else if unmeasured {
			kind = availability.OutcomeUnmeasured
		}
		outcome := s.detector.Process(&state, kind, time.Now())

		// Refusal backoff (part 2): an unmeasured target is asked less often,
		// doubling from the effective interval and capped at 24 h; a served
		// Retry-After floors the delay. Any measurable result resets it.
		refusals, backoffUntil := backoffFor(unmeasured,
			int(facts.ConsecutiveRefusals), int(interval), res.RetryAfterSec)

		_ = s.pool.Queries().UpsertTargetFacts(ctx, sqlc.UpsertTargetFactsParams{
			TargetID:              targetID,
			Status:                state.Status,
			ConsecutiveFailures:   int32(state.ConsecutiveFailures),
			ConsecutiveUnmeasured: int32(state.ConsecutiveUnmeasured),
			ConsecutiveRefusals:   refusals,
			BackoffUntil:          backoffUntil,
		})

		// SSL expiry.
		if res.SslExpiresAt != nil {
			_ = s.pool.Queries().UpdateTargetFactsExpiry(ctx, sqlc.UpdateTargetFactsExpiryParams{
				TargetID:     targetID,
				SslExpiresAt: pgtype.Timestamptz{Time: res.SslExpiresAt.AsTime(), Valid: true},
			})
		}

		// The first ok stamps first_ok_at once; the reaper's host-page
		// exemption and the index gate read it.
		if res.Ok {
			_ = s.pool.Queries().SetTargetFirstOk(ctx, targetID)
		}

		_ = s.pool.Queries().ClearLeaseAndScheduleTarget(ctx, sqlc.ClearLeaseAndScheduleTargetParams{
			Column1:  float64(interval),
			TargetID: targetID,
		})

		// One row per result, on the target: every subscriber and the host
		// page read the same history.
		checkRows = append(checkRows, pgstore.CheckRow{
			TargetID: res.TargetId, IntervalSec: uint32(interval), TS: time.Now(),
			Region: req.Msg.Region, OK: res.Ok, StatusCode: uint16(res.StatusCode),
			ErrorClass: errClassStr(res.ErrorClass),
			DNSMs:      res.DnsMs, ConnectMs: res.ConnectMs, TLSMs: res.TlsMs,
			TTFBMs: res.TtfbMs, TotalMs: res.TotalMs, BodyHash: res.BodyHash,
		})

		// Incident fan-out by set, not by edge: while the target is down every
		// unpaused subscription holds an open incident (a subscriber who joins
		// an outage gets its alert within one check); recovery closes every
		// subscription, paused or not. Unmeasured results do neither.
		if !unmeasured && s.incidents != nil {
			if state.Status == availability.StatusDown {
				if subs, serr := s.pool.Queries().ActiveSubscriberMonitors(ctx, targetID); serr == nil {
					for _, monitorID := range subs {
						title := monitorTitle(ctx, s.pool, monitorID, res)
						// Open writes effective_interval_sec itself (the cadence this
						// row was taken at, review decision 14). A failed open is a
						// lost alert, never a silent one: logged with both ids.
						if _, _, oerr := s.incidents.Open(ctx, monitorID, title, interval); oerr != nil {
							slog.Warn("incident open failed", "target", targetID, "monitor", monitorID, "err", oerr)
						}
					}
				}
			}
			if outcome.Close {
				if subs, serr := s.pool.Queries().AllSubscriberMonitors(ctx, targetID); serr == nil {
					for _, monitorID := range subs {
						_ = s.incidents.Close(ctx, monitorID, incident.ReasonRecovered)
					}
				}
			}
		}

		accepted++
	}

	if droppedNoTarget > 0 {
		resultsDroppedNoTarget.Add(uint64(droppedNoTarget))
		// One line per batch, not per row.
		slog.Warn("results without target_id dropped", "count", droppedNoTarget)
	}

	// Persist the raw check rows; InsertChecks logs its own failures.
	if s.pgs != nil {
		_ = s.pgs.InsertChecks(ctx, checkRows)
	}
	return connect.NewResponse(&probev1.SubmitResultsResponse{Accepted: accepted}), nil
}

// backoffFor computes the refusal backoff in Go (the doubling is policy, the
// database only stores the result): interval * 2^min(refusals, 6) capped at
// 24 h after the Nth consecutive refusal, with a served Retry-After as the
// floor. A measurable result resets both counters to clean.
func backoffFor(unmeasured bool, prevRefusals, intervalSec int, retryAfterSec int32) (int32, pgtype.Timestamptz) {
	if !unmeasured {
		return 0, pgtype.Timestamptz{}
	}
	n := prevRefusals + 1
	shift := n
	if shift > 6 {
		shift = 6
	}
	delay := time.Duration(intervalSec) * time.Second * (1 << uint(shift))
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	if ra := time.Duration(retryAfterSec) * time.Second; ra > delay {
		delay = ra
	}
	return int32(n), pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true}
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
	_ = s.pool.Queries().ClearLeasesForNode(ctx, &nodeID)
	return connect.NewResponse(&probev1.ReportBlindResponse{}), nil
}

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
	return triage.Build(name, errClassStr(res.ErrorClass), int(res.StatusCode)).Title
}
