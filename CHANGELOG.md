# Changelog

All notable changes to this project are documented in this file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0-rc.1] - RELEASE_DATE_PLACEHOLDER

### Added

- `leadscore run` brings leads in from CSV files, Google Sheet tabs and Apollo webhooks, merges them into one lead per person, and scores each with your own YAML rubric.
- Lanes route each lead to one place: an Apollo sequence, HubSpot contacts and deals, or an export list.
- Replies and opt-outs are read back, so nobody who replied or opted out gets cold outreach again, and nobody is cold-contacted twice.
- Runs as one binary for macOS, Linux and Windows, with `docker compose`, or on Google Cloud, with a SQLite or Google Sheets store and plug-in stores and sinks held to conformance suites.

[Unreleased]: https://github.com/HarshitBadhwar8/leadscore/compare/v0.1.0-rc.1...HEAD
[0.1.0-rc.1]: https://github.com/HarshitBadhwar8/leadscore/releases/tag/v0.1.0-rc.1
