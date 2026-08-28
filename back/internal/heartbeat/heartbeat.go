// Package heartbeat is the monitor the customer's own job pings. A ping is a
// passing check and a window that closes without one is a failed check; both
// go through the availability detector, so incidents, alerts and uptime read
// a heartbeat the way they read every other monitor.
package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/detect/availability"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// region tags the synthetic check rows; readers that measure milliseconds
// skip it, a heartbeat has none.
const region = "heartbeat"

// Service answers the ping route (ucapi) and sweeps closed windows (ucworker).
type Service struct {
	pool *pg.Pool
	pgs  *pgstore.Store // optional: nil records no check rows
	det  *availability.Detector
	lc   *incident.Lifecycle
}

// New builds the service. Threshold 1: the window already carries the grace,
// so one miss is a miss, not a blip.
func New(pool *pg.Pool, pgs *pgstore.Store, lc *incident.Lifecycle) *Service {
	return &Service{
		pool: pool,
		pgs:  pgs,
		det:  availability.New(1),
		lc:   lc,
	}
}

// ServeHTTP answers GET|POST /public/ping/{token}. The token is the only
// credential and the body is never read.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	found, err := s.Ping(r.Context(), r.PathValue("token"))
	switch {
	case err != nil:
		writePingErr(w, http.StatusInternalServerError, "internal", "Recording the ping failed.")
	case !found:
		writePingErr(w, http.StatusNotFound, "not_found", "Unknown ping token.")
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK\n"))
	}
}

func writePingErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}

// Ping records one beat; found is false only for an unknown token.
func (s *Service) Ping(ctx context.Context, token string) (found bool, err error) {
	// ping_token is nullable in the schema, so sqlc takes a pointer; an empty
	// path segment never reaches here (the mux would not match).
	row, err := s.pool.Queries().GetMonitorByPingToken(ctx, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A paused monitor answers OK but records nothing: nobody asked it to run.
	if row.Paused {
		return true, nil
	}
	st := availability.State{Status: availability.StatusNoData}
	if facts, ferr := s.pool.Queries().GetMonitorFacts(ctx, row.ID); ferr == nil {
		st.Status = facts.Status
		st.ConsecutiveFailures = int(facts.ConsecutiveFailures)
	} else if !errors.Is(ferr, pgx.ErrNoRows) {
		return true, ferr
	}
	if err := s.apply(ctx, row.ID, row.TenantID, row.Name, st, true, time.Now()); err != nil {
		return true, err
	}
	// A ping pushes the whole window out: interval + grace.
	return true, s.pool.Queries().SetHeartbeatDue(ctx, sqlc.SetHeartbeatDueParams{
		Secs:      float64(row.IntervalSec + row.GraceSec),
		MonitorID: row.ID,
	})
}

// Tick records a failed check for every heartbeat whose window closed.
func (s *Service) Tick(ctx context.Context) error {
	rows, err := s.pool.Queries().ListMissedHeartbeats(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, row := range rows {
		// A failed row must not fail the sweep; with next_due_at still in the
		// past, the next tick retries it.
		st := availability.State{
			Status:              row.Status,
			ConsecutiveFailures: int(row.ConsecutiveFailures),
		}
		if err := s.apply(ctx, row.ID, row.TenantID, row.Name, st, false, now); err != nil {
			continue
		}
		// One down per expected beat, not one per minute: the next miss row
		// lands a full interval later.
		_ = s.pool.Queries().SetHeartbeatDue(ctx, sqlc.SetHeartbeatDueParams{
			Secs:      float64(row.IntervalSec),
			MonitorID: row.ID,
		})
	}
	return nil
}

// apply is the dance every check result goes through: update the facts,
// record the raw row, open or close the incident. It mirrors
// rpc.ProbeService.SubmitResults per result.
func (s *Service) apply(ctx context.Context, monitorID, tenantID int64, name string,
	st availability.State, ok bool, now time.Time) error {
	out := s.det.Process(&st, ok, now)
	if err := s.pool.Queries().UpsertMonitorFacts(ctx, sqlc.UpsertMonitorFactsParams{
		MonitorID:           monitorID,
		Status:              st.Status,
		ConsecutiveFailures: int32(st.ConsecutiveFailures),
	}); err != nil {
		return err
	}
	// The raw row feeds uptime and history; its ms columns stay 0 and the
	// millisecond readers exclude the region.
	if s.pgs != nil {
		errClass := ""
		if !ok {
			errClass = "missed"
		}
		_ = s.pgs.InsertChecks(ctx, []pgstore.CheckRow{{
			TenantID: uint64(tenantID), MonitorID: uint64(monitorID), TS: now,
			Region: region, OK: ok, ErrorClass: errClass,
		}})
	}
	if out.Open {
		_, _, _ = s.lc.Open(ctx, monitorID, name+" has not checked in")
	}
	if out.Close {
		_ = s.lc.Close(ctx, monitorID, incident.ReasonRecovered)
	}
	return nil
}
