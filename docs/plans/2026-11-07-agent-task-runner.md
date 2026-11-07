# agent-task-runner — Implementation Plan

**Goal:** MCP-сервис на Go, который по запросу Hermes создаёт Kubernetes Job с образом `hermes-agent`, выполняет `hermes -z "<prompt>"` в изолированном поде, отдаёт результат (финальный ответ + exit code) и гасит под.

**Architecture:** Control-plane, а не terminal-backend. Родительский Hermes теряет kubeconfig и ходит только по MCP (`/mcp`, streamable HTTP, `X-API-Key`). Состояние живёт в Kubernetes (Job + labels), а не в памяти сервиса: рестарт сервиса ничего не теряет. Параметров, из которых можно собрать произвольный под (`image`, `command`, `labels`, `serviceAccount`), в API нет — только валидируемый allowlist.

**Tech stack:** Go 1.27, `github.com/modelcontextprotocol/go-sdk` v1.8.0, `k8s.io/client-go` v0.36.6 (k3s v1.36.3), stdlib `log/slog`, `flag`, `testing`. Без CGO, без кобры, без testify.

**Verified facts (спайк 2026-11-07, ns hermes, образ `nousresearch/hermes-agent@sha256:adb7890916de63245523492be8f68f9d2095fecbec4e2a6dab7960381f0fc002`):**

1. `hermes -z` в ПУСТОМ `HERMES_HOME` работает: `SPIKE-OK`, exit 0, ~50 с (из них ~20 с старт пода).
2. Падает с `unsafe_control_path`, если каталог принадлежит uid 0 → нужен `runAsUser: 10000`.
3. Падает с `unsafe_control_permissions` (mode 0777 у emptyDir) → init-контейнер делает `chmod 0755`.
4. Ключ провайдера из env (`OPENCODE_GO_API_KEY`) подхватывается, `auth.json` создаётся автоматически.
5. `-t <toolsets>` реально сужает набор инструментов; `--ignore-user-config` и `--yolo` нужны для неинтерактивного прогона.
6. `--usage-file <path>` пишет JSON с расходом токенов (задел на учёт).

---

## 1. Контракт MCP

Сервер отдаёт пять инструментов. Промпт и параметры попадают в под **только через env-переменные** и allowlist-валидацию — никакой конкатенации в shell.

### `task_run`

```json
{
  "prompt": "string, 1..262144 байт, задача для hermes -z",
  "model": "string, optional, должен матчить ALLOWED_MODELS (regex)",
  "provider": "string, optional, из ALLOWED_PROVIDERS",
  "toolsets": "string, optional, csv, каждое значение из ALLOWED_TOOLSETS",
  "timeout_seconds": "int, optional, clamp [60, MAX_TIMEOUT_SECONDS]",
  "cpu": "string, optional, clamp [100m, MAX_CPU]",
  "memory": "string, optional, clamp [256Mi, MAX_MEMORY]"
}
```

Ответ: `{"task_id":"atr-1f3c9a7b2d41","state":"pending","created_at":"2026-11-07T15:30:00Z","pod":"atr-1f3c9a7b2d41-x7k2p"}`

Ошибки: `busy` (превышен `MAX_CONCURRENT_JOBS`), `invalid_argument` (не прошло валидацию), `internal`.

### `task_status`

Вход `{"task_id":"..."}` → `{"task_id","state","start_time","completion_time","exit_code","message"}`.
`state` ∈ `pending | running | succeeded | failed | unknown`. `unknown` = Job не найден (истёк TTL или удалён).

### `task_result`

Вход `{"task_id":"...","max_bytes":0}` → `{"task_id","state","exit_code","log","truncated","log_bytes"}`.
`log` = stdout/stderr контейнера `hermes`, читается `pods/log` с `limitBytes = min(max_bytes || MAX_LOG_BYTES, MAX_LOG_BYTES)` и `timestamps=false`. Если под уже удалён → `state=unknown`, `log=""`, `message="result expired"`.

### `task_cancel`

Вход `{"task_id":"..."}` → `{"task_id","deleted":true}`. Удаляет Job с `propagationPolicy=Background`.

### `task_list`

Вход `{"limit":20,"state":"running|succeeded|failed|"}` → `{"tasks":[{"task_id","state","created_at","prompt_preview"}],"count":N}`.
`prompt_preview` берётся из аннотации Job (`atr.io/prompt-preview`, первые 120 символов, обрезается по руне).

---

## 2. Рабочий Job (проверен спайком, менять нельзя без повторного спайка)

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: atr-<12 hex>
  namespace: agent-tasks
  labels:
    app.kubernetes.io/name: agent-task-runner
    app.kubernetes.io/managed-by: agent-task-runner
    atr.io/task-id: atr-<12 hex>
  annotations:
    atr.io/prompt-preview: <первые 120 символов>
    atr.io/toolsets: <csv>
spec:
  backoffLimit: 0
  activeDeadlineSeconds: <timeout>
  ttlSecondsAfterFinished: 3600     # 1 час; task_result после этого → unknown
  template:
    metadata:
      labels:
        app.kubernetes.io/name: agent-task-worker
        atr.io/task-id: atr-<12 hex>
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext:
        runAsUser: 10000
        runAsGroup: 10000
        runAsNonRoot: true
        seccompProfile: { type: RuntimeDefault }
      initContainers:
        - name: chown-home
          image: busybox:1.36
          securityContext: { runAsUser: 0, runAsNonRoot: false, allowPrivilegeEscalation: false }
          command: ["sh", "-c", "mkdir -p /data/home && chown 10000:10000 /data/home && chmod 0755 /data/home"]
          volumeMounts: [{ name: home, mountPath: /data/home }]
      containers:
        - name: hermes
          image: <WORKER_IMAGE>          # digest-пин из конфига сервиса
          imagePullPolicy: IfNotPresent
          env:
            - { name: HERMES_HOME, value: /data/home }
            - { name: HOME,        value: /data/home }
            - { name: TASK_PROMPT, value: <prompt> }
            - { name: TASK_MODEL,  value: <model> }
            - { name: TASK_PROVIDER, value: <provider> }
            - { name: TASK_TOOLSETS, value: <csv> }
            - name: OPENCODE_GO_API_KEY
              valueFrom: { secretKeyRef: { name: agent-task-llm, key: OPENCODE_GO_API_KEY } }
          command: ["/bin/sh", "-c"]
          args:
            - exec hermes -z "$TASK_PROMPT" -m "$TASK_MODEL" --provider "$TASK_PROVIDER" -t "$TASK_TOOLSETS" --ignore-user-config --yolo
          volumeMounts: [{ name: home, mountPath: /data/home }]
          resources:
            requests: { cpu: <cpu>, memory: <mem> }
            limits:   { cpu: <cpu>, memory: <mem> }
      volumes:
        - name: home
          emptyDir: { sizeLimit: 2Gi }
```

Правила: `env`-переменные не интерпретируются shell'ом → инъекция через промпт невозможна. `$TASK_*` всегда в двойных кавычках. Если `model`/`provider`/`toolsets` пусты — сервис подставляет значения из конфига (дефолты), а не пустые строки.

---

## 3. Конфигурация сервиса

| Env | Дефолт | Смысл |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | адрес HTTP |
| `API_KEY` | — (обязателен) | ключ `X-API-Key` / `Authorization: Bearer` |
| `WORKER_NAMESPACE` | `agent-tasks` | ns для Job'ов |
| `WORKER_IMAGE` | — (обязателен) | digest-пин образа hermes-agent |
| `LLM_SECRET_NAME` | `agent-task-llm` | Secret с `OPENCODE_GO_API_KEY` |
| `MAX_CONCURRENT_JOBS` | `3` | потолок одновременных Job в ns |
| `DEFAULT_TIMEOUT_SECONDS` | `1800` | если не задан |
| `MAX_TIMEOUT_SECONDS` | `7200` | верхняя граница clamp |
| `DEFAULT_MODEL` | `deepseek-v4.1-flash` | |
| `DEFAULT_PROVIDER` | `opencode-go` | |
| `DEFAULT_TOOLSETS` | `web` | |
| `ALLOWED_PROVIDERS` | `opencode-go,zai` | csv |
| `ALLOWED_MODELS` | `^(deepseek-v4\.[0-9](-[a-z]+)?\|glm-5[.0-9]*(-flash)?\|qwen3\.[0-9](-flash)?)$` | regex |
| `ALLOWED_TOOLSETS` | `web,files,terminal,memory` | csv |
| `MAX_CPU` / `MAX_MEMORY` | `2` / `4Gi` | clamp |
| `MAX_LOG_BYTES` | `262144` | потолок чтения лога |
| `TTL_SECONDS` | `3600` | ttlSecondsAfterFinished |
| `SHUTDOWN_TIMEOUT_SECONDS` | `15` | graceful stop |

Секрет сервиса: тот же `API_KEY` (генерируется нами) + `WORKER_IMAGE` может жить в ConfigMap.

---

## 4. Layout репозитория

```
agent-task-runner/
├── cmd/agent-task-runner/main.go        # flag/env load, http.Server, graceful shutdown
├── internal/config/config.go            # Load() (env + defaults + validation), Config struct
├── internal/config/config_test.go
├── internal/runner/types.go             # TaskRunRequest/Response/Task, state enum
├── internal/runner/validate.go          # Validate(req, cfg) error — вся allowlist-логика
├── internal/runner/validate_test.go     # table-driven, включая инъекционные кейсы
├── internal/runner/jobspec.go           # BuildJob(taskID, req, cfg) *batchv1.Job
├── internal/runner/jobspec_test.go      # golden-тест YAML через sigs.k8s.io/yaml
├── internal/runner/service.go           # Run/Status/Result/Cancel/List на clientset.Interface
├── internal/runner/service_test.go      # kubernetes/fake + fake logs
├── internal/mcpserver/server.go         # mcp.NewServer + 5 AddTool
├── internal/mcpserver/server_test.go    # tools/list по схеме, вызов task_run на fake
├── internal/httpapi/auth.go             # middleware X-API-Key / Bearer, constant-time
├── internal/httpapi/auth_test.go
├── internal/httpapi/router.go           # /mcp, /healthz, /readyz
├── deploy/kustomization.yaml
├── deploy/00-namespace.yaml
├── deploy/10-rbac.yaml                  # SA + Role + RoleBinding (только ns agent-tasks)
├── deploy/20-secret.example.yaml        # placeholder, реальный ключ не коммитим
├── deploy/30-configmap.yaml
├── deploy/40-deployment.yaml            # distroless, non-root, RO rootfs, /tmp emptyDir
├── deploy/50-service.yaml               # ClusterIP 8080
├── deploy/60-networkpolicy.yaml         # ingress только из ns hermes; egress к API+LLM
├── .gitlab-ci.yml                       # stage test (go vet/test) + manual build (kaniko → <registry>)
├── Dockerfile                           # golang:1.27-alpine → distroless/static:nonroot
├── Makefile                             # build, test, vet, lint(no-op), image, e2e
├── scripts/e2e.sh                       # kubectl port-forward + curl MCP initialize/tools/call
└── README.md                            # runbook: деплой, подключение к Hermes, откат
```

---

## 5. Задачи (порядок выполнения, каждая — коммит)

**T1. Скелет + конфиг.** `go mod init github.com/Stash088/agent-task-runner`; зависимости `go-sdk@v1.8.0`, `client-go@v0.36.6`, `sigs.k8s.io/yaml`; `internal/config` с Load() и тестами (обязательность `API_KEY`/`WORKER_IMAGE`, парсинг csv, клампы); `cmd/main.go` поднимает `/healthz` → 200 `ok`, graceful shutdown. Критерий: `go vet ./... && go test ./...` зелёные, `curl :8080/healthz` → 200.

**T2. Валидация.** `internal/runner/validate.go`: длина промпта, regex модели, membership провайдера/тулсета, клампы timeout/cpu/memory (парсинг `resource.MustParse`). Тесты: 20+ table-кейсов, включая `prompt` с `"; rm -rf /`, `$(id)`, backtick, переносом строки, NUL-байтом (NUL → reject). Критерий: тесты зелёные, NUL и слишком длинный промпт отвергаются.

**T3. JobSpec.** `BuildJob`: ровно как в разделе 2, с env-передачей промпта, labels `atr.io/task-id`, аннотацией preview (обрезка по рунам, не по байтам), `ttlSecondsAfterFinished`, `activeDeadlineSeconds`, `backoffLimit: 0`. Golden-тест: собрать Job, сериализовать `sigs.k8s.io/yaml`, сравнить с `testdata/job.golden.yaml`. Критерий: в собранном Job нет ни одной строки, содержащей промпт в `Args`/`Command`.

**T4. K8s-сервис.** `internal/runner/service.go` на `clientset.Interface` + `corev1client.PodExpansion` (интерфейс для логов, чтобы тестировать фейком): Run (create, 409 → retry с новым id), Status (Job conditions + pod phase + terminated exitCode), Result (stream log с limit), Cancel (Background delete), List (label selector, сортировка по created desc, prompt_preview из аннотации). Проверка `MAX_CONCURRENT_JOBS` через list с label `app.kubernetes.io/managed-by`. Критерий: тесты на `k8s.io/client-go/kubernetes/fake` зелёные, покрыты: create, status succeeded/failed/running/unknown, cancel, list limit, busy.

**T5. MCP-сервер.** `internal/mcpserver`: `mcp.NewServer(&mcp.Implementation{Name:"agent-task-runner", Version:version}, nil)`, пять `mcp.AddTool` со структурными схемами (json-теги + jsonschema-описания), обработчики возвращают `Output`-структуры, ошибки — `tool_error`-совместимый текст. Роутер: `mcp.NewStreamableHTTPHandler(func(*http.Request)*mcp.Server{return srv}, &mcp.StreamableHTTPOptions{Logger: slog, SessionTimeout: 30*time.Minute})` на `/mcp`. Критерий: `initialize` + `tools/list` отдают 5 инструментов с корректными именами; `tools/call task_run` на фейке создаёт Job и возвращает `task_id`.

**T6. Auth + деплой-манифесты.** `internal/httpapi/auth.go` (constant-time сравнение, 401 JSON-RPC-совместимый ответ, healthz/readyz без ключа), `deploy/*` — RBAC строго в ns `agent-tasks`: `jobs` (create/get/list/watch/delete), `pods` (get/list/watch), `pods/log` (get). Deployment: 1 реплика, `runAsUser: 10000`, `readOnlyRootFilesystem: true`, `drop: [ALL]`, `allowPrivilegeEscalation: false`, `automountServiceAccountToken: true` (нужен для API), probes `/healthz` `/readyz`. NetPol: ingress на 8080 только из ns `hermes`; egress — DNS + `<API_SERVICE_IP>:443`. Критерий: `kubectl kustomize deploy/` рендерится, `kubectl apply --dry-run=server -k deploy/` проходит.

**T7. Образ + CI + e2e.** `Dockerfile` (static, `CGO_ENABLED=0`, distroless nonroot) и `.gitlab-ci.yml` (stage test: `go vet`/`go test`; stage build: kaniko → `<registry>/agent-task-runner:<sha>`, manual). `scripts/e2e.sh`: деплой, port-forward, `initialize` → `tools/call task_run` с промптом «Reply with exactly: E2E-OK», поллинг `task_status` до `succeeded`, `task_result` содержит `E2E-OK`, `task_cancel` → Job исчез. Критерий: скрипт завершается 0 на живом кластере.

**T8. README + приёмка.** Runbook: как сгенерировать `API_KEY`, как положить `agent-task-llm`, как подключить к Hermes (`mcp_servers.agent-tasks` = url `http://agent-task-runner.agent-tasks.svc.cluster.local:8080/mcp` + header), как проверить, как откатить, известные ограничения (артефакты только в логе; TTL 1 час; worker egress открыт). Критерий: README повторяем с нуля, все ссылки на реальные файлы.

---

## 6. Non-goals (сознательно вне MVP)

- Артефакты на PVC (только лог; расширение — отдельная задача).
- Очередь и ретраи: при `MAX_CONCURRENT_JOBS` возвращается `busy`, клиент решает сам.
- Метрики Prometheus (есть `/healthz`, `/readyz`, структурные логи с `task_id`).
- Стриминг прогресса задачи (`hermes -z` печатает только финал).
- Мультипровайдерные секреты: один Secret, один ключ.
- Ограничение egress воркера beyond DNS: воркеру нужен интернет (LLM + web-tools).

## 7. Риски

| Риск | Митигация |
|---|---|
| Образ hermes-agent сменит поведение `-z` (флаги/проверки пути) | digest-пин в конфиге; e2e в CI ловит регресс |
| Разные версии Hermes в воркере и гейтвее расходятся по тулсетам | `WORKER_IMAGE` обновляем вместе с гейтвеем, e2e проверяет `-t web` |
| SQLite/состояние воркера теряется между задачами | by design: `HERMES_HOME` = emptyDir, задачи stateless |
| Промпт-инъекция пытается достучаться до k8s API | `automountServiceAccountToken: false`, у воркера нет прав и kubeconfig |
| Логи содержат чувствительные данные | `MAX_LOG_BYTES`, TTL 1 час, в README — предупреждение |

**Acceptance для всего сервиса:** `go vet ./... && go test ./... -race` зелёные; `GOOS=linux GOARCH=amd64 go build ./...` проходит; `kubectl apply --dry-run=server -k deploy/` проходит; e2e на живом кластере возвращает `E2E-OK`; в Job'е нет креденшелов кроме LLM-ключа; у SA сервиса нет прав вне ns `agent-tasks`.
