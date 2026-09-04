# Changelog

Fixture for scripts/release/test-changelog-section.sh. Every heading form the
extractor must accept appears once, plus the two shapes it must refuse.

## Unreleased

### Added

- Something not yet released.

## 0.3.0 — 2026-09-01

### Added

- Em-dash date form, the shape 0.1.0 uses.

### Fixed

- Blank lines around this section must be trimmed off the extracted body.

## 0.2.0 - 2026-08-01

Hyphen date form.

## 0.2.0-rc.1 (2026-07-20)

Parenthesised date form, and a prerelease carrying its own section.

## 0.1.5

Bare heading, no date at all.

## 0.1.4 — 2026-06-01

## 0.1.3 — 2026-05-01

Last section in the file; the extractor stops at end of file, not at a heading
that is not there.
