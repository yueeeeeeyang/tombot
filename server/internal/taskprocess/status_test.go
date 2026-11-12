package taskprocess

import (
	"strings"
	"testing"
)

const safeStatus = "Name:\ttask\nUid:\t10001 10001 10001 10001\nGid:\t10001 10001 10001 10001\nGroups:\nCapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapAmb:\t0000000000000000\nNoNewPrivs:\t1\nSeccomp:\t2\n"

func TestPrivilegeVerificationRejectsResidualAuthority(t *testing.T) {
	status, err := parseStatus([]byte(safeStatus))
	if err != nil || !status.unprivileged(10001, 10001) {
		t.Fatalf("合法非特权状态被拒绝：%v", err)
	}
	for _, change := range [][2]string{{"Uid:\t10001 10001 10001 10001", "Uid:\t10001 10001 0 10001"}, {"Gid:\t10001 10001 10001 10001", "Gid:\t10001 0 10001 10001"}, {"Groups:\n", "Groups:\t0\n"}, {"CapPrm:\t0000000000000000", "CapPrm:\t0000000000000001"}, {"CapEff:\t0000000000000000", "CapEff:\t0000000000000001"}, {"CapInh:\t0000000000000000", "CapInh:\t0000000000000001"}, {"CapAmb:\t0000000000000000", "CapAmb:\t0000000000000001"}, {"NoNewPrivs:\t1", "NoNewPrivs:\t0"}, {"Seccomp:\t2", "Seccomp:\t0"}} {
		status, err := parseStatus([]byte(strings.Replace(safeStatus, change[0], change[1], 1)))
		if err == nil && status.unprivileged(10001, 10001) {
			t.Fatalf("残留权限被接受：%s", change[0])
		}
	}
	for _, value := range []string{"", safeStatus + "Uid:\t0 0 0 0\n", strings.Replace(safeStatus, "Seccomp:\t2\n", "", 1), strings.Replace(safeStatus, "Seccomp:\t2", "Seccomp:\ttwo", 1), strings.Replace(safeStatus, "CapPrm:\t0000000000000000", "CapPrm:\t0 1", 1), strings.Replace(safeStatus, "Uid:\t10001 10001 10001 10001", "Uid:\t0", 1), strings.Repeat("x", 64<<10+1)} {
		if _, err := parseStatus([]byte(value)); err == nil {
			t.Fatal("损坏隔离状态被接受")
		}
	}
}
