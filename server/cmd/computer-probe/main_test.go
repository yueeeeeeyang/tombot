package main

import (
	"io"
	"testing"
)

func TestProbeCommandRejectsExternalConfiguration(t *testing.T) {
	for _, arguments := range [][]string{{"/bin/sh"}, {"--worker", "{}"}, {"--worker"}, {"--worker", "null"}, {"--worker", "{}", "extra"}} {
		if err := run(t.Context(), arguments, io.Discard); err == nil {
			t.Fatal("探针接受外部命令或无效授权")
		}
	}
}
