# kconmon-ng promo publication checklist

Order of operations for publishing the three pieces in this directory. Not a
marketing campaign, just a solo maintainer sharing a real project.

Current set:

| File | Where | Language | What it is |
|---|---|---|---|
| `article-medium.md` | Medium | EN | Long-form: the pain, the measurement core, the console tour, Grafana/PromQL, quickstart |
| `article-habr.md` | Habr | RU | Long-form, incident-story voice: same arc with more commands, internals and the chaos drill |
| `post-linkedin.md` | LinkedIn | RU + EN | Short follow-up to the earlier announcement post, about the console |

The two 1.2.0-era drafts (`article-devto-draft.md`, `article-habr-draft.md`) were
deleted: they described a product with no web UI and a Goldpinger feature-comparison
table, both of which stopped being true several releases ago.

## Before publishing anything

- [x] Screenshots are the current shots, re-taken on the 10-node qa fleet, and
      every alt/caption in both articles describes what is actually in frame:
  - `docs/img/console-overview.png` (hero, both articles): 9 nodes counted from
    agents, 9 of 90 failing pairs, critical UdpPairLoss, one open incident
  - `docs/img/console-matrix.png`: UDP, the column into qa-node-07 at 16.7–17%
  - `docs/img/console-investigate.png`: qa-node-07 timeline, rollout annotation,
    five UdpPairLoss rows, Likely causes ranking nothing and saying why
  - `docs/img/console-timemachine.png`: the 10:49:03 instant, banner up, column
    at 19.5–20.5%
  - `docs/img/console-alerting.png`: the minikube shot, six managed rules SYNCED
  - `docs/img/overview.png`, `docs/img/zone-heatmap.png`, `docs/img/node-detail.png`
        (Grafana: reworked and re-shot, so the "indicative" caveat is gone from
        both articles)
- [ ] Convert image references when pasting: both articles use repo-relative
      paths (`../img/...`). Medium and Habr both need uploaded images, so the
      captions travel but the paths do not.
- [ ] Confirm the chart version in every install snippet still matches
      `charts/kconmon-ng/Chart.yaml` and `RELEASE_NOTES.md`. Both articles are
      now aligned at **2.0.0** in all three places (base install, console
      install, closing links), matching `Chart.yaml` (`appVersion: 2.0.0`), and
      the README install snippets carry the same version.
- [ ] Confirm the krew manifest URL still points at a release that exists. Both
      articles use the **v1.4.0** manifest, which is what the README uses.
- [ ] Verify every PromQL snippet against a live `/metrics` endpoint:
      `make local-up` then `make local-urls`. The histogram queries assume the
      classic `_bucket` suffix, which is what `client_golang` exports today.
- [ ] Read both articles end to end as a stranger. Cut anything that reads like
      marketing copy.
- [ ] Add a byline/bio line if you want a more personal opening than the current
      one.

## Facts that are load-bearing (re-verify if the repo moved on)

Everything below is stated in the articles as a number. Each was checked against
the repo at the time of writing:

- 5 protocols, reactive MTR on failure, 5s default interval: `README.md`
- 7 built-in alert rules, names and expressions: `docs/metrics.md`,
  `charts/kconmon-ng/templates/prometheusrule.yaml`
- 3 Grafana dashboards: `dashboards/`
- 12 console pages: `web/src/nav.ts`
- 25 permissions, 4 built-in roles: `internal/console/authz/authz.go`
- 9 timeline sources, cause weights 3/2/1/0, 300s window:
  `web/src/lib/investigation.ts`
- 2099 frontend tests in 81 files: `cd web && npx vitest run`
- 29 Go packages carrying tests, run under `-race`: `make test-race`
- Chart **2.0.0**, `appVersion: 2.0.0`: `charts/kconmon-ng/Chart.yaml`. The
  release workflow publishes the images at that tag.

## Order of publication

1. **Medium first** (English). English-speaking Kubernetes/SRE audience is the
   primary target for GitHub stars and chart adoption, and it is the piece the
   LinkedIn post links to most naturally.
2. **Habr second** (Russian). Wait a day in case Medium feedback surfaces a
   factual correction worth carrying over.
3. **LinkedIn third**, once at least one article is live, so the post can link to
   both the repo and the long read. Fill in `[link to the previous post]` with
   the original announcement URL before posting.
4. Cross-link: add a short "also published in Russian/English" line at the top of
   each article once both are live.

## Where to also mention it (lightweight, no separate write-up)

- [ ] r/kubernetes: short text post linking the Medium article, not a duplicate
      of it
- [ ] CNCF Slack, relevant SIG channels if it fits channel norms: link only
- [ ] Artifact Hub: confirm `charts/kconmon-ng/README.md` still renders
      correctly on the package page
- [ ] GitHub repo: link the published articles from the README once they exist

## After publishing

- [ ] Watch issues/discussions for the first 48h. The console will draw "does it
      do X" questions; answer with what actually ships, not with the roadmap.
- [ ] Goldpinger is mentioned as honest lineage in both articles and nowhere as a
      competitor. If its maintainers or users show up, keep that tone in replies.
- [ ] Apply any factual correction to the repo first. The repo is the source of
      truth, the published articles are downstream copies.

## Explicitly out of scope

- No paid promotion, no coordinated social push, no invented usage or adoption
  numbers.
- No benchmark claims. None have been run that are worth publishing, and both
  articles say so out loud.
- No feature-comparison table against any other product. This is what killed the
  old dev.to draft.
- No claims about anything not in the current release.
