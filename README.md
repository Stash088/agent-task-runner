# agent-task-runner

MCP-сервис, который выполняет задачи Hermes в **отдельном Kubernetes Job** и гасит под после
завершения. Родительский агент (gateway) при этом не держит kubeconfig и не может выполнить
`terminal`-команду в кластере: единственный путь к задаче — вызов MCP-инструмента.

```
Hermes gateway (ns hermes)            agent-task-runner (ns agent-tasks)          worker Job
  mcp_servers.agent-tasks  ──HTTP──▶  /mcp  ──create/wait──▶  batch/v1 Job  ──▶  hermes -z "<prompt>"
  (никаких k8s-прав)                  Role: jobs,pods,pods/log                   свой HERMES_HOME (emptyDir)
                                      (только ns agent-tasks)                     только LLM-ключ, без SA-токена
```

Полная спецификация и решения — `docs/plans/2026-11-07-agent-task-runner.md`.

## Инструменты MCP

| Инструмент | Назначение |
|---|---|
| `task_run` | создать Job: `prompt` (обязателен), `model`, `provider`, `toolsets`, `timeout_seconds`, `cpu`, `memory` |
| `task_status` | `pending` / `running` / `succeeded` / `failed` / `unknown` + exit code |
| `task_result` | лог контейнера `hermes` (ограничен `MAX_LOG_BYTES`) + exit code |
| `task_cancel` | удалить Job (Background propagation) |
| `task_list` | список задач с превью промпта |

Ни `image`, ни `command`, ни `labels` через API не передаются: промпт попадает в под **только**
через env `TASK_PROMPT`, модель/провайдер/тулсеты сверяются с allowlist из конфига.

## Требования к образу воркера (проверено на живом кластере)

Образ — `nousresearch/hermes-agent`, тот же digest, что у гейтвея. Job устроен так:

- под запускается как uid/gid `10000`, `runAsNonRoot: true`;
- init-контейнер (root) делает `chown 10000:10000 /data/home && chmod 0755 /data/home` —
  без этого `hermes` падает с `unsafe_control_path` (каталог принадлежит root) или
  `unsafe_control_permissions` (emptyDir монтируется как 0777);
- `HERMES_HOME=/data/home` (emptyDir, stateless), ключ провайдера — из Secret через env
  (`OPENCODE_GO_API_KEY`), `auth.json` создаётся сам;
- команда: `exec hermes -z "$TASK_PROMPT" -m "$TASK_MODEL" --provider "$TASK_PROVIDER"
  -t "$TASK_TOOLSETS" --ignore-user-config --yolo`;
- `automountServiceAccountToken: false` — у воркера нет прав в кластере.

**Обновлять `WORKER_IMAGE` в `deploy/30-configmap.yaml` вместе с образом гейтвея** и прогонять
`scripts/e2e.sh`: расхождение версий Hermes между гейтвеем и воркером — единственный реальный
источник регрессов.

## Деплой

```bash
# 1. Секрет: ключ доступа к MCP (генерируется) + ключ LLM-провайдера
kubectl -n agent-tasks create secret generic agent-task-llm \
  --from-literal=API_KEY="$(python3 -c 'import secrets;print(secrets.token_hex(24))')" \
  --from-literal=OPENCODE_GO_API_KEY="<opencode-go key>"

# 2. Манифесты (ns, RBAC, ConfigMap, Deployment, Service, NetworkPolicy)
kubectl apply -k deploy/

# 3. Проверка
kubectl -n agent-tasks rollout status deploy/agent-task-runner
kubectl -n agent-tasks port-forward svc/agent-task-runner 18080:8080 &
curl -s localhost:18080/readyz   # 200 = RBAC и доступ к API в порядке
```

`deploy/20-secret.example.yaml` в kustomization не входит — чтобы плейсхолдеры не применились
случайно.

Сборка образа: `make image` (kaniko в кластере) либо любой CI с kaniko
(kaniko → `<your-registry>/agent-task-runner:$CI_COMMIT_SHORT_SHA`).

**Питфолл (2026-11-07):** `gcr.io` доступен не со всех нод кластера — джоба `build` падала на
`ErrImagePull` (`dial tcp gcr.io:443: i/o timeout`), когда под приезжал на <node>. Kaniko
завендорен в локальный реестр:
`<your-registry>/infra/kaniko-executor:v1.23.2-debug` (тот же образ, скопирован
`k3s ctr images pull --all-platforms` + `push`). Не возвращать `gcr.io/...` в CI.

## Подключение к Hermes

В `config.yaml` профиля Hermes:

```yaml
mcp_servers:
  agent-tasks:
    url: http://agent-task-runner.agent-tasks.svc.cluster.local:8080/mcp
    headers:
      X-API-Key: <API_KEY из секрета agent-task-llm>
    enabled: true
    timeout: 120
```

После правки — рестарт гейтвея (`kubectl -n <gateway-ns> rollout restart deploy/<gateway>`), затем
проверка: `hermes mcp list` / `hermes mcp test agent-tasks` и вызов `task_status` из чата.

## Проверка end-to-end

```bash
export API_KEY=<из секрета> OPENCODE_GO_API_KEY=<ключ провайдера>
./scripts/e2e.sh
```

Скрипт применяет манифесты, поднимает port-forward, проходит `initialize` → `tools/list` →
`task_run` («Reply with exactly: E2E-OK») → поллинг `task_status` → `task_result` (проверяет
строку `E2E-OK`) → `task_cancel` (проверяет, что Job исчез). Проверено на k3s: проход
занимает ~1.5–2 минуты, из них ~20 с — старт пода.

## Ограничения и известные грабли

- **Логи, не артефакты.** Результат = stdout/err контейнера. Файлы, созданные воркером, живут в
  его emptyDir и исчезают вместе с подом. Нужны артефакты — добавлять PVC, это отдельная задача.
- **TTL 1 час** (`TTL_SECONDS`). После удаления Job `task_result` вернёт `state=unknown`,
  `message="result expired"`.
- **Лимит параллельности — процессный мьютекс** (`MAX_CONCURRENT_JOBS`, по умолчанию 3). При
  `replicas > 1` глобальный лимит не гарантируется; держите 1 реплику.
- **NetworkPolicy и ClusterIP.** kube-router применяет egress-правила после DNAT ClusterIP, поэтому
  разрешение только адреса API-сервиса на 443 оставляет `/readyz` в вечном 503. В манифесте разрешены
  `<SERVICE_CIDR>`, `<POD_CIDR>` на 443 и IP нод на 6443 — не сокращать «для красоты».
- **Воркер виден из ns `hermes` и не виден снаружи.** Ingress к сервису разрешён только из
  namespace `hermes`; проверено: из `default` соединение блокируется.
- **Предупреждение PID 1.** `hermes` пишет `this process is PID 1 with no init above it` —
  следствие переопределения entrypoint. Для одноразового пода безопасно: дерево процессов одно,
  hermes сам пожинает своих детей. Лечится только запуском через entrypoint образа.
- **Воркеру нужен egress в интернет** (LLM + web-tools). Поэтому на воркер-поды правила egress не
  навешаны — изоляция идёт по линии «нет прав в кластере + нет kubeconfig + нет SA-токена».
