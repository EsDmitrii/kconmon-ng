# LinkedIn follow-up post: the kconmon-ng console

Follow-up to the original announcement post: [link to the previous post].

Two variants below, RU and EN. Pick one, or post the EN version and put the RU
one in the first comment. Both are written to stand alone if someone never saw
the first post.

**Images to attach** (in this order, LinkedIn shows up to 4 well in a carousel):

1. `docs/img/console-overview.png`: cluster health, worst pairs, firing alerts,
   open incident
2. `docs/img/console-matrix.png`: 10-node fleet, one node down as a red column
3. `docs/img/console-investigate.png`: timeline of a break with annotation and
   alerts
4. `docs/img/console-alerting.png`: managed rules, SYNCED against a live
   prometheus-operator

---

## EN

A while back I posted about kconmon-ng, a small tool I wrote because "the
network is fine" is not a measurement. [link to the previous post]

Back then it was agents, a controller and Grafana dashboards. Since then it grew
a console, and the project reached its last planned milestone. Here is what
actually changed:

→ **An N×N matrix of every ordered node pair.** One cell per pair per protocol,
so a partial failure reads as "this pair, this protocol" instead of disappearing
into a green aggregate.

→ **Investigate.** Nine timeline sources merged around one scope and window
(topology events, K8s events, MTR path changes, maintenance windows, threshold
crossings, firing alerts and more), with candidate causes ranked by documented
arithmetic. No ML. The weights are constants you can read, and a firing alert is
deliberately weighted zero because it restates the symptom rather than causing
it.

→ **Time Machine.** Put `?at=` on any URL and every page resolves through that
instant instead of now. Mutations are disabled while it is engaged, so you
cannot change the fleet from inside the past.

→ **Alerting that closes the loop.** Build a rule from six typed templates or
raw PromQL, preview it by running it against your actual Prometheus, then let
the console reconcile every enabled rule into one real PrometheusRule object via
prometheus-operator. The console manages, Prometheus evaluates.

→ **MTR path history.** Every traced path is content-hashed and deduped at
ingest, so what you get is a list of route *changes*, not a wall of identical
traces.

→ **Still just Prometheus underneath.** The console is a consumer, not a
replacement. Three Grafana dashboards still ship, the metric names are unchanged,
and if you delete the console tomorrow every alert keeps working.

Off by default, one Helm flag to turn on, Apache 2.0.

Repo, chart and docs: https://github.com/EsDmitrii/kconmon-ng

If you run an on-prem cluster where partial connectivity failures are a regular
evening, I would like to hear whether this catches yours.

#kubernetes #sre #devops #observability #prometheus #opensource #golang

---

## RU

Некоторое время назад я писал про kconmon-ng, инструмент, который появился из
простой мысли: «сеть в порядке» это не измерение. [link to the previous post]

Тогда там были агенты, контроллер и дашборды в Grafana. С тех пор проект оброс
консолью и дошёл до последней запланированной вехи. Что реально изменилось:

→ **Матрица N×N по всем упорядоченным парам нод.** По ячейке на пару и протокол,
поэтому частичная деградация читается как «эта пара, этот протокол», а не тонет в
зелёном агрегате.

→ **Investigate.** Девять источников таймлайна вокруг одного скоупа и окна
(события топологии, события Kubernetes, изменения MTR-путей, окна обслуживания,
пересечения порогов, горящие алерты и не только), плюс ранжирование кандидатов в
причины по документированной арифметике. Без ML: веса это константы, которые можно
прочитать, а горящему алерту сознательно дан вес ноль, потому что он пересказывает
симптом, а не вызывает его.

→ **Time Machine.** Добавьте `?at=` к любому URL, и каждая страница отвечает про
этот момент, а не про «сейчас». Пока режим включён, мутирующие действия
задизейблены: поменять флот из прошлого нельзя.

→ **Алертинг, который замыкает цикл.** Собрать правило из шести типовых шаблонов
или сырого PromQL, проверить его, запустив против вашего живого Prometheus, и
дать консоли свести все включённые правила в один настоящий объект PrometheusRule
через prometheus-operator. Консоль управляет, Prometheus вычисляет.

→ **История MTR-путей.** Каждый путь хешируется по содержимому и
дедуплицируется на входе, поэтому на выходе список *изменений* маршрута, а не
стена одинаковых трейсов.

→ **Под капотом всё тот же Prometheus.** Консоль это потребитель, а не замена.
Три дашборда для Grafana на месте, имена метрик не поменялись, снесёте консоль
завтра и все алерты продолжат работать.

По умолчанию выключено, включается одним флагом Helm, Apache 2.0.

Репозиторий, чарт и документация: https://github.com/EsDmitrii/kconmon-ng

Если у вас on-prem кластер, где частичные сетевые деградации это регулярный
вечер, мне интересно, поймает ли оно ваши.

#kubernetes #sre #devops #observability #prometheus #opensource #golang
