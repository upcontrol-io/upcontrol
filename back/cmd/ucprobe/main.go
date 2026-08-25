// Command ucprobe is the regional probe node: no DB secrets (depguard
// enforced); it polls ucapi for checks and runs them through the SSRF guard.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	probev1 "go.upcontrol.io/back/gen/rpc/probe/v1"
	probev1connect "go.upcontrol.io/back/gen/rpc/probe/v1/probev1connect"
	"go.upcontrol.io/back/internal/platform/app"
	"go.upcontrol.io/back/internal/probe/executor"
)

func main() {
	os.Exit(app.Run("ucprobe", setup))
}

func setup(_ context.Context, d app.Deps) (func() error, error) {
	mux := http.NewServeMux()
	mux.Handle("GET /health", d.Health.Handler())

	if d.Config.NodeToken != "" {
		apiAddr := getenv("UC_API_ADDR", "http://ucapi:8080")
		nodeID := getenv("UC_NODE_ID", "ucprobe-"+d.Config.HTTPAddr)
		region := getenv("UC_NODE_REGION", "default")
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
		ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)

		leaseResp, err := client.Lease(ctx, connect.NewRequest(&probev1.LeaseRequest{
			NodeId: nodeID, Region: region, Capacity: 50, Version: "1",
		}))
		if err != nil {
			log.Warn("lease error", "err", err)
			cancel()
			time.Sleep(5 * time.Second)
			continue
		}

		checks := leaseResp.Msg.Checks
		nextLease := time.Duration(leaseResp.Msg.NextLeaseAfterMs) * time.Millisecond
		if nextLease <= 0 {
			nextLease = 30 * time.Second
		}

		if len(checks) == 0 {
			cancel()
			time.Sleep(nextLease)
			continue
		}

		results := make([]*probev1.CheckResult, 0, len(checks))
		for _, spec := range checks {
			r := exec.Execute(ctx, executor.CheckSpec{
				URL: spec.Url, Method: spec.Method, Keyword: spec.Keyword,
				TimeoutMs: spec.TimeoutMs, MaxRedirects: spec.MaxRedirects,
				MaxBodyBytes: spec.MaxBodyBytes, CollectExpiry: spec.CollectExpiry,
			})
			rc := &probev1.CheckResult{
				CheckId: spec.CheckId, MonitorId: spec.MonitorId,
				Ok: r.OK, StatusCode: uint32(r.StatusCode),
				ErrorClass:  mapErrClass(r.ErrorClass),
				ErrorDetail: r.ErrorDetail,
				DnsMs:       r.DNSMs, ConnectMs: r.ConnectMs, TlsMs: r.TLSMs,
				TtfbMs: r.TTFBMs, TotalMs: r.TotalMs, BodyHash: r.BodyHash,
			}
			if !r.SSLExpiresAt.IsZero() {
				rc.SslExpiresAt = timestamppb.New(r.SSLExpiresAt)
			}
			results = append(results, rc)
		}

		_, err = client.SubmitResults(ctx, connect.NewRequest(&probev1.SubmitResultsRequest{
			NodeId: nodeID, Region: region, Results: results,
		}))
		if err != nil {
			log.Warn("submit error", "err", err, "results", len(results))
		}

		cancel()
		time.Sleep(nextLease)
	}
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
	case "none":
		return probev1.ErrorClass_ERROR_CLASS_NONE
	default:
		return probev1.ErrorClass_ERROR_CLASS_UNSPECIFIED
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
