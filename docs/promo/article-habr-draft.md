# Как я перестал гадать, какая пара нод сломалась после апдейта CNI, и написал для этого свой мониторинг

*Черновик для Хабра. TODO: заменить плейсхолдеры скриншотов, вычитать перед публикацией, добавить свою историю от первого лица в начало, если она отличается от описанной ниже.*

## Боль

Стандартная ситуация для любого, кто держит on-prem Kubernetes-кластер на десятки-сотни нод: обновили CNI (или ядро, или драйвер сетевой карты, или просто передвинули часть нод в новый сегмент) — и через какое-то время начинают приходить тикеты «у нас иногда таймаутит между подами», «DNS иногда не резолвится», «HTTP health-check то падает, то нет». Ключевое слово — «иногда». Проблема не глобальная, не 100% трафика, а деградация конкретных пар нод или конкретного протокола.

Стандартный набор инструментов в такой момент не очень помогает:

- `kubectl exec` + `ping`/`curl` руками — но какую пару нод проверять? Их сотни.
- Node exporter и стандартные дашборды показывают CPU/memory/disk конкретной ноды, но не «нода A не может достучаться до ноды B по UDP».
- Логи CNI-плагина — если повезёт, там будет что-то релевантное, но это постфактум и не по каждой паре.

Нужен был инструмент, который постоянно, во всех направлениях, прогоняет реальные пробы между нодами — и как только что-то падает, сразу показывает: какая пара, какой протокол, на каком хопе.

## Что уже есть: Goldpinger

Первое, что приходит в голову — [Goldpinger](https://github.com/bloomberg/goldpinger). Это зрелый, проверенный временем инструмент: DaemonSet, который опрашивает всех своих собратьев по HTTP, отдаёт метрики и рисует граф связности в собственном web UI. Он простой в развёртывании и решает базовый вопрос «видит ли нода X ноду Y». Для многих кластеров этого достаточно, и если у вас его ещё нет — начните с него, это низкий порог входа и мгновенная польза.

Но когда деградация не бинарная («видит/не видит»), а частичная и протокол-специфичная, его возможностей перестаёт хватать. Да, он умеет UDP-пробу (loss/hop-count/RTT), проверки внешних TCP/HTTP-таргетов и DNS-резолвинг — но основной меш между пирами у него HTTP, а per-peer ICMP и реактивного per-hop-трейсинга нет. kconmon-ng гоняет все пять протоколов как отдельные проверки между каждой парой пиров и при фейле автоматически запускает MTR — вот в этом разница.

Так появился **kconmon-ng** — Kubernetes Node Connectivity Monitor, next generation ([репозиторий](https://github.com/EsDmitrii/kconmon-ng), Apache 2.0).

## Что делает kconmon-ng иначе

Архитектурно это тоже пара agent/controller, но устроена немного по-другому, чем классический full-mesh HTTP-опрос:

- **Controller** (Deployment) — держит реестр агентов (heartbeat-eviction), следит за нодами кластера через Kubernetes informer, отдаёт топологию по gRPC-стриму и поддерживает leader election для HA (`controller.leaderElection: true`).
- **Agent** (DaemonSet) — получает от контроллера живой список пиров по gRPC (не поллинг — контроллер сам пушит full-sync при подключении и инкрементальные апдейты при изменении топологии) и гоняет между собой и всеми пирами пять типов проверок: **TCP, UDP, ICMP, DNS, HTTP**.

Каждый чекер — это не бинарный «жив/не жив», а отдельный набор метрик: TCP считает время коннекта и полный RTT отдельно, UDP меряет RTT/джиттер/packet loss на burst из нескольких пакетов, ICMP — RTT и loss по IPv4/IPv6, DNS резолвит список хостов через системный резолвер либо явно заданные upstream-сервера, HTTP снимает фазы DNS/connect/TLS/TTFB/total plus опциональную сверку тела ответа по regexp.

Главное отличие в диагностике: **когда TCP/UDP/ICMP проба падает, агент автоматически запускает MTR** для этой пары (source, destination) с cooldown, чтобы не заваливать сеть трейсами, и экспортирует RTT/loss по каждому хопу отдельной метрикой (`kconmon_ng_mtr_hop_rtt_seconds` с лейблами `hop_number`, `hop_ip`). То есть вместо «между node-5 и node-17 что-то не так» вы сразу видите, на каком хопе начинается потеря пакетов.

```
![Overview dashboard: Connectivity Matrix + per-protocol success rate panels]
```

Ещё то, что не относится напрямую к «поймать инцидент», но экономит нервы в проде:

- **Zone auto-discovery** (новое в v1.2.0) — раньше зону агента нужно было прописывать вручную через `agent.zone`, теперь контроллер сам резолвит зону из label ноды (`failureDomainLabel`, по умолчанию `topology.kubernetes.io/zone`) и раздаёт её агентам при регистрации. `agent.zone`, если задан, по-прежнему имеет приоритет. Смена лейбла зоны у ноды разлетается пирам сразу через full sync.
- **Self-probe prevention** — агент фильтрует себя из списка пиров по agent ID, имени ноды и pod IP, так что сам себя не пингует.
- **Config hot-reload** через fsnotify — поправили YAML, агент подхватил без рестарта.
- **Strict config validation** (тоже v1.2.0) — конфиг парсится с отклонением незнакомых полей и посчековой валидацией (интервалы/таймауты > 0 для включённых чекеров, корректный URL у HTTP-таргетов, непустой список DNS-хостов и т.д.). Опечатка в конфиге теперь роняет запуск явно, а не тихо игнорируется — при hot-reload невалидный конфиг просто не применяется, старый остаётся активным.
- **Self-monitoring** (v1.2.0) — сам мониторинг мониторится: контроллер знает, сколько нод *должно* иметь агента (`kconmon_ng_controller_expected_agents`, из числа schedulable-нод), и если зарегистрированных агентов меньше — это отдельный алерт, а не тишина.

## Установка

Из OCI-реестра, три команды:

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.2.0 \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

Проверить, что всё поднялось:

```bash
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -o wide
# Ожидается: 1 controller pod + по одному agent pod на ноду, все Running, RESTARTS 0
```

И что агенты реально отдают метрики:

```bash
AGENT=$(kubectl get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$AGENT" 8080 &
curl -s http://localhost:8080/metrics | grep '^kconmon_ng' | head -20
```

Агенту нужна capability `NET_RAW` для ICMP и MTR (чарт добавляет её сам), а сервис-аккаунту контроллера — `get/list/watch` на `nodes` (тоже настраивается чартом при `serviceAccount.create: true`).

## Метрики, алерты, дашборды

Все метрики идут с настраиваемым префиксом (`kconmon_ng` по умолчанию) и общим набором лейблов для парных проверок: `source_node`, `destination_node`, `source_zone`, `destination_zone`. Это то, что сразу даёт cross-zone видимость без дополнительной магии.

Из коробки в чарте (`prometheusRule.enabled: true`) едут пять правил алертинга:

```yaml
- alert: UDPLossHigh
  expr: kconmon_ng_udp_packet_loss_ratio > 0.5
  for: 5m
- alert: TCPChecksFailing
  expr: rate(kconmon_ng_tcp_results_total{result="fail"}[5m]) > 0
  for: 5m
- alert: DNSChecksFailing
  expr: rate(kconmon_ng_dns_results_total{result="fail"}[5m]) > 0
  for: 5m
- alert: KconmonAgentsMissing
  expr: kconmon_ng_controller_registered_agents < kconmon_ng_controller_expected_agents
  for: 10m
- alert: KconmonControllerDown
  expr: absent(kconmon_ng_controller_leader == 1)
  for: 5m
```

И три готовых дашборда для Grafana (`dashboards/*.json`, импортируются через API или UI):

- **Overview** — Connectivity Matrix, success rate и латентность по каждому протоколу (TCP/UDP/ICMP/DNS/HTTP), статус контроллера и число зарегистрированных агентов, счётчик срабатываний MTR.
- **Node Detail** — то же самое, но в разбивке по destination-ноде — удобно, когда уже понятно, какая нода «плохая», и нужно посмотреть на неё в деталях.
- **Zone Heatmap** — тепловая карта латентности и потерь между зонами для TCP/UDP/ICMP.

```
![Zone Heatmap dashboard: cross-zone latency and packet loss panels]
```

```
![Node Detail dashboard: per-destination TCP/UDP/ICMP/DNS panels for one node]
```

## Что kconmon-ng не умеет (честно)

- Никакого web UI с собственным графом связности, как у Goldpinger — только Prometheus + Grafana. Если хочется быстро визуально ткнуть в ноду прямо из встроенного интерфейса — это не сюда.
- MTU-детекция, подсказки root-cause и отдельный kubectl-плагин — в планах, но не в v1.2.0. Не обещаю их как готовую функциональность.
- Это не замена трассировке приложенческого трафика (сервис-меш телеметрия, eBPF-инструменты) — kconmon-ng работает на уровне node-to-node проб, а не перехватывает реальный прод-трафик.

## Что дальше

В роадмапе — MTU detection, подсказки по вероятной причине деградации и kubectl-плагин для быстрого просмотра топологии без похода в Grafana. Пока это только направление, не готовая функциональность — если интересно приложить руку, issues и PR в репозитории открыты.

## Ссылки

- Репозиторий: <https://github.com/EsDmitrii/kconmon-ng>
- Чарт на Artifact Hub: `oci://ghcr.io/esdmitrii/charts/kconmon-ng`
- Release notes v1.2.0: `RELEASE_NOTES.md` в репозитории
