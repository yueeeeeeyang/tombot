package execution

import (
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/provider"
)

// ComputerBrowser 只注册容器内工具定义，刻意不持有节点运行器和服务端执行方法。
type ComputerBrowser struct{}

func (ComputerBrowser) Schema() provider.ToolSchema { return computertools.BrowserSchema() }
func (ComputerBrowser) UsesComputer() bool          { return true }
func (ComputerBrowser) RequiresVision() bool        { return true }
func (ComputerBrowser) RunsInAgent() bool           { return true }

var _ LocalTool = ComputerBrowser{}
