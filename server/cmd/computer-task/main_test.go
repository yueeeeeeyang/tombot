package main

import "testing"

func TestTaskRejectsExecutionOverrides(t *testing.T) {
	for _, arguments := range [][]string{{"--unknown"}, {"--browser", "--no-sandbox"}, {"--browser", "--browser"}, {"/bin/sh"}, {"--profile=/workspace"}} {
		if err := run(arguments); err == nil {
			t.Fatal("接受外部执行配置")
		}
	}
}
