package dockerbootstrap

// SocketPath 与启动引导实际查询同一引擎，部署组合不允许节点改用另一 Docker 守护进程。
const SocketPath = "/run/docker.sock"

// StateDirectory 和 RuntimeDirectory 由 root 引导使用，不能与用户磁盘或其他控制账本重叠。
const StateDirectory = "/var/lib/tombot-node/bootstrap"
const RuntimeDirectory = "/run/tombot-node"
