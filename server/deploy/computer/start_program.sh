#!/bin/sh
# 版本由节点在受保护的控制目录选定，直到该电脑排空并停止后才切换。
set -eu
IFS= read -r core < /run/tombot-control/program-core
case "$core" in ''|*[!0-9a-f]*) exit 125 ;; esac
[ "${#core}" -eq 64 ] || exit 125
exec "/usr/lib/tombot/programs/$core/${0##*/}" "$@"
