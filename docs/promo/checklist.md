# kconmon-ng promo publication checklist

Short plan for publishing the two drafts in this directory. Not a marketing campaign — just an order of operations for a solo maintainer sharing a real project.

## Before publishing anything

- [ ] Take real screenshots to replace every `TODO-screenshot` / `![...]()` placeholder in both drafts:
  - Overview dashboard: Connectivity Matrix + per-protocol success rate panels
  - Node Detail dashboard: per-destination TCP/UDP/ICMP/DNS panels for one node
  - Zone Heatmap dashboard: cross-zone latency/loss panels
  - (Optional, Habr only) a real MTR-triggered incident view if one is available, or a synthetic one from `make local-up`
- [ ] Read both drafts end to end once as if you were a stranger — cut anything that reads like marketing copy
- [ ] Confirm current chart version / image tags in both drafts still match the latest release (`Chart.yaml`, `RELEASE_NOTES.md`)
- [ ] Verify every PromQL snippet in the dev.to draft against a live `/metrics` endpoint (`make local-up` + `make local-urls`)
- [ ] Add your own byline/bio and a one-line "why I built this" if you want a more personal opening than the current draft

## Order of publication

1. **dev.to first** (English, versus-format). Lower stakes, faster feedback loop, English-speaking Kubernetes/SRE audience is the primary target for GitHub stars and Helm chart adoption.
2. **Habr second** (Russian, incident story). Wait for at least a day of dev.to feedback in case it surfaces a factual correction that should also go into the Habr draft.
3. Cross-link: once both are live, add a short "also published in Russian/English" line at the top of each with a link to the other.

## Where to also mention it (lightweight, no separate write-up needed)

- [ ] r/kubernetes — a short text post linking to the dev.to article once it's live, not a duplicate of the article
- [ ] CNCF Slack `#kubernetes-novice` / relevant SIG channels, if appropriate and not against channel norms — link only, no copy-paste of the article
- [ ] Artifact Hub listing — confirm `charts/kconmon-ng/README.md` renders correctly on the package page (already fixed in v1.2.0 per `RELEASE_NOTES.md`)
- [ ] GitHub repo — pin or link the published articles from the README or a "Press/Articles" section if one gets added later (out of scope for this task)

## After the 1.3.2 release

- [ ] Verify the krew install on a clean machine: `kubectl krew install --manifest-url https://github.com/EsDmitrii/kconmon-ng/releases/download/v1.3.2/kconmon.yaml`, then `kubectl kconmon topology` against a live cluster — confirm the release archives and sha256s in `kconmon.yaml` resolve.
- [ ] Apply for Artifact Hub "official" status once the 1.3.2 chart is indexed and the security report clears — file the request via the artifacthub/hub issue template (verified publisher + README-in-package are already satisfied).

## After publishing

- [ ] Watch GitHub issues/discussions for the first 48h — comparison articles tend to draw "what about X" questions (MTU detection, root-cause hints, kubectl plugin are known gaps, already flagged as roadmap in both drafts)
- [ ] If Goldpinger maintainers or community members comment, engage respectfully — the articles are written to be fair to Goldpinger, keep that tone in replies too
- [ ] Note any factual corrections needed and apply them to both drafts (source of truth stays the repo, not the published articles)

## Explicitly out of scope for this checklist

- No paid promotion, no coordinated social media push, no invented usage statistics or adoption numbers in any follow-up posts
- No claims about features not in the current release (MTU detection, root-cause hints, kubectl plugin stay "roadmap" until they ship)

- After the dev.to article is published: add its public URL to the README "Why not just Goldpinger?" section (the long-form "when Goldpinger is the better fit" discussion is currently not linked from the README by design — drafts must not be linked from the storefront).
