package rpc

import (
	"testing"

	probev1 "go.upcontrol.io/back/gen/rpc/probe/v1"
)

func TestErrClassStr_RoundTrip(t *testing.T) {
	// errClassStr mirrors ucprobe's mapErrClass in reverse: every enum the
	// probe sends must map back, or the result is stored with an empty class.
	for _, tc := range []struct {
		enum probev1.ErrorClass
		want string
	}{
		{probev1.ErrorClass_ERROR_CLASS_DNS, "dns"},
		{probev1.ErrorClass_ERROR_CLASS_CONNECT, "connect"},
		{probev1.ErrorClass_ERROR_CLASS_TLS, "tls"},
		{probev1.ErrorClass_ERROR_CLASS_TIMEOUT, "timeout"},
		{probev1.ErrorClass_ERROR_CLASS_STATUS, "status"},
		{probev1.ErrorClass_ERROR_CLASS_KEYWORD_MISSING, "keyword_missing"},
		{probev1.ErrorClass_ERROR_CLASS_BLOCKED_TARGET, "blocked_target"},
		{probev1.ErrorClass_ERROR_CLASS_NONE, "none"},
		{probev1.ErrorClass_ERROR_CLASS_UNSPECIFIED, ""},
	} {
		if got := errClassStr(tc.enum); got != tc.want {
			t.Fatalf("errClassStr(%v) = %q, want %q", tc.enum, got, tc.want)
		}
	}
}

func TestErrClassStr_BlockedTargetSurfaces(t *testing.T) {
	// A blocked check must store "blocked_target", not ""; that is what
	// makes it distinguishable from a real outage downstream.
	if got := errClassStr(probev1.ErrorClass_ERROR_CLASS_BLOCKED_TARGET); got != "blocked_target" {
		t.Fatalf("blocked_target must surface verbatim, got %q", got)
	}
}
