package sharedbrowser

import "testing"

func TestSandboxProbeRejectsPartialAndMisleadingReports(t *testing.T) {
	for _, report := range []string{"PID namespaces\tYes\nNetwork namespaces\tYes\nSeccomp-BPF sandbox\tYes", "PID namespaces Yes\nNetwork namespaces No\nSeccomp-BPF sandbox Yes", "PID namespaces Yes\nNetwork namespaces Yes", "Not PID namespaces Yes\nNetwork namespaces Yes\nSeccomp-BPF sandbox Yes"} {
		if sandboxEnabled(report) != (report == "PID namespaces\tYes\nNetwork namespaces\tYes\nSeccomp-BPF sandbox\tYes") {
			t.Fatal(report)
		}
	}
}
