#!/usr/bin/env bash
# 先编译全部集成测试二进制，再逐包串行运行。
# 2 核 runner 上 go test 会边编译边跑：fixture 的启动、清理超时被并行编译吃掉，
# 包级 10m 超时也算进了排队时间。拆开后，测试计时只含测试本身。
#
# INTEGRATION_SERVICES 列出的后端（"apiserver"、"postgres"、"openbao"）在 go test 之外启动一次：编译期间
# 在后台拉起，就绪后测试从 AYATORI_IT_<SERVICE> 找到它们（apiserver 为 kubeconfig 路径，其余为
# 容器 ID），并自行隔离或复位。测试串行运行，每个场景独占这些实例；脚本退出时全部清除。
set -euo pipefail

# 不放在 bin/：CI 缓存 bin，测试二进制不该进缓存。
out=$(mktemp -d)
cleanup() {
	for file in "$out"/*.id; do
		if [[ -s $file ]]; then docker rm -f "$(<"$file")" >/dev/null 2>&1 || true; fi
	done
	if [[ -n ${apiserver-} ]]; then
		kill "$apiserver" 2>/dev/null || true
		wait "$apiserver" 2>/dev/null || true
	fi
	rm -rf "$out"
}
trap cleanup EXIT

# 数据只活一个测试会话：放 tmpfs 并关掉落盘，initdb 与每次复位都不等磁盘。
start_postgres() {
	docker run -d --rm --tmpfs /var/lib/postgresql/data -p 127.0.0.1::5432 \
		-e POSTGRES_PASSWORD="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')" \
		--health-cmd 'pg_isready -h 127.0.0.1 -U postgres' --health-interval 1s \
		postgres@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73 \
		-c fsync=off -c synchronous_commit=off -c full_page_writes=off
}

# 无持久卷的 dev server。root token 与测试中的 fixtureToken 相同，只用于测试。
start_openbao() {
	docker run -d --rm -p 127.0.0.1::8200 -e BAO_ADDR=http://127.0.0.1:8200 \
		--health-cmd 'bao status' --health-interval 1s \
		openbao/openbao@sha256:5b2486ab0fb90bbc788cc345b0a08616dfb375873ee8be5df3a2fd4d378a67e0 \
		server -dev -dev-root-token-id=AYATORI-TEST-ONLY-bao-token -dev-listen-address=0.0.0.0:8200
}

read -ra services <<<"${INTEGRATION_SERVICES-}"
starters=()
for service in "${services[@]}"; do
	if [[ $service == apiserver ]]; then
		# envtest 的 etcd 与 kube-apiserver；启动超时 5m，与下面的编译并行。
		go build -o "$out/apiserver" ./hack/apiserver
		"$out/apiserver" -write-kubeconfig "$out/kubeconfig" &
		apiserver=$!
		continue
	fi
	"start_$service" >"$out/$service.id" &
	starters+=($!)
done

mapfile -t packages < <(go list -tags=integration \
	-f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}} {{.Dir}}{{end}}' "$@")

start=$SECONDS
for entry in "${packages[@]}"; do
	pkg=${entry%% *}
	go test -c -tags=integration -o "$out/${pkg//\//_}.test" "$pkg"
done
echo "编译 ${#packages[@]} 个测试包用时 $((SECONDS - start))s"

for pid in "${starters[@]}"; do
	wait "$pid"
done
start=$SECONDS
for service in "${services[@]}"; do
	if [[ $service == apiserver ]]; then
		until [[ -s $out/kubeconfig ]]; do
			kill -0 "$apiserver" || { echo "apiserver 启动失败" >&2; exit 1; }
			sleep 1
		done
		export AYATORI_IT_APISERVER=$out/kubeconfig
		continue
	fi
	id=$(<"$out/$service.id")
	# 冷缓存时镜像拉取也在这里等待；WAN 不稳，给足时间。
	until [[ $(docker inspect -f '{{.State.Health.Status}}' "$id") == healthy ]]; do
		if ((SECONDS - start > 300)); then
			echo "$service 未在 300s 内就绪" >&2
			docker logs --tail 50 "$id" >&2
			exit 1
		fi
		sleep 1
	done
	export "AYATORI_IT_${service^^}=$id"
done
((${#services[@]})) && echo "后端 ${services[*]} 就绪用时 $((SECONDS - start))s"

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
