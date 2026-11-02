#!/usr/bin/env bash
# 先编译全部集成测试二进制，再逐包串行运行。
# 2 核 runner 上 go test 会边编译边跑：fixture 的启动、清理超时被并行编译吃掉，
# 包级 10m 超时也算进了排队时间。拆开后，测试计时只含测试本身。
set -euo pipefail

# 不放在 bin/：CI 缓存 bin，测试二进制不该进缓存。
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

mapfile -t packages < <(go list -tags=integration \
	-f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}} {{.Dir}}{{end}}' "$@")

start=$SECONDS
for entry in "${packages[@]}"; do
	pkg=${entry%% *}
	go test -c -tags=integration -o "$out/${pkg//\//_}.test" "$pkg"
done
echo "编译 ${#packages[@]} 个测试包用时 $((SECONDS - start))s"

failed=()
for entry in "${packages[@]}"; do
	pkg=${entry%% *}
	start=$SECONDS
	# 测试以包目录为工作目录（CRD 等相对路径）。
	if (cd "${entry#* }" && "$out/${pkg//\//_}.test" -test.count=1 -test.timeout=30m); then
		echo "ok   $pkg $((SECONDS - start))s"
	else
		echo "FAIL $pkg $((SECONDS - start))s"
		failed+=("$pkg")
	fi
done

if ((${#failed[@]})); then
	printf 'FAIL %s\n' "${failed[@]}"
	exit 1
fi
