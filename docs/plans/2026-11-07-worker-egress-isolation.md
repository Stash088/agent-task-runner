# Plan: close the worker egress gap and add PSA labels

## Контекст (факты, проверены)

- Воркер-поды (Job) имеют метку `app.kubernetes.io/name=agent-task-worker` и `atr.io/task-id=<id>`.
- Существующая NetworkPolicy `agent-task-runner` селектит только поды сервиса
  (`app.kubernetes.io/name=agent-task-runner`) → воркеры сейчас **не ограничены**: из песочницы
  доступны `<POD_CIDR>`, `<SERVICE_CIDR>`, API-сервер на 6443 и метаданные облака.
- В JobSpec уже есть: `runAsUser: 10000` для воркера, `runAsNonRoot: false` у init-контейнера
  (он делает `chown` от root), `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`,
  `automountServiceAccountToken: false`.
- Namespace `agent-tasks` не имеет PSA-лейблов.

## Задача 1 — `deploy/65-worker-networkpolicy.yaml` (новый файл)

NetworkPolicy:
- `metadata.name: agent-task-worker`, `namespace: agent-tasks`
- `podSelector.matchLabels: {app.kubernetes.io/name: agent-task-worker}`
- `policyTypes: [Ingress, Egress]`
- `ingress: []` — воркер не принимает входящих соединений вообще.
- `egress`:
  1. DNS: `to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kube-system}},
     podSelector: {matchLabels: {k8s-app: kube-dns}}}]`, `ports: [UDP/53, TCP/53]`.
  2. Внешний HTTPS/HTTP: `to: [{ipBlock: {cidr: 0.0.0.0/0, except: [<POD_CIDR>, <SERVICE_CIDR>,
     169.254.169.254/32, <API_NODE_1_IP>/32, <API_NODE_2_IP>/32, <API_NODE_3_IP>/32]}}]`,
     `ports: [TCP/443, TCP/80]`.
- Комментарии в файле: почему `except` обязателен (кластерные диапазоны + metadata endpoint),
  почему 80 разрешён (часть провайдеров редиректит).

## Задача 2 — `deploy/00-namespace.yaml`

Добавить лейблы:
```yaml
pod-security.kubernetes.io/enforce: baseline
pod-security.kubernetes.io/audit: restricted
pod-security.kubernetes.io/warn: restricted
```
Комментарий: `enforce=restricted` сломал бы init-контейнер (`chown` от root), поэтому enforce=baseline,
а restricted идёт как warn/audit — регрессии увидим в событиях, но Job не упадёт.

## Критерии приёмки

- `kubectl kustomize deploy/` рендерит 9 объектов (было 8), без ошибок.
- Оба файла — валидный YAML, без секретов и без изменения существующих правил.
- NetworkPolicy не трогает поды сервиса (у них свой селектор).
