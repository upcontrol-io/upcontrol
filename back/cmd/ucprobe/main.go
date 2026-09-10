// Command ucprobe is the regional probe node: no DB secrets (depguard
// enforced); it polls ucapi for checks and runs them through the SSRF guard.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	probev1 "go.upcontrol.io/back/gen/rpc/probe/v1"
	probev1connect "go.upcontrol.io/back/gen/rpc/probe/v1/probev1connect"
	"go.upcontrol.io/back/internal/platform/app"
	"go.upcontrol.io/back/internal/probe/executor"
)

const (
	// fetchConcurrency is the worker-pool width for one batch. A fixed
	// constant, not runtime.NumCPU: a fetch is a network wait, not CPU work,
	// so core count says nothing about how many checks a node can hold in
	// flight, and a fixed width keeps the fleet's outbound footprint
	// predictable.
	fetchConcurrency = 20

	// leaseCapacity is the batch size one Lease call asks for.
	leaseCapacity = 50

	// batchBudget bounds one full lease-execute cycle; checkTimeout is one
	// check's timeout and the budget guard's threshold (no fetch starts with
	// less than that remaining); submitBudget bounds SubmitResults alone, so
	// collected results reach the server even when the batch budget is spent.
	batchBudget  = 70 * time.Second
	checkTimeout = 10 * time.Second
	submitBudget = 20 * time.Second
)

func main() {
	os.Exit(app.Run("ucprobe", setup))
}

func setup(_ context.Context, d app.Deps) (func() error, error) {
	mux := http.NewServeMux()
	mux.Handle("GET /health", d.Health.Handler())

	if d.Config.NodeToken != "" {
		apiAddr := "http://ucapi:8080"
		if v := os.Getenv("UC_API_ADDR"); v != "" {
			apiAddr = v
		}
		nodeID := "ucprobe-" + d.Config.HTTPAddr
		if v := os.Getenv("UC_NODE_ID"); v != "" {
			nodeID = v
		}
		region := "default"
		if v := os.Getenv("UC_NODE_REGION"); v != "" {
			region = v
		}
		go runProbeLoop(apiAddr, nodeID, region, d.Config.NodeToken, d.Logger)
	}

	return app.ServeHTTP(d.Config.HTTPAddr, mux, d)
}

func runProbeLoop(apiAddr, nodeID, region, token string, log *slog.Logger) {
	client := probev1connect.NewProbeServiceClient(
		http.DefaultClient, apiAddr,
		connect.WithInterceptors(&nodeAuth{token: token}),
	)
	exec := executor.New()

	log.Info("ucprobe started", "api", apiAddr, "node", nodeID, "region", region)

	for {
		ctx, cancel := context.WithTimeout(context.Background(), batchBudget)
		_, next, err := probeCycle(ctx, client, exec, nodeID, region, log)
		cancel()
		if err != nil {
			log.Warn("lease error", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		time.Sleep(next)
	}
}

// probeCycle runs one lease-execute-submit cycle on the batch-budget
// context: lease a batch, run it through the worker pool, submit what was
// collected on the separate submit budget. It returns how many checks were
// leased and how long the caller should wait before the next lease. The
// shape is the sequential loop's; only the execution underneath changed.
func probeCycle(
	ctx context.Context,
	client probev1connect.ProbeServiceClient,
	exec *executor.Executor,
	nodeID, region string,
	log *slog.Logger,
) (int, time.Duration, error) {
	leaseResp, err := client.Lease(ctx, connect.NewRequest(&probev1.LeaseRequest{
		NodeId: nodeID, Region: region, Capacity: leaseCapacity, Version: "1",
	}))
	if err != nil {
		return 0, 0, err
	}
	checks := leaseResp.Msg.Checks
	nextLease := time.Duration(leaseResp.Msg.NextLeaseAfterMs) * time.Millisecond
	if nextLease <= 0 {
		nextLease = 30 * time.Second
	}
	if len(checks) == 0 {
		return 0, nextLease, nil
	}

	results := executeChecks(ctx, exec, checks)

	// Submit on its own context, detached from the lease/execute context:
	// the collected results must reach the server even when the batch
	// budget is spent. 20 s is longer than any single check and short
	// enough that the next lease is not delayed by a stuck submit.
	if len(results) > 0 {
		submitCtx, submitCancel := context.WithTimeout(context.Background(), submitBudget)
		_, err = client.SubmitResults(submitCtx, connect.NewRequest(&probev1.SubmitResultsRequest{
			NodeId: nodeID, Region: region, Results: results,
		}))
		submitCancel()
		if err != nil {
			log.Warn("submit error", "err", err, "results", len(results))
		}
	}
	return len(checks), nextLease, nil
}

// executeChecks runs a leased batch with at most fetchConcurrency fetches in
// flight. The batch context governs the cycle, no fetch starts with less than
// one check timeout of budget left, and a failing or hanging host neither
// cancels its siblings nor loses the batch: Execute turns every error into a
// result. Each fetch writes only its own slot, so no lock is needed.
func executeChecks(ctx context.Context, exec *executor.Executor, checks []*probev1.CheckSpec) []*probev1.CheckResult {
	slots := make([]*probev1.CheckResult, len(checks))
	sem := make(chan struct{}, fetchConcurrency)
	var wg sync.WaitGroup
	for i, spec := range checks {
		sem <- struct{}{}
		// Budget guard, taken when a slot frees: with less than one check
		// timeout left of the batch context, start nothing more and let the
		// caller submit what was collected. A check started without the time
		// to finish is a check whose result dies with the context.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < checkTimeout {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			slots[i] = runCheck(ctx, exec, spec)
		})
	}
	wg.Wait()
	return slices.DeleteFunc(slots, func(r *probev1.CheckResult) bool { return r == nil })
}

// runCheck fetches one spec and maps the executor result onto the wire; the
// mapping (mapErrClass, the timings, TargetId and RetryAfterSec copied
// through) is the sequential loop's, unchanged.
func runCheck(ctx context.Context, exec *executor.Executor, spec *probev1.CheckSpec) *probev1.CheckResult {
	r := exec.Execute(ctx, executor.CheckSpec{
		URL: spec.Url, Method: spec.Method, Keyword: spec.Keyword,
		TimeoutMs: spec.TimeoutMs, MaxRedirects: spec.MaxRedirects,
		MaxBodyBytes: spec.MaxBodyBytes, CollectExpiry: spec.CollectExpiry,
	})
	rc := &probev1.CheckResult{
		CheckId: spec.CheckId, TargetId: spec.TargetId,
		Ok: r.OK, StatusCode: uint32(r.StatusCode),
		ErrorClass:  mapErrClass(r.ErrorClass),
		ErrorDetail: r.ErrorDetail,
		DnsMs:       r.DNSMs, ConnectMs: r.ConnectMs, TlsMs: r.TLSMs,
		TtfbMs: r.TTFBMs, TotalMs: r.TotalMs, BodyHash: r.BodyHash,
		RetryAfterSec: r.RetryAfterSec,
	}
	if !r.SSLExpiresAt.IsZero() {
		rc.SslExpiresAt = timestamppb.New(r.SSLExpiresAt)
	}
	return rc
}

type nodeAuth struct{ token string }

func (a *nodeAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+a.token)
		return next(ctx, req)
	}
}

func (a *nodeAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+a.token)
		return conn
	}
}

func (a *nodeAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func mapErrClass(s string) probev1.ErrorClass {
	switch s {
	case "dns":
		return probev1.ErrorClass_ERROR_CLASS_DNS
	case "connect":
		return probev1.ErrorClass_ERROR_CLASS_CONNECT
	case "tls":
		return probev1.ErrorClass_ERROR_CLASS_TLS
	case "timeout":
		return probev1.ErrorClass_ERROR_CLASS_TIMEOUT
	case "status":
		return probev1.ErrorClass_ERROR_CLASS_STATUS
	case "keyword_missing":
		return probev1.ErrorClass_ERROR_CLASS_KEYWORD_MISSING
	case "blocked_target":
		return probev1.ErrorClass_ERROR_CLASS_BLOCKED_TARGET
	case "challenge":
		return probev1.ErrorClass_ERROR_CLASS_CHALLENGE
	case "none":
		return probev1.ErrorClass_ERROR_CLASS_NONE
	default:
		return probev1.ErrorClass_ERROR_CLASS_UNSPECIFIED
	}
}
