#!/usr/bin/env bash
# A-W2 压测基线：带计量的接入服务对比不带计量的 GOST 服务（本机回环，全部组件同进程）。
#
# 用法：internal/agent/scripts/bench.sh [输出文件]
#   COUNT=6 BENCHTIME=1s 可通过环境变量调整；结果写入输出文件（默认 bench-<日期>.txt）。
#   装有 benchstat（go install golang.org/x/perf/cmd/benchstat@latest）时会顺带打印汇总。
#
# 结果解读见 docs/04-计量与执行方案.md §8。
set -euo pipefail

cd "$(dirname "$0")/../../.."
out="${1:-bench-$(date +%Y%m%d-%H%M%S).txt}"
count="${COUNT:-6}"
benchtime="${BENCHTIME:-1s}"

{
	echo "# date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "# commit: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
	echo "# go: $(go version)"
	echo "# uname: $(uname -srm)"
	if [ "$(uname -s)" = "Darwin" ]; then
		echo "# cpu: $(sysctl -n machdep.cpu.brand_string), $(sysctl -n hw.ncpu) cores, $(($(sysctl -n hw.memsize) / 1073741824)) GiB"
	elif command -v lscpu >/dev/null; then
		echo "# cpu: $(lscpu | sed -n 's/^Model name:[[:space:]]*//p'), $(nproc) cores"
	fi
} | tee "$out"

go test -run '^$' -bench 'Proxy(Download|Upload|RTT|Parallel)|ConnBySrc' -benchmem -count "$count" -benchtime "$benchtime" ./internal/agent/ | tee -a "$out"
# 建连基准每次操作留下 TIME_WAIT 端口，固定次数、只跑一轮，避免耗尽临时端口。
go test -run '^$' -bench 'ProxyConnect' -benchmem -count 1 -benchtime "${CONNECT_N:-1000}x" ./internal/agent/ | grep '^Benchmark' | tee -a "$out"

if command -v benchstat >/dev/null; then
	benchstat "$out"
fi
echo "wrote $out"
