# Governance

## Roles

- **Contributors** open issues and pull requests. Anyone can.
- **Maintainers** ([MAINTAINERS.md](MAINTAINERS.md)) review and merge, cut releases, and decide on
  the roadmap.

## Decisions

Day-to-day changes are decided in the pull request: one maintainer's approval merges it. Changes to
the API, the metric names, the chart's values contract or this document need lazy consensus among
the maintainers: the proposal stays open for seven days, and silence is agreement. A maintainer's
explicit objection blocks it until resolved; if the maintainers cannot agree, a simple majority
decides.

## Becoming a maintainer

A contributor with a sustained record of reviewed contributions (code, docs or reviews) over at
least three months can be nominated by any maintainer. The nomination passes by lazy consensus of
the existing maintainers. A maintainer who has been inactive for twelve months, or who asks to,
moves to emeritus.

## Releases

Maintainers tag releases from `main`. Every release runs the full CI, a signed build and the
end-to-end suite before it is published; the release notes and the chart's changelog are part of
the release commit.

## Changes to this document

By pull request and lazy consensus, as above.
