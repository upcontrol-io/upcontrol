package normalize

import "testing"

// The two engines that read events rows back are silent when they break:
// LastDeployAt (post-deploy suppression) and ucadmin's install_verified
// funnel. These tests pin the names those engines depend on.
func TestClassifyNamed(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{"deploy", "deploy"},
		{"deploy.succeeded", "deploy.succeeded"},
		{"deployment_created", "deployment_created"},
		{"DEPLOY", "deploy"},
		{"install_verified", "install_verified"},
	} {
		got := Classify(c.in)
		if got.Name != c.want || got.Reserved {
			t.Errorf("Classify(%q) = %+v, want Name %q", c.in, got, c.want)
		}
	}
}

func TestClassifyOrdinary(t *testing.T) {
	// payment_failed and signup were dictionary names once; nothing queries
	// them now, so they are ordinary lines like any other message.
	for _, in := range []string{"", "payment_failed", "signup", "user 42 not found"} {
		if got := Classify(in); got.Name != "" || got.Reserved {
			t.Errorf("Classify(%q) = %+v, want an ordinary log line", in, got)
		}
	}
}

func TestClassifyReserved(t *testing.T) {
	for _, in := range []string{"uc.internal", "uc.deploy", "UC.Prefix"} {
		got := Classify(in)
		if !got.Reserved || got.Name != "" {
			t.Errorf("Classify(%q) = %+v, want reserved with no name", in, got)
		}
	}
}
